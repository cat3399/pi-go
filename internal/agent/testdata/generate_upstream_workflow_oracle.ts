import { execFileSync } from "node:child_process";
import { createRequire } from "node:module";
import { mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

type ResponseInput = {
  text: string;
  toolCall: boolean;
  inputTokens: number;
  outputTokens: number;
};

type Corpus = {
  upstreamCommit: string;
  nodeVersion: string;
  scenario: {
    name: string;
    sessionId: string;
    systemPrompt: string;
    firstPrompt: string;
    image: { mimeType: string; base64: string };
    tool: { name: string; callId: string; description: string; argument: string; result: string };
    secondPrompt: string;
    responses: ResponseInput[];
  };
  queueAbortScenario: {
    name: string;
    sessionId: string;
    systemPrompt: string;
    initialPrompt: string;
    steeringMode: "all" | "one-at-a-time";
    followUpMode: "all" | "one-at-a-time";
    recalled: { steering: string; followUp: string };
    surviving: { steering: string[]; followUp: string[] };
    abortError: string;
    responses: Array<{ text: string; inputTokens: number; outputTokens: number }>;
  };
  retryScenario: {
    name: string;
    sessionId: string;
    systemPrompt: string;
    prompt: string;
    maxRetries: number;
    baseDelayMs: number;
    retryAfterMs: number;
    failure: { message: string; httpStatus: number };
    response: { text: string; inputTokens: number; outputTokens: number };
  };
  manualCompactionScenario: {
    name: string;
    sessionId: string;
    systemPrompt: string;
    firstPrompt: string;
    secondPrompt: string;
    customInstructions: string;
    reserveTokens: number;
    keepRecentTokens: number;
    responses: Array<{ text: string; inputTokens: number; outputTokens: number }>;
  };
  overflowCompactionScenario: {
    name: string;
    sessionId: string;
    systemPrompt: string;
    firstPrompt: string;
    overflowPrompt: string;
    errorMessage: string;
    reserveTokens: number;
    keepRecentTokens: number;
    seedResponse: { text: string; inputTokens: number; outputTokens: number };
    summaryResponse: { text: string; inputTokens: number; outputTokens: number };
    recoveryResponse: { text: string; inputTokens: number; outputTokens: number };
  };
  turnSnapshotScenario: {
    name: string;
    sessionId: string;
    initialSystemPrompt: string;
    reloadedSystemPrompt: string;
    initialPrompt: string;
    postReloadPrompt: string;
    initialModel: { id: string; name: string };
    nextModel: { id: string; name: string };
    initialThinkingLevel: "low";
    nextThinkingLevel: "high";
    initialTool: { name: string; callId: string; description: string; result: string };
    nextTool: { name: string; description: string };
    responses: Array<{ text: string; toolCall: boolean; inputTokens: number; outputTokens: number }>;
  };
  treeForkScenario: {
    name: string;
    sourceSessionId: string;
    systemPrompt: string;
    rootPrompt: string;
    abandonedPrompt: string;
    branchPrompt: string;
    responses: Array<{ text: string; inputTokens: number; outputTokens: number }>;
  };
};

type EntryIDMap = Map<string, string>;

const here = dirname(fileURLToPath(import.meta.url));
const corpus = JSON.parse(readFileSync(join(here, "upstream_workflow_corpus.json"), "utf8")) as Corpus;
const upstreamRoot = resolve(process.env.PI_UPSTREAM_ROOT ?? join(here, "../../../../pi"));

function moduleURL(path: string): string {
  return pathToFileURL(path).href;
}

function normalizePathText(value: string, root: string, cwd: string): string {
  return value.split(cwd).join("<cwd>").split(root).join("<root>");
}

function normalizeUsage(usage: any): Record<string, unknown> {
  return {
    input: usage.input,
    output: usage.output,
    cacheRead: usage.cacheRead,
    cacheWrite: usage.cacheWrite,
    totalTokens: usage.totalTokens,
    cost: {
      input: usage.cost.input,
      output: usage.cost.output,
      cacheRead: usage.cost.cacheRead,
      cacheWrite: usage.cost.cacheWrite,
      total: usage.cost.total,
    },
  };
}

function normalizeContentBlock(block: any): Record<string, unknown> {
  switch (block.type) {
    case "text":
      return { type: "text", text: block.text };
    case "image":
      return { type: "image", mimeType: block.mimeType, data: block.data };
    case "thinking":
      return { type: "thinking", thinking: block.thinking };
    case "toolCall":
      return { type: "toolCall", id: block.id, name: block.name, arguments: block.arguments };
    default:
      throw new Error(`unsupported message content block ${String(block?.type)}`);
  }
}

function normalizeMessage(message: any): Record<string, unknown> {
  if (message.role === "user") {
    const content = typeof message.content === "string"
      ? [{ type: "text", text: message.content }]
      : message.content.map(normalizeContentBlock);
    return { role: "user", content };
  }
  if (message.role === "assistant") {
    const normalized: Record<string, unknown> = {
      role: "assistant",
      content: message.content.map(normalizeContentBlock),
      api: message.api,
      provider: message.provider,
      model: message.model,
      usage: normalizeUsage(message.usage),
      stopReason: message.stopReason,
    };
    if (message.errorMessage !== undefined) normalized.errorMessage = message.errorMessage;
    return normalized;
  }
  if (message.role === "toolResult") {
    const normalized: Record<string, unknown> = {
      role: "toolResult",
      toolCallId: message.toolCallId,
      toolName: message.toolName,
      content: message.content.map(normalizeContentBlock),
      isError: message.isError,
      details: message.details ?? null,
    };
    if (message.usage !== undefined) normalized.usage = normalizeUsage(message.usage);
    if (message.addedToolNames !== undefined) normalized.addedToolNames = message.addedToolNames;
    return normalized;
  }
  if (message.role === "compactionSummary") {
    return {
      role: "compactionSummary",
      summary: message.summary,
      tokensBefore: message.tokensBefore,
    };
  }
  throw new Error(`unsupported message role ${String(message?.role)}`);
}

function normalizeCompactionResult(result: any, ids: EntryIDMap): Record<string, unknown> {
  const firstKeptEntryId = ids.get(result.firstKeptEntryId);
  if (!firstKeptEntryId) throw new Error(`unknown compaction firstKeptEntryId ${String(result.firstKeptEntryId)}`);
  const normalized: Record<string, unknown> = {
    summary: result.summary,
    firstKeptEntryId,
    tokensBefore: result.tokensBefore,
  };
  if (result.estimatedTokensAfter !== undefined) normalized.estimatedTokensAfter = result.estimatedTokensAfter;
  if (result.usage !== undefined) normalized.usage = normalizeUsage(result.usage);
  if (result.details !== undefined) normalized.details = JSON.parse(JSON.stringify(result.details));
  return normalized;
}

function normalizeEntry(entry: any, ids: EntryIDMap): Record<string, unknown> {
  const base = {
    type: entry.type,
    id: ids.get(entry.id),
    parentId: entry.parentId === null ? null : ids.get(entry.parentId),
  };
  if (!base.id || (entry.parentId !== null && !base.parentId)) {
    throw new Error(`entry relationship is outside normalized log: ${JSON.stringify(entry)}`);
  }
  switch (entry.type) {
    case "model_change":
      return { ...base, provider: entry.provider, modelId: entry.modelId };
    case "thinking_level_change":
      return { ...base, thinkingLevel: entry.thinkingLevel };
    case "message":
      return { ...base, message: normalizeMessage(entry.message) };
    case "compaction": {
      const firstKeptEntryId = ids.get(entry.firstKeptEntryId);
      if (!firstKeptEntryId) throw new Error(`unknown compaction firstKeptEntryId ${String(entry.firstKeptEntryId)}`);
      const normalized: Record<string, unknown> = {
        ...base,
        summary: entry.summary,
        firstKeptEntryId,
        tokensBefore: entry.tokensBefore,
      };
      if (entry.details !== undefined) normalized.details = JSON.parse(JSON.stringify(entry.details));
      if (entry.usage !== undefined) normalized.usage = normalizeUsage(entry.usage);
      if (entry.fromHook !== undefined) normalized.fromHook = entry.fromHook;
      return normalized;
    }
    default:
      throw new Error(`unexpected entry type in first workflow: ${entry.type}`);
  }
}

function normalizeProviderEvent(event: any): Record<string, unknown> {
  const normalized: Record<string, unknown> = { type: event.type };
  if (event.contentIndex !== undefined) normalized.contentIndex = event.contentIndex;
  if (event.delta !== undefined) normalized.delta = event.delta;
  if (event.content !== undefined && typeof event.content === "string") normalized.content = event.content;
  if (event.toolCall !== undefined) normalized.toolCall = normalizeContentBlock(event.toolCall);
  if (event.reason !== undefined) normalized.reason = event.reason;
  if (event.message?.usage !== undefined) normalized.usage = normalizeUsage(event.message.usage);
  return normalized;
}

function normalizeToolResult(result: any): Record<string, unknown> {
  return {
    content: result.content.map(normalizeContentBlock),
    details: result.details ?? null,
  };
}

function normalizeEvent(event: any, ids: EntryIDMap): Record<string, unknown> {
  switch (event.type) {
    case "agent_start":
    case "turn_start":
    case "agent_settled":
      return { type: event.type };
    case "agent_end":
      return {
        type: event.type,
        messageRoles: event.messages.map((message: any) => message.role),
        willRetry: event.willRetry,
      };
    case "turn_end":
      return {
        type: event.type,
        messageRole: event.message.role,
        toolResultRoles: event.toolResults.map((message: any) => message.role),
      };
    case "message_start":
      return { type: event.type, role: event.message.role };
    case "message_update":
      return {
        type: event.type,
        role: event.message.role,
        providerEvent: normalizeProviderEvent(event.assistantMessageEvent),
      };
    case "message_end":
      return { type: event.type, message: normalizeMessage(event.message) };
    case "tool_execution_start":
      return {
        type: event.type,
        toolCallId: event.toolCallId,
        toolName: event.toolName,
        arguments: event.args,
      };
    case "tool_execution_update":
      return {
        type: event.type,
        toolCallId: event.toolCallId,
        toolName: event.toolName,
        arguments: event.args,
        partialResult: event.partialResult,
      };
    case "tool_execution_end":
      return {
        type: event.type,
        toolCallId: event.toolCallId,
        toolName: event.toolName,
        result: normalizeToolResult(event.result),
        isError: event.isError,
      };
    case "entry_appended":
      return { type: event.type, entry: normalizeEntry(event.entry, ids) };
    case "queue_update":
      return { type: event.type, steering: event.steering, followUp: event.followUp };
    case "thinking_level_changed":
      return { type: event.type, level: event.level };
    case "auto_retry_start":
      return {
        type: event.type,
        attempt: event.attempt,
        maxAttempts: event.maxAttempts,
        delayMs: event.delayMs,
        errorMessage: event.errorMessage,
      };
    case "auto_retry_end": {
      const normalized: Record<string, unknown> = {
        type: event.type,
        success: event.success,
        attempt: event.attempt,
      };
      if (event.finalError !== undefined) normalized.finalError = event.finalError;
      return normalized;
    }
    case "compaction_start":
      return { type: event.type, reason: event.reason };
    case "compaction_end": {
      const normalized: Record<string, unknown> = {
        type: event.type,
        reason: event.reason,
        aborted: event.aborted,
        willRetry: event.willRetry,
      };
      if (event.result !== undefined) normalized.result = normalizeCompactionResult(event.result, ids);
      if (event.errorMessage !== undefined) normalized.errorMessage = event.errorMessage;
      return normalized;
    }
    default:
      throw new Error(`unexpected AgentSession event in first workflow: ${event.type}`);
  }
}

function normalizeTool(tool: any): Record<string, unknown> {
  return {
    name: tool.name,
    description: tool.description,
    parameters: JSON.parse(JSON.stringify(tool.parameters)),
  };
}

function normalizeProviderInput(
  model: any,
  context: any,
  options: any,
  root: string,
  cwd: string,
  sessionId: string,
  foreignSessionIdLabel = "<summary-session-id>",
): Record<string, unknown> {
  const requestSessionId = options?.sessionId ?? "";
  return {
    model: { provider: model.provider, api: model.api, id: model.id },
    systemPrompt: normalizePathText(context.systemPrompt, root, cwd),
    messages: context.messages.map(normalizeMessage),
    tools: (context.tools ?? []).map(normalizeTool),
    stream: {
      sessionId: requestSessionId === sessionId || requestSessionId === "" ? requestSessionId : foreignSessionIdLabel,
      reasoning: options?.reasoning ?? null,
      transport: options?.transport ?? "",
    },
  };
}

function normalizeStats(stats: any): Record<string, unknown> {
  return {
    sessionFile: stats.sessionFile === undefined ? null : "<session-file>",
    sessionId: stats.sessionId,
    userMessages: stats.userMessages,
    assistantMessages: stats.assistantMessages,
    toolCalls: stats.toolCalls,
    toolResults: stats.toolResults,
    totalMessages: stats.totalMessages,
    tokens: stats.tokens,
    cost: stats.cost,
    contextUsage: stats.contextUsage ?? null,
  };
}

function normalizeHeader(header: any, root: string, cwd: string): Record<string, unknown> {
  return {
    type: header.type,
    version: header.version,
    id: header.id,
    cwd: normalizePathText(header.cwd, root, cwd),
  };
}

function queueSnapshot(session: any): Record<string, unknown> {
  return {
    steering: [...session.getSteeringMessages()],
    followUp: [...session.getFollowUpMessages()],
    pendingMessageCount: session.pendingMessageCount,
  };
}

async function runQueueAbortScenario(
  root: string,
  sdkModule: any,
  sessionModule: any,
  settingsModule: any,
  authModule: any,
  utilityModule: any,
  modelTestModule: any,
  harnessModule: any,
  eventStreamModule: any,
): Promise<Record<string, unknown>> {
  const scenario = corpus.queueAbortScenario;
  if (scenario.responses.length !== 3 || scenario.surviving.steering.length !== 2 || scenario.surviving.followUp.length !== 2) {
    throw new Error("queue/abort workflow requires two surviving messages per queue and three continuation responses");
  }

  const scenarioRoot = join(root, "queue-abort");
  const cwd = join(scenarioRoot, "project");
  const agentDir = join(scenarioRoot, "agent");
  const sessionDir = join(scenarioRoot, "sessions");
  mkdirSync(cwd, { recursive: true });
  mkdirSync(agentDir, { recursive: true });
  mkdirSync(sessionDir, { recursive: true });

  const normal = harnessModule.createFauxStreamFn(
    scenario.responses.map((response) => ({
      text: response.text,
      stopReason: "stop",
      usage: {
        input: response.inputTokens,
        output: response.outputTokens,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: response.inputTokens + response.outputTokens,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
      },
    })),
  );
  const providerContexts: any[] = [];
  const streamOptions: any[] = [];
  let resolveFirstCall!: () => void;
  const firstCallStarted = new Promise<void>((resolve) => {
    resolveFirstCall = resolve;
  });
  const model = harnessModule.fauxModel;
  const streamSimple = (streamModel: any, context: any, options: any) => {
    const callIndex = providerContexts.length;
    providerContexts.push(context);
    streamOptions.push(options);
    if (callIndex !== 0) {
      return normal.streamFn(streamModel, context, options);
    }

    const stream = eventStreamModule.createAssistantMessageEventStream();
    let finished = false;
    const abort = () => {
      if (finished) return;
      finished = true;
      const message = {
        role: "assistant",
        content: [],
        api: streamModel.api,
        provider: streamModel.provider,
        model: streamModel.id,
        usage: {
          input: 0,
          output: 0,
          cacheRead: 0,
          cacheWrite: 0,
          totalTokens: 0,
          cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
        },
        stopReason: "aborted",
        errorMessage: scenario.abortError,
        timestamp: Date.now(),
      };
      stream.push({ type: "error", reason: "aborted", error: message });
    };
    if (options?.signal?.aborted) abort();
    else options?.signal?.addEventListener("abort", abort, { once: true });
    resolveFirstCall();
    return stream;
  };

  const authStorage = authModule.AuthStorage.inMemory();
  await authStorage.modify(model.provider, async () => ({ type: "api_key", key: "faux-key" }));
  const modelRegistry = await modelTestModule.createInMemoryModelRegistry(authStorage);
  modelRegistry.registerProvider(model.provider, {
    baseUrl: model.baseUrl,
    apiKey: "faux-key",
    api: model.api,
    streamSimple,
    models: [{
      id: model.id,
      name: model.name,
      api: model.api,
      reasoning: model.reasoning,
      input: model.input,
      cost: model.cost,
      contextWindow: model.contextWindow,
      maxTokens: model.maxTokens,
      baseUrl: model.baseUrl,
    }],
  });

  const settingsManager = settingsModule.SettingsManager.inMemory({
    compaction: { enabled: false },
    retry: { enabled: false },
    transport: "sse",
    steeringMode: scenario.steeringMode,
    followUpMode: scenario.followUpMode,
  });
  const sessionManager = sessionModule.SessionManager.create(cwd, sessionDir, { id: scenario.sessionId });
  const resourceLoader = {
    ...utilityModule.createTestResourceLoader(),
    getSystemPrompt: () => scenario.systemPrompt,
  };
  const created = await sdkModule.createAgentSession({
    cwd,
    agentDir,
    model,
    thinkingLevel: "off",
    tools: [],
    customTools: [],
    resourceLoader,
    sessionManager,
    settingsManager,
    modelRuntime: modelTestModule.getModelRuntime(modelRegistry),
  });
  const session = created.session;
  const events: any[] = [];
  const settledSnapshots: Array<Record<string, unknown>> = [];
  session.subscribe((event: any) => {
    events.push(event);
    if (event.type === "agent_settled") {
      settledSnapshots.push({
        isStreaming: session.isStreaming,
        isIdle: session.isIdle,
        ...queueSnapshot(session),
      });
    }
  });

  const initialRun = session.prompt(scenario.initialPrompt);
  await firstCallStarted;
  await session.prompt(scenario.recalled.steering, { streamingBehavior: "steer" });
  await session.prompt(scenario.recalled.followUp, { streamingBehavior: "followUp" });
  const queueBeforeClear = queueSnapshot(session);
  const cleared = session.clearQueue();
  const clearResult = { steering: [...cleared.steering], followUp: [...cleared.followUp] };
  const queueAfterClear = queueSnapshot(session);

  for (const text of scenario.surviving.steering) await session.steer(text);
  for (const text of scenario.surviving.followUp) await session.followUp(text);
  const queueBeforeAbort = queueSnapshot(session);
  const abortRun = session.abort();
  await Promise.all([initialRun, abortRun]);
  const abortReturn = {
    isStreaming: session.isStreaming,
    isIdle: session.isIdle,
    settledEventCount: settledSnapshots.length,
    ...queueSnapshot(session),
  };

  if (providerContexts.length !== 4 || streamOptions.length !== 4 || normal.state.callCount !== 3) {
    throw new Error(
      `queue/abort provider calls ${providerContexts.length}/${streamOptions.length}/${normal.state.callCount}, want 4/4/3`,
    );
  }
  if (settledSnapshots.length !== 1) {
    throw new Error(`queue/abort settled events ${settledSnapshots.length}, want 1`);
  }

  const entries = sessionManager.getEntries();
  const ids: EntryIDMap = new Map(entries.map((entry: any, index: number) => [entry.id, `entry-${index + 1}`]));
  const sessionFile = session.sessionFile;
  if (!sessionFile) throw new Error("persistent queue/abort AgentSession did not publish a session file");
  const fileLines = readFileSync(sessionFile, "utf8").trimEnd().split("\n").map((line) => JSON.parse(line));
  const header = fileLines[0];
  const fileEntries = fileLines.slice(1);
  const stats = session.getSessionStats();
  const finalState = {
    isStreaming: session.isStreaming,
    pendingMessageCount: session.pendingMessageCount,
    model: { provider: session.model.provider, api: session.model.api, id: session.model.id },
    thinkingLevel: session.thinkingLevel,
    activeTools: session.getActiveToolNames(),
    systemPrompt: normalizePathText(session.systemPrompt, scenarioRoot, cwd),
    messages: session.messages.map(normalizeMessage),
    stats: normalizeStats(stats),
  };
  session.dispose();

  const reopened = sessionModule.SessionManager.open(sessionFile, sessionDir);
  const reopenedContext = reopened.buildSessionContext();
  const reopenedEntries = reopened.getEntries();
  return {
    name: scenario.name,
    input: scenario,
    actions: {
      queueBeforeClear,
      clearResult,
      queueAfterClear,
      queueBeforeAbort,
      abortReturn,
      settledSnapshots,
    },
    providerInputs: providerContexts.map((context: any, index: number) =>
      normalizeProviderInput(model, context, streamOptions[index], scenarioRoot, cwd, scenario.sessionId)),
    events: events.map((event) => normalizeEvent(event, ids)),
    finalState,
    session: {
      header: normalizeHeader(header, scenarioRoot, cwd),
      entries: entries.map((entry: any) => normalizeEntry(entry, ids)),
      fileEntries: fileEntries.map((entry: any) => normalizeEntry(entry, ids)),
      reopened: {
        header: normalizeHeader(reopened.getHeader(), scenarioRoot, cwd),
        entries: reopenedEntries.map((entry: any) => normalizeEntry(entry, ids)),
        context: {
          messages: reopenedContext.messages.map(normalizeMessage),
          model: reopenedContext.model,
          thinkingLevel: reopenedContext.thinkingLevel,
        },
      },
    },
  };
}

async function runRetryScenario(
  root: string,
  sdkModule: any,
  sessionModule: any,
  settingsModule: any,
  authModule: any,
  utilityModule: any,
  modelTestModule: any,
  harnessModule: any,
): Promise<Record<string, unknown>> {
  const scenario = corpus.retryScenario;
  const scenarioRoot = join(root, "retry");
  const cwd = join(scenarioRoot, "project");
  const agentDir = join(scenarioRoot, "agent");
  const sessionDir = join(scenarioRoot, "sessions");
  mkdirSync(cwd, { recursive: true });
  mkdirSync(agentDir, { recursive: true });
  mkdirSync(sessionDir, { recursive: true });

  const faux = harnessModule.createFauxStreamFn([
    {
      text: "",
      stopReason: "error",
      error: scenario.failure.message,
      usage: {
        input: 0,
        output: 0,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: 0,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
      },
    },
    {
      text: scenario.response.text,
      stopReason: "stop",
      usage: {
        input: scenario.response.inputTokens,
        output: scenario.response.outputTokens,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: scenario.response.inputTokens + scenario.response.outputTokens,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
      },
    },
  ]);
  const streamOptions: any[] = [];
  const streamSimple = (model: any, context: any, options: any) => {
    streamOptions.push(options);
    return faux.streamFn(model, context, options);
  };
  const model = harnessModule.fauxModel;
  const authStorage = authModule.AuthStorage.inMemory();
  await authStorage.modify(model.provider, async () => ({ type: "api_key", key: "faux-key" }));
  const modelRegistry = await modelTestModule.createInMemoryModelRegistry(authStorage);
  modelRegistry.registerProvider(model.provider, {
    baseUrl: model.baseUrl,
    apiKey: "faux-key",
    api: model.api,
    streamSimple,
    models: [{
      id: model.id,
      name: model.name,
      api: model.api,
      reasoning: model.reasoning,
      input: model.input,
      cost: model.cost,
      contextWindow: model.contextWindow,
      maxTokens: model.maxTokens,
      baseUrl: model.baseUrl,
    }],
  });

  const settingsManager = settingsModule.SettingsManager.inMemory({
    compaction: { enabled: false },
    retry: {
      enabled: true,
      maxRetries: scenario.maxRetries,
      baseDelayMs: scenario.baseDelayMs,
      provider: { maxRetries: 0, maxRetryDelayMs: scenario.retryAfterMs },
    },
    transport: "sse",
  });
  const sessionManager = sessionModule.SessionManager.create(cwd, sessionDir, { id: scenario.sessionId });
  const resourceLoader = {
    ...utilityModule.createTestResourceLoader(),
    getSystemPrompt: () => scenario.systemPrompt,
  };
  const created = await sdkModule.createAgentSession({
    cwd,
    agentDir,
    model,
    thinkingLevel: "off",
    tools: [],
    customTools: [],
    resourceLoader,
    sessionManager,
    settingsManager,
    modelRuntime: modelTestModule.getModelRuntime(modelRegistry),
  });
  const session = created.session;
  const events: any[] = [];
  const settledSnapshots: Array<Record<string, unknown>> = [];
  session.subscribe((event: any) => {
    events.push(event);
    if (event.type === "agent_settled") {
      settledSnapshots.push({
        isStreaming: session.isStreaming,
        isIdle: session.isIdle,
        isRetrying: session.isRetrying,
        ...queueSnapshot(session),
      });
    }
  });

  await session.prompt(scenario.prompt);
  const promptReturn = {
    isStreaming: session.isStreaming,
    isIdle: session.isIdle,
    isRetrying: session.isRetrying,
    settledEventCount: settledSnapshots.length,
    ...queueSnapshot(session),
  };
  if (faux.state.callCount !== 2 || streamOptions.length !== 2) {
    throw new Error(`retry provider calls ${faux.state.callCount}/${streamOptions.length}, want 2/2`);
  }
  if (settledSnapshots.length !== 1) {
    throw new Error(`retry settled events ${settledSnapshots.length}, want 1`);
  }

  const entries = sessionManager.getEntries();
  const ids: EntryIDMap = new Map(entries.map((entry: any, index: number) => [entry.id, `entry-${index + 1}`]));
  const sessionFile = session.sessionFile;
  if (!sessionFile) throw new Error("persistent retry AgentSession did not publish a session file");
  const fileLines = readFileSync(sessionFile, "utf8").trimEnd().split("\n").map((line) => JSON.parse(line));
  const header = fileLines[0];
  const fileEntries = fileLines.slice(1);
  const stats = session.getSessionStats();
  const finalState = {
    isStreaming: session.isStreaming,
    pendingMessageCount: session.pendingMessageCount,
    model: { provider: session.model.provider, api: session.model.api, id: session.model.id },
    thinkingLevel: session.thinkingLevel,
    activeTools: session.getActiveToolNames(),
    systemPrompt: normalizePathText(session.systemPrompt, scenarioRoot, cwd),
    messages: session.messages.map(normalizeMessage),
    stats: normalizeStats(stats),
  };
  session.dispose();

  const reopened = sessionModule.SessionManager.open(sessionFile, sessionDir);
  const reopenedContext = reopened.buildSessionContext();
  const reopenedEntries = reopened.getEntries();
  return {
    name: scenario.name,
    input: scenario,
    actions: { promptReturn, settledSnapshots },
    providerInputs: faux.state.contexts.map((context: any, index: number) =>
      normalizeProviderInput(model, context, streamOptions[index], scenarioRoot, cwd, scenario.sessionId)),
    events: events.map((event) => normalizeEvent(event, ids)),
    finalState,
    session: {
      header: normalizeHeader(header, scenarioRoot, cwd),
      entries: entries.map((entry: any) => normalizeEntry(entry, ids)),
      fileEntries: fileEntries.map((entry: any) => normalizeEntry(entry, ids)),
      reopened: {
        header: normalizeHeader(reopened.getHeader(), scenarioRoot, cwd),
        entries: reopenedEntries.map((entry: any) => normalizeEntry(entry, ids)),
        context: {
          messages: reopenedContext.messages.map(normalizeMessage),
          model: reopenedContext.model,
          thinkingLevel: reopenedContext.thinkingLevel,
        },
      },
    },
  };
}

async function runManualCompactionScenario(
  root: string,
  sdkModule: any,
  sessionModule: any,
  settingsModule: any,
  authModule: any,
  utilityModule: any,
  modelTestModule: any,
  harnessModule: any,
): Promise<Record<string, unknown>> {
  const scenario = corpus.manualCompactionScenario;
  if (scenario.responses.length !== 3) {
    throw new Error("manual compaction workflow requires two Agent responses and one summary response");
  }
  const scenarioRoot = join(root, "manual-compaction");
  const cwd = join(scenarioRoot, "project");
  const agentDir = join(scenarioRoot, "agent");
  const sessionDir = join(scenarioRoot, "sessions");
  mkdirSync(cwd, { recursive: true });
  mkdirSync(agentDir, { recursive: true });
  mkdirSync(sessionDir, { recursive: true });

  const faux = harnessModule.createFauxStreamFn(scenario.responses.map((response) => ({
    text: response.text,
    stopReason: "stop",
    usage: {
      input: response.inputTokens,
      output: response.outputTokens,
      cacheRead: 0,
      cacheWrite: 0,
      totalTokens: response.inputTokens + response.outputTokens,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
    },
  })));
  const streamOptions: any[] = [];
  const streamSimple = (model: any, context: any, options: any) => {
    streamOptions.push(options);
    return faux.streamFn(model, context, options);
  };
  const model = harnessModule.fauxModel;
  const authStorage = authModule.AuthStorage.inMemory();
  await authStorage.modify(model.provider, async () => ({ type: "api_key", key: "faux-key" }));
  const modelRegistry = await modelTestModule.createInMemoryModelRegistry(authStorage);
  modelRegistry.registerProvider(model.provider, {
    baseUrl: model.baseUrl,
    apiKey: "faux-key",
    api: model.api,
    streamSimple,
    models: [{
      id: model.id,
      name: model.name,
      api: model.api,
      reasoning: model.reasoning,
      input: model.input,
      cost: model.cost,
      contextWindow: model.contextWindow,
      maxTokens: model.maxTokens,
      baseUrl: model.baseUrl,
    }],
  });

  const settingsManager = settingsModule.SettingsManager.inMemory({
    compaction: {
      enabled: true,
      reserveTokens: scenario.reserveTokens,
      keepRecentTokens: scenario.keepRecentTokens,
    },
    retry: { enabled: false, provider: { maxRetries: 0 } },
    transport: "sse",
  });
  const sessionManager = sessionModule.SessionManager.create(cwd, sessionDir, { id: scenario.sessionId });
  const resourceLoader = {
    ...utilityModule.createTestResourceLoader(),
    getSystemPrompt: () => scenario.systemPrompt,
  };
  const created = await sdkModule.createAgentSession({
    cwd,
    agentDir,
    model,
    thinkingLevel: "off",
    tools: [],
    customTools: [],
    resourceLoader,
    sessionManager,
    settingsManager,
    modelRuntime: modelTestModule.getModelRuntime(modelRegistry),
  });
  const session = created.session;
  const events: any[] = [];
  session.subscribe((event: any) => events.push(event));

  await session.prompt(scenario.firstPrompt);
  await session.prompt(scenario.secondPrompt);
  const beforeCompact = {
    isStreaming: session.isStreaming,
    isIdle: session.isIdle,
    isCompacting: session.isCompacting,
    ...queueSnapshot(session),
  };
  const compactResult = await session.compact(scenario.customInstructions);
  const afterCompact = {
    isStreaming: session.isStreaming,
    isIdle: session.isIdle,
    isCompacting: session.isCompacting,
    ...queueSnapshot(session),
  };
  if (faux.state.callCount !== 3 || streamOptions.length !== 3) {
    throw new Error(`manual compaction provider calls ${faux.state.callCount}/${streamOptions.length}, want 3/3`);
  }

  const entries = sessionManager.getEntries();
  const ids: EntryIDMap = new Map(entries.map((entry: any, index: number) => [entry.id, `entry-${index + 1}`]));
  const sessionFile = session.sessionFile;
  if (!sessionFile) throw new Error("persistent manual compaction AgentSession did not publish a session file");
  const fileLines = readFileSync(sessionFile, "utf8").trimEnd().split("\n").map((line) => JSON.parse(line));
  const header = fileLines[0];
  const fileEntries = fileLines.slice(1);
  const stats = session.getSessionStats();
  const finalState = {
    isStreaming: session.isStreaming,
    pendingMessageCount: session.pendingMessageCount,
    model: { provider: session.model.provider, api: session.model.api, id: session.model.id },
    thinkingLevel: session.thinkingLevel,
    activeTools: session.getActiveToolNames(),
    systemPrompt: normalizePathText(session.systemPrompt, scenarioRoot, cwd),
    messages: session.messages.map(normalizeMessage),
    stats: normalizeStats(stats),
  };
  session.dispose();

  const reopened = sessionModule.SessionManager.open(sessionFile, sessionDir);
  const reopenedContext = reopened.buildSessionContext();
  const reopenedEntries = reopened.getEntries();
  return {
    name: scenario.name,
    input: scenario,
    actions: {
      beforeCompact,
      compactReturn: normalizeCompactionResult(compactResult, ids),
      afterCompact,
    },
    providerInputs: faux.state.contexts.map((context: any, index: number) =>
      normalizeProviderInput(model, context, streamOptions[index], scenarioRoot, cwd, scenario.sessionId)),
    events: events.map((event) => normalizeEvent(event, ids)),
    finalState,
    session: {
      header: normalizeHeader(header, scenarioRoot, cwd),
      entries: entries.map((entry: any) => normalizeEntry(entry, ids)),
      fileEntries: fileEntries.map((entry: any) => normalizeEntry(entry, ids)),
      reopened: {
        header: normalizeHeader(reopened.getHeader(), scenarioRoot, cwd),
        entries: reopenedEntries.map((entry: any) => normalizeEntry(entry, ids)),
        context: {
          messages: reopenedContext.messages.map(normalizeMessage),
          model: reopenedContext.model,
          thinkingLevel: reopenedContext.thinkingLevel,
        },
      },
    },
  };
}

async function runOverflowCompactionScenario(
  root: string,
  sdkModule: any,
  sessionModule: any,
  settingsModule: any,
  authModule: any,
  utilityModule: any,
  modelTestModule: any,
  harnessModule: any,
): Promise<Record<string, unknown>> {
  const scenario = corpus.overflowCompactionScenario;
  const scenarioRoot = join(root, "overflow-compaction");
  const cwd = join(scenarioRoot, "project");
  const agentDir = join(scenarioRoot, "agent");
  const sessionDir = join(scenarioRoot, "sessions");
  mkdirSync(cwd, { recursive: true });
  mkdirSync(agentDir, { recursive: true });
  mkdirSync(sessionDir, { recursive: true });

  const successResponse = (response: { text: string; inputTokens: number; outputTokens: number }) => ({
    text: response.text,
    stopReason: "stop",
    usage: {
      input: response.inputTokens,
      output: response.outputTokens,
      cacheRead: 0,
      cacheWrite: 0,
      totalTokens: response.inputTokens + response.outputTokens,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
    },
  });
  const faux = harnessModule.createFauxStreamFn([
    successResponse(scenario.seedResponse),
    {
      text: "",
      stopReason: "error",
      error: scenario.errorMessage,
      usage: {
        input: 0,
        output: 0,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: 0,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
      },
    },
    successResponse(scenario.summaryResponse),
    successResponse(scenario.recoveryResponse),
  ]);
  const streamOptions: any[] = [];
  const streamSimple = (model: any, context: any, options: any) => {
    streamOptions.push(options);
    return faux.streamFn(model, context, options);
  };
  const model = harnessModule.fauxModel;
  const authStorage = authModule.AuthStorage.inMemory();
  await authStorage.modify(model.provider, async () => ({ type: "api_key", key: "faux-key" }));
  const modelRegistry = await modelTestModule.createInMemoryModelRegistry(authStorage);
  modelRegistry.registerProvider(model.provider, {
    baseUrl: model.baseUrl,
    apiKey: "faux-key",
    api: model.api,
    streamSimple,
    models: [{
      id: model.id,
      name: model.name,
      api: model.api,
      reasoning: model.reasoning,
      input: model.input,
      cost: model.cost,
      contextWindow: model.contextWindow,
      maxTokens: model.maxTokens,
      baseUrl: model.baseUrl,
    }],
  });

  const settingsManager = settingsModule.SettingsManager.inMemory({
    compaction: {
      enabled: true,
      reserveTokens: scenario.reserveTokens,
      keepRecentTokens: scenario.keepRecentTokens,
    },
    retry: { enabled: false, provider: { maxRetries: 0 } },
    transport: "sse",
  });
  const sessionManager = sessionModule.SessionManager.create(cwd, sessionDir, { id: scenario.sessionId });
  const resourceLoader = {
    ...utilityModule.createTestResourceLoader(),
    getSystemPrompt: () => scenario.systemPrompt,
  };
  const created = await sdkModule.createAgentSession({
    cwd,
    agentDir,
    model,
    thinkingLevel: "off",
    tools: [],
    customTools: [],
    resourceLoader,
    sessionManager,
    settingsManager,
    modelRuntime: modelTestModule.getModelRuntime(modelRegistry),
  });
  const session = created.session;
  const events: any[] = [];
  const settledSnapshots: Array<Record<string, unknown>> = [];
  session.subscribe((event: any) => {
    events.push(event);
    if (event.type === "agent_settled") {
      settledSnapshots.push({
        isStreaming: session.isStreaming,
        isIdle: session.isIdle,
        isCompacting: session.isCompacting,
        ...queueSnapshot(session),
      });
    }
  });

  await session.prompt(scenario.firstPrompt);
  const seedReturn = {
    isStreaming: session.isStreaming,
    isIdle: session.isIdle,
    isCompacting: session.isCompacting,
    settledEventCount: settledSnapshots.length,
    ...queueSnapshot(session),
  };
  await session.prompt(scenario.overflowPrompt);
  const overflowReturn = {
    isStreaming: session.isStreaming,
    isIdle: session.isIdle,
    isCompacting: session.isCompacting,
    settledEventCount: settledSnapshots.length,
    ...queueSnapshot(session),
  };
  if (faux.state.callCount !== 4 || streamOptions.length !== 4) {
    throw new Error(`overflow compaction provider calls ${faux.state.callCount}/${streamOptions.length}, want 4/4`);
  }
  if (settledSnapshots.length !== 2) {
    throw new Error(`overflow compaction settled events ${settledSnapshots.length}, want 2`);
  }

  const entries = sessionManager.getEntries();
  const ids: EntryIDMap = new Map(entries.map((entry: any, index: number) => [entry.id, `entry-${index + 1}`]));
  const sessionFile = session.sessionFile;
  if (!sessionFile) throw new Error("persistent overflow compaction AgentSession did not publish a session file");
  const fileLines = readFileSync(sessionFile, "utf8").trimEnd().split("\n").map((line) => JSON.parse(line));
  const header = fileLines[0];
  const fileEntries = fileLines.slice(1);
  const stats = session.getSessionStats();
  const finalState = {
    isStreaming: session.isStreaming,
    pendingMessageCount: session.pendingMessageCount,
    model: { provider: session.model.provider, api: session.model.api, id: session.model.id },
    thinkingLevel: session.thinkingLevel,
    activeTools: session.getActiveToolNames(),
    systemPrompt: normalizePathText(session.systemPrompt, scenarioRoot, cwd),
    messages: session.messages.map(normalizeMessage),
    stats: normalizeStats(stats),
  };
  session.dispose();

  const reopened = sessionModule.SessionManager.open(sessionFile, sessionDir);
  const reopenedContext = reopened.buildSessionContext();
  const reopenedEntries = reopened.getEntries();
  return {
    name: scenario.name,
    input: scenario,
    actions: { seedReturn, overflowReturn, settledSnapshots },
    providerInputs: faux.state.contexts.map((context: any, index: number) =>
      normalizeProviderInput(model, context, streamOptions[index], scenarioRoot, cwd, scenario.sessionId)),
    events: events.map((event) => normalizeEvent(event, ids)),
    finalState,
    session: {
      header: normalizeHeader(header, scenarioRoot, cwd),
      entries: entries.map((entry: any) => normalizeEntry(entry, ids)),
      fileEntries: fileEntries.map((entry: any) => normalizeEntry(entry, ids)),
      reopened: {
        header: normalizeHeader(reopened.getHeader(), scenarioRoot, cwd),
        entries: reopenedEntries.map((entry: any) => normalizeEntry(entry, ids)),
        context: {
          messages: reopenedContext.messages.map(normalizeMessage),
          model: reopenedContext.model,
          thinkingLevel: reopenedContext.thinkingLevel,
        },
      },
    },
  };
}

async function runTurnSnapshotScenario(
  root: string,
  sdkModule: any,
  sessionModule: any,
  settingsModule: any,
  authModule: any,
  utilityModule: any,
  modelTestModule: any,
  harnessModule: any,
  eventStreamModule: any,
  Type: any,
): Promise<Record<string, unknown>> {
  const scenario = corpus.turnSnapshotScenario;
  if (scenario.responses.length !== 3 || !scenario.responses[0]?.toolCall) {
    throw new Error("turn snapshot workflow requires one tool-use response followed by two text responses");
  }

  const scenarioRoot = join(root, "turn-snapshot");
  const cwd = join(scenarioRoot, "project");
  const agentDir = join(scenarioRoot, "agent");
  const sessionDir = join(scenarioRoot, "sessions");
  mkdirSync(cwd, { recursive: true });
  mkdirSync(agentDir, { recursive: true });
  mkdirSync(sessionDir, { recursive: true });

  const initialModel = {
    ...harnessModule.fauxModel,
    id: scenario.initialModel.id,
    name: scenario.initialModel.name,
    reasoning: true,
  };
  const nextModel = {
    ...harnessModule.fauxModel,
    id: scenario.nextModel.id,
    name: scenario.nextModel.name,
    reasoning: true,
  };
  const faux = harnessModule.createFauxStreamFn(scenario.responses.map((response, index) => ({
    text: response.text,
    toolCalls: response.toolCall
      ? [{ id: scenario.initialTool.callId, name: scenario.initialTool.name, args: {} }]
      : undefined,
    stopReason: response.toolCall ? "toolUse" : "stop",
    model: { provider: initialModel.provider, id: index === 0 ? initialModel.id : nextModel.id },
    usage: {
      input: response.inputTokens,
      output: response.outputTokens,
      cacheRead: 0,
      cacheWrite: 0,
      totalTokens: response.inputTokens + response.outputTokens,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
    },
  })));
  const providerModels: any[] = [];
  const streamOptions: any[] = [];
  let signalFirstCallStarted!: () => void;
  const firstCallStarted = new Promise<void>((resolve) => {
    signalFirstCallStarted = resolve;
  });
  let releaseFirstCall!: () => void;
  const firstCallReleased = new Promise<void>((resolve) => {
    releaseFirstCall = resolve;
  });
  const streamSimple = (model: any, context: any, options: any) => {
    const callIndex = providerModels.length;
    providerModels.push(model);
    streamOptions.push(options);
    const source = faux.streamFn(model, context, options);
    if (callIndex !== 0) return source;

    const blocked = eventStreamModule.createAssistantMessageEventStream();
    signalFirstCallStarted();
    void (async () => {
      await firstCallReleased;
      for await (const event of source) blocked.push(event);
    })();
    return blocked;
  };

  const authStorage = authModule.AuthStorage.inMemory();
  await authStorage.modify(initialModel.provider, async () => ({ type: "api_key", key: "faux-key" }));
  const modelRegistry = await modelTestModule.createInMemoryModelRegistry(authStorage);
  modelRegistry.registerProvider(initialModel.provider, {
    baseUrl: initialModel.baseUrl,
    apiKey: "faux-key",
    api: initialModel.api,
    streamSimple,
    models: [initialModel, nextModel].map((model) => ({
      id: model.id,
      name: model.name,
      api: model.api,
      reasoning: model.reasoning,
      input: model.input,
      cost: model.cost,
      contextWindow: model.contextWindow,
      maxTokens: model.maxTokens,
      baseUrl: model.baseUrl,
    })),
  });

  const controlActions: string[] = [];
  const extensionsResult = await utilityModule.createTestExtensionsResult([
    {
      factory: (pi: any) => {
        pi.on("session_start", async (event: any) => controlActions.push(`session_start:${event.reason}`));
        pi.on("session_shutdown", async (event: any) => controlActions.push(`session_shutdown:${event.reason}`));
        pi.on("model_select", async (event: any) => {
          controlActions.push(`model_select:${event.previousModel?.id ?? "none"}->${event.model.id}:${event.source}`);
        });
        pi.on("thinking_level_select", async (event: any) => {
          controlActions.push(`thinking_level_select:${event.previousLevel}->${event.level}`);
        });
      },
      path: "<turn-snapshot-extension>",
    },
  ], cwd);
  let loadedResourceGeneration = 0;
  const resourceLoader = {
    ...utilityModule.createTestResourceLoader({ extensionsResult }),
    getSystemPrompt: () => loadedResourceGeneration === 0
      ? scenario.initialSystemPrompt
      : scenario.reloadedSystemPrompt,
    reload: async () => {
      controlActions.push("resource_reload");
      loadedResourceGeneration++;
    },
  };
  const settingsManager = settingsModule.SettingsManager.inMemory({
    compaction: { enabled: false },
    retry: { enabled: false },
    transport: "sse",
  });
  const sessionManager = sessionModule.SessionManager.create(cwd, sessionDir, { id: scenario.sessionId });
  const toolRuns: Array<Record<string, unknown>> = [];
  const initialTool = {
    name: scenario.initialTool.name,
    label: "Initial Snapshot Tool",
    description: scenario.initialTool.description,
    parameters: Type.Object({}, { additionalProperties: false }),
    execute: async (toolCallId: string) => {
      toolRuns.push({ toolCallId, toolName: scenario.initialTool.name });
      return {
        content: [{ type: "text", text: scenario.initialTool.result }],
        details: { generation: "initial" },
      };
    },
  };
  const nextTool = {
    name: scenario.nextTool.name,
    label: "Next Snapshot Tool",
    description: scenario.nextTool.description,
    parameters: Type.Object({}, { additionalProperties: false }),
    execute: async (toolCallId: string) => {
      toolRuns.push({ toolCallId, toolName: scenario.nextTool.name });
      return {
        content: [{ type: "text", text: "next snapshot tool executed" }],
        details: { generation: "next" },
      };
    },
  };

  const created = await sdkModule.createAgentSession({
    cwd,
    agentDir,
    model: initialModel,
    thinkingLevel: scenario.initialThinkingLevel,
    tools: [scenario.initialTool.name, scenario.nextTool.name],
    customTools: [initialTool, nextTool],
    resourceLoader,
    sessionManager,
    settingsManager,
    modelRuntime: modelTestModule.getModelRuntime(modelRegistry),
  });
  const session = created.session;
  session.setActiveToolsByName([scenario.initialTool.name]);
  const events: any[] = [];
  const settledSnapshots: Array<Record<string, unknown>> = [];
  session.subscribe((event: any) => {
    events.push(event);
    if (event.type === "agent_settled") {
      settledSnapshots.push({
        isStreaming: session.isStreaming,
        isIdle: session.isIdle,
        model: session.model.id,
        thinkingLevel: session.thinkingLevel,
        activeTools: session.getActiveToolNames(),
        systemPrompt: normalizePathText(session.systemPrompt, scenarioRoot, cwd),
      });
    }
  });
  await session.bindExtensions({ shutdownHandler: () => {} });

  const firstRun = session.prompt(scenario.initialPrompt);
  await firstCallStarted;
  await session.setModel(nextModel);
  session.setThinkingLevel(scenario.nextThinkingLevel);
  session.setActiveToolsByName([scenario.nextTool.name]);
  const duringFirstRequest = {
    isStreaming: session.isStreaming,
    isIdle: session.isIdle,
    model: session.model.id,
    thinkingLevel: session.thinkingLevel,
    activeTools: session.getActiveToolNames(),
    systemPrompt: normalizePathText(session.systemPrompt, scenarioRoot, cwd),
  };
  releaseFirstCall();
  await firstRun;
  const afterFirstRun = {
    isStreaming: session.isStreaming,
    isIdle: session.isIdle,
    model: session.model.id,
    thinkingLevel: session.thinkingLevel,
    activeTools: session.getActiveToolNames(),
    systemPrompt: normalizePathText(session.systemPrompt, scenarioRoot, cwd),
  };

  session.setActiveToolsByName([scenario.nextTool.name, scenario.initialTool.name]);
  const beforeReload = {
    isStreaming: session.isStreaming,
    isIdle: session.isIdle,
    model: session.model.id,
    thinkingLevel: session.thinkingLevel,
    activeTools: session.getActiveToolNames(),
    systemPrompt: normalizePathText(session.systemPrompt, scenarioRoot, cwd),
  };

  await session.reload({
    beforeSessionStart: async () => {
      controlActions.push("before_session_start");
    },
  });
  const afterReload = {
    isStreaming: session.isStreaming,
    isIdle: session.isIdle,
    model: session.model.id,
    thinkingLevel: session.thinkingLevel,
    activeTools: session.getActiveToolNames(),
    systemPrompt: normalizePathText(session.systemPrompt, scenarioRoot, cwd),
  };
  await session.prompt(scenario.postReloadPrompt);
  const promptReturn = {
    isStreaming: session.isStreaming,
    isIdle: session.isIdle,
    settledEventCount: settledSnapshots.length,
  };

  if (faux.state.callCount !== 3 || streamOptions.length !== 3 || providerModels.length !== 3) {
    throw new Error(
      `turn snapshot provider calls ${faux.state.callCount}/${streamOptions.length}/${providerModels.length}, want 3/3/3`,
    );
  }
  if (toolRuns.length !== 1 || toolRuns[0]?.toolName !== scenario.initialTool.name) {
    throw new Error(`turn snapshot tool runs ${JSON.stringify(toolRuns)}, want the initial snapshotted tool once`);
  }
  if (settledSnapshots.length !== 2) {
    throw new Error(`turn snapshot settled events ${settledSnapshots.length}, want 2`);
  }

  const entries = sessionManager.getEntries();
  const ids: EntryIDMap = new Map(entries.map((entry: any, index: number) => [entry.id, `entry-${index + 1}`]));
  const sessionFile = session.sessionFile;
  if (!sessionFile) throw new Error("persistent turn snapshot AgentSession did not publish a session file");
  const fileLines = readFileSync(sessionFile, "utf8").trimEnd().split("\n").map((line) => JSON.parse(line));
  const header = fileLines[0];
  const fileEntries = fileLines.slice(1);
  const stats = session.getSessionStats();
  const finalState = {
    isStreaming: session.isStreaming,
    pendingMessageCount: session.pendingMessageCount,
    model: { provider: session.model.provider, api: session.model.api, id: session.model.id },
    thinkingLevel: session.thinkingLevel,
    activeTools: session.getActiveToolNames(),
    systemPrompt: normalizePathText(session.systemPrompt, scenarioRoot, cwd),
    messages: session.messages.map(normalizeMessage),
    stats: normalizeStats(stats),
  };
  session.dispose();

  const reopened = sessionModule.SessionManager.open(sessionFile, sessionDir);
  const reopenedContext = reopened.buildSessionContext();
  const reopenedEntries = reopened.getEntries();
  return {
    name: scenario.name,
    input: scenario,
    actions: {
      duringFirstRequest,
      afterFirstRun,
      beforeReload,
      afterReload,
      promptReturn,
      settledSnapshots,
      controlActions,
      loadedResourceGeneration,
    },
    providerInputs: faux.state.contexts.map((context: any, index: number) =>
      normalizeProviderInput(providerModels[index], context, streamOptions[index], scenarioRoot, cwd, scenario.sessionId)),
    toolRuns,
    events: events.map((event) => normalizeEvent(event, ids)),
    finalState,
    session: {
      header: normalizeHeader(header, scenarioRoot, cwd),
      entries: entries.map((entry: any) => normalizeEntry(entry, ids)),
      fileEntries: fileEntries.map((entry: any) => normalizeEntry(entry, ids)),
      reopened: {
        header: normalizeHeader(reopened.getHeader(), scenarioRoot, cwd),
        entries: reopenedEntries.map((entry: any) => normalizeEntry(entry, ids)),
        context: {
          messages: reopenedContext.messages.map(normalizeMessage),
          model: reopenedContext.model,
          thinkingLevel: reopenedContext.thinkingLevel,
        },
      },
    },
  };
}

function addEntryIDs(entries: any[], ids: EntryIDMap): void {
  for (const entry of entries) {
    if (!ids.has(entry.id)) ids.set(entry.id, `entry-${ids.size + 1}`);
  }
}

function normalizeSessionTree(nodes: any[], ids: EntryIDMap): Array<Record<string, unknown>> {
  return nodes.map((node) => ({
    entry: normalizeEntry(node.entry, ids),
    label: node.label ?? null,
    children: normalizeSessionTree(node.children, ids),
  }));
}

function normalizeTreeForkHeader(
  header: any,
  root: string,
  cwd: string,
  forkSessionId: string,
  sourceSessionFile: string,
): Record<string, unknown> {
  const normalized: Record<string, unknown> = {
    type: header.type,
    version: header.version,
    id: header.id === forkSessionId ? "<fork-session-id>" : header.id,
    cwd: normalizePathText(header.cwd, root, cwd),
  };
  if (header.parentSession !== undefined) {
    normalized.parentSession = header.parentSession === sourceSessionFile
      ? "<source-session-file>"
      : normalizePathText(header.parentSession, root, cwd);
  }
  return normalized;
}

function normalizeTreeForkContext(context: any): Record<string, unknown> {
  return {
    messages: context.messages.map(normalizeMessage),
    model: context.model,
    thinkingLevel: context.thinkingLevel,
  };
}

async function runTreeForkScenario(
  root: string,
  runtimeModule: any,
  sessionModule: any,
  settingsModule: any,
  authModule: any,
  modelTestModule: any,
  harnessModule: any,
): Promise<Record<string, unknown>> {
  const scenario = corpus.treeForkScenario;
  if (scenario.responses.length !== 4) {
    throw new Error("tree/fork workflow requires four provider responses");
  }

  const scenarioRoot = join(root, "tree-fork");
  const cwd = join(scenarioRoot, "project");
  const agentDir = join(scenarioRoot, "agent");
  const sessionDir = join(scenarioRoot, "sessions");
  mkdirSync(cwd, { recursive: true });
  mkdirSync(agentDir, { recursive: true });
  mkdirSync(sessionDir, { recursive: true });

  const faux = harnessModule.createFauxStreamFn(scenario.responses.map((response) => ({
    text: response.text,
    stopReason: "stop",
    usage: {
      input: response.inputTokens,
      output: response.outputTokens,
      cacheRead: 0,
      cacheWrite: 0,
      totalTokens: response.inputTokens + response.outputTokens,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
    },
  })));
  const providerModels: any[] = [];
  const streamOptions: any[] = [];
  const streamSimple = (model: any, context: any, options: any) => {
    providerModels.push(model);
    streamOptions.push(options);
    return faux.streamFn(model, context, options);
  };
  const model = harnessModule.fauxModel;
  const authStorage = authModule.AuthStorage.inMemory();
  await authStorage.modify(model.provider, async () => ({ type: "api_key", key: "faux-key" }));
  const modelRegistry = await modelTestModule.createInMemoryModelRegistry(authStorage);
  modelRegistry.registerProvider(model.provider, {
    baseUrl: model.baseUrl,
    apiKey: "faux-key",
    api: model.api,
    streamSimple,
    models: [{
      id: model.id,
      name: model.name,
      api: model.api,
      reasoning: model.reasoning,
      input: model.input,
      cost: model.cost,
      contextWindow: model.contextWindow,
      maxTokens: model.maxTokens,
      baseUrl: model.baseUrl,
    }],
  });
  const modelRuntime = modelTestModule.getModelRuntime(modelRegistry);
  const settingsManager = settingsModule.SettingsManager.inMemory({
    compaction: { enabled: false },
    retry: { enabled: false },
    transport: "sse",
  });
  const createRuntime = async ({ cwd: runtimeCwd, sessionManager, sessionStartEvent }: any) => {
    const services = await runtimeModule.createAgentSessionServices({
      cwd: runtimeCwd,
      agentDir,
      modelRuntime,
      settingsManager,
      resourceLoaderOptions: {
        noExtensions: true,
        noSkills: true,
        noPromptTemplates: true,
        noThemes: true,
        noContextFiles: true,
        systemPrompt: scenario.systemPrompt,
      },
    });
    return {
      ...(await runtimeModule.createAgentSessionFromServices({
        services,
        sessionManager,
        sessionStartEvent,
        model,
        thinkingLevel: "off",
        tools: [],
        customTools: [],
      })),
      services,
      diagnostics: services.diagnostics,
    };
  };
  const sourceManager = sessionModule.SessionManager.create(cwd, sessionDir, { id: scenario.sourceSessionId });
  const runtimeHost = await runtimeModule.createAgentSessionRuntime(createRuntime, {
    cwd,
    agentDir,
    sessionManager: sourceManager,
  });
  const sourceSession = runtimeHost.session;
  const sourceEvents: any[] = [];
  sourceSession.subscribe((event: any) => sourceEvents.push(event));

  await sourceSession.prompt(scenario.rootPrompt);
  await sourceSession.prompt(scenario.abandonedPrompt);
  const usersBeforeNavigation = sourceSession.sessionManager.getEntries().filter(
    (entry: any) => entry.type === "message" && entry.message.role === "user",
  );
  if (usersBeforeNavigation.length !== 2) {
    throw new Error(`tree/fork source user entries ${usersBeforeNavigation.length}, want 2`);
  }
  const abandonedUserEntry = usersBeforeNavigation[1];
  const sourceBeforeNavigation = {
    leafId: sourceSession.sessionManager.getLeafId(),
    context: sourceSession.sessionManager.buildSessionContext(),
  };
  const navigation = await sourceSession.navigateTree(abandonedUserEntry.id);
  const sourceAfterNavigation = {
    leafId: sourceSession.sessionManager.getLeafId(),
    context: sourceSession.sessionManager.buildSessionContext(),
  };
  await sourceSession.prompt(scenario.branchPrompt);
  const branchUsers = sourceSession.sessionManager.getEntries().filter(
    (entry: any) => entry.type === "message" && entry.message.role === "user",
  );
  const branchUserEntry = branchUsers.find((entry: any) => {
    const content = entry.message.content;
    return typeof content === "string"
      ? content === scenario.branchPrompt
      : content.some((part: any) => part.type === "text" && part.text === scenario.branchPrompt);
  });
  if (!branchUserEntry) throw new Error("tree/fork replacement branch user entry was not persisted");

  const sourceSessionFile = sourceSession.sessionFile;
  if (!sourceSessionFile) throw new Error("persistent tree/fork source did not publish a session file");
  const sourceEntries = sourceSession.sessionManager.getEntries();
  const sourceHeader = sourceSession.sessionManager.getHeader();
  if (!sourceHeader) throw new Error("tree/fork source header is missing");
  const sourceLeafId = sourceSession.sessionManager.getLeafId();
  const sourceTree = sourceSession.sessionManager.getTree();
  const sourceContext = sourceSession.sessionManager.buildSessionContext();
  const sourceStats = sourceSession.getSessionStats();

  const fork = await runtimeHost.fork(branchUserEntry.id);
  const forkSession = runtimeHost.session;
  const replacedSession = forkSession !== sourceSession;
  const forkSessionFile = forkSession.sessionFile;
  if (!forkSessionFile) throw new Error("persistent tree/fork replacement did not publish a session path");
  const forkSessionId = forkSession.sessionManager.getSessionId();
  const forkEvents: any[] = [];
  forkSession.subscribe((event: any) => forkEvents.push(event));
  if (fork.selectedText !== scenario.branchPrompt) {
    throw new Error(`tree/fork selected text ${String(fork.selectedText)}, want ${scenario.branchPrompt}`);
  }
  await forkSession.prompt(fork.selectedText);
  if (faux.state.callCount !== 4 || providerModels.length !== 4 || streamOptions.length !== 4) {
    throw new Error(
      `tree/fork provider calls ${faux.state.callCount}/${providerModels.length}/${streamOptions.length}, want 4/4/4`,
    );
  }

  const forkEntries = forkSession.sessionManager.getEntries();
  const ids: EntryIDMap = new Map();
  addEntryIDs(sourceEntries, ids);
  addEntryIDs(forkEntries, ids);

  const sourceFileLines = readFileSync(sourceSessionFile, "utf8").trimEnd().split("\n").map((line) => JSON.parse(line));
  const forkFileLines = readFileSync(forkSessionFile, "utf8").trimEnd().split("\n").map((line) => JSON.parse(line));
  const forkHeader = forkSession.sessionManager.getHeader();
  if (!forkHeader) throw new Error("tree/fork replacement header is missing");
  const forkLeafId = forkSession.sessionManager.getLeafId();
  const forkTree = forkSession.sessionManager.getTree();
  const forkContext = forkSession.sessionManager.buildSessionContext();
  const forkStats = forkSession.getSessionStats();
  const normalizedForkStats = normalizeStats(forkStats);
  normalizedForkStats.sessionId = "<fork-session-id>";
  const finalState = {
    isStreaming: forkSession.isStreaming,
    pendingMessageCount: forkSession.pendingMessageCount,
    model: { provider: forkSession.model.provider, api: forkSession.model.api, id: forkSession.model.id },
    thinkingLevel: forkSession.thinkingLevel,
    activeTools: forkSession.getActiveToolNames(),
    systemPrompt: normalizePathText(forkSession.systemPrompt, scenarioRoot, cwd),
    messages: forkSession.messages.map(normalizeMessage),
    stats: normalizedForkStats,
  };
  await runtimeHost.dispose();

  const reopenedSource = sessionModule.SessionManager.open(sourceSessionFile, sessionDir);
  const reopenedFork = sessionModule.SessionManager.open(forkSessionFile, sessionDir);
  const normalizeProjection = (manager: any, header: any, entries: any[], fileLines: any[]) => ({
    header: normalizeTreeForkHeader(header, scenarioRoot, cwd, forkSessionId, sourceSessionFile),
    leafId: ids.get(manager.getLeafId()) ?? null,
    entries: entries.map((entry: any) => normalizeEntry(entry, ids)),
    fileEntries: fileLines.slice(1).map((entry: any) => normalizeEntry(entry, ids)),
    tree: normalizeSessionTree(manager.getTree(), ids),
    context: normalizeTreeForkContext(manager.buildSessionContext()),
  });
  const normalizePoint = (point: { leafId: string | null; context: any }) => ({
    leafId: point.leafId === null ? null : ids.get(point.leafId),
    context: normalizeTreeForkContext(point.context),
  });
  return {
    name: scenario.name,
    input: scenario,
    actions: {
      navigation: {
        cancelled: navigation.cancelled,
        editorText: navigation.editorText,
        targetId: ids.get(abandonedUserEntry.id),
      },
      fork: {
        cancelled: fork.cancelled,
        selectedText: fork.selectedText,
        targetId: ids.get(branchUserEntry.id),
        replacedSession,
        sourceSessionFile: "<source-session-file>",
        forkSessionFile: "<fork-session-file>",
        forkSessionId: "<fork-session-id>",
      },
      sourceBeforeNavigation: normalizePoint(sourceBeforeNavigation),
      sourceAfterNavigation: normalizePoint(sourceAfterNavigation),
    },
    providerInputs: faux.state.contexts.map((context: any, index: number) =>
      normalizeProviderInput(
        providerModels[index],
        context,
        streamOptions[index],
        scenarioRoot,
        cwd,
        scenario.sourceSessionId,
        "<fork-session-id>",
      )),
    sourceEvents: sourceEvents.map((event) => normalizeEvent(event, ids)),
    forkEvents: forkEvents.map((event) => normalizeEvent(event, ids)),
    finalState,
    source: {
      ...normalizeProjection(
        {
          getLeafId: () => sourceLeafId,
          getTree: () => sourceTree,
          buildSessionContext: () => sourceContext,
        },
        sourceHeader,
        sourceEntries,
        sourceFileLines,
      ),
      stats: normalizeStats(sourceStats),
      reopened: normalizeProjection(
        reopenedSource,
        reopenedSource.getHeader(),
        reopenedSource.getEntries(),
        sourceFileLines,
      ),
    },
    fork: {
      ...normalizeProjection(
        {
          getLeafId: () => forkLeafId,
          getTree: () => forkTree,
          buildSessionContext: () => forkContext,
        },
        forkHeader,
        forkEntries,
        forkFileLines,
      ),
      reopened: normalizeProjection(
        reopenedFork,
        reopenedFork.getHeader(),
        reopenedFork.getEntries(),
        forkFileLines,
      ),
    },
  };
}

async function main(): Promise<void> {
  const commit = execFileSync("git", ["-C", upstreamRoot, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
  if (commit !== corpus.upstreamCommit) throw new Error(`upstream commit ${commit}, want ${corpus.upstreamCommit}`);
  if (process.version !== corpus.nodeVersion) throw new Error(`Node ${process.version}, want ${corpus.nodeVersion}`);
  if (corpus.scenario.responses.length !== 3 || !corpus.scenario.responses[0]?.toolCall) {
    throw new Error("first workflow requires one tool-use response followed by two text responses");
  }

  const root = mkdtempSync(join(tmpdir(), "pi-go-agent-workflow-oracle-"));
  const cwd = join(root, "project");
  const agentDir = join(root, "agent");
  const sessionDir = join(root, "sessions");
  mkdirSync(cwd, { recursive: true });
  mkdirSync(agentDir, { recursive: true });
  mkdirSync(sessionDir, { recursive: true });

  const originalRandom = Math.random;
  Math.random = () => 0;
  try {
    const sdkModule = await import(moduleURL(join(upstreamRoot, "packages/coding-agent/src/core/sdk.ts")));
    const runtimeModule = await import(
      moduleURL(join(upstreamRoot, "packages/coding-agent/src/core/agent-session-runtime.ts"))
    );
    const sessionModule = await import(moduleURL(join(upstreamRoot, "packages/coding-agent/src/core/session-manager.ts")));
    const settingsModule = await import(moduleURL(join(upstreamRoot, "packages/coding-agent/src/core/settings-manager.ts")));
    const authModule = await import(moduleURL(join(upstreamRoot, "packages/coding-agent/src/core/auth-storage.ts")));
    const utilityModule = await import(moduleURL(join(upstreamRoot, "packages/coding-agent/test/utilities.ts")));
    const modelTestModule = await import(moduleURL(join(upstreamRoot, "packages/coding-agent/test/model-runtime-test-utils.ts")));
    const harnessModule = await import(moduleURL(join(upstreamRoot, "packages/coding-agent/test/test-harness.ts")));
    const eventStreamModule = await import(moduleURL(join(upstreamRoot, "packages/ai/src/utils/event-stream.ts")));
    const upstreamRequire = createRequire(join(upstreamRoot, "package.json"));
    const { Type } = upstreamRequire("typebox");

    const responses = corpus.scenario.responses.map((response) => ({
      text: response.text,
      toolCalls: response.toolCall
        ? [{ id: corpus.scenario.tool.callId, name: corpus.scenario.tool.name, args: { text: corpus.scenario.tool.argument } }]
        : undefined,
      stopReason: response.toolCall ? "toolUse" : "stop",
      usage: {
        input: response.inputTokens,
        output: response.outputTokens,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: response.inputTokens + response.outputTokens,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
      },
    }));
    const faux = harnessModule.createFauxStreamFn(responses);
    const streamOptions: any[] = [];
    const streamSimple = (model: any, context: any, options: any) => {
      streamOptions.push(options);
      return faux.streamFn(model, context, options);
    };
    const model = harnessModule.fauxModel;
    const authStorage = authModule.AuthStorage.inMemory();
    await authStorage.modify(model.provider, async () => ({ type: "api_key", key: "faux-key" }));
    const modelRegistry = await modelTestModule.createInMemoryModelRegistry(authStorage);
    modelRegistry.registerProvider(model.provider, {
      baseUrl: model.baseUrl,
      apiKey: "faux-key",
      api: model.api,
      streamSimple,
      models: [{
        id: model.id,
        name: model.name,
        api: model.api,
        reasoning: model.reasoning,
        input: model.input,
        cost: model.cost,
        contextWindow: model.contextWindow,
        maxTokens: model.maxTokens,
        baseUrl: model.baseUrl,
      }],
    });

    const settingsManager = settingsModule.SettingsManager.inMemory({
      compaction: { enabled: false },
      retry: { enabled: false },
      transport: "sse",
    });
    const sessionManager = sessionModule.SessionManager.create(cwd, sessionDir, { id: corpus.scenario.sessionId });
    const resourceLoader = {
      ...utilityModule.createTestResourceLoader(),
      getSystemPrompt: () => corpus.scenario.systemPrompt,
    };
    const toolRuns: Array<Record<string, unknown>> = [];
    const echoTool = {
      name: corpus.scenario.tool.name,
      label: "Echo",
      description: corpus.scenario.tool.description,
      parameters: Type.Object(
        { text: Type.String() },
        { additionalProperties: false },
      ),
      execute: async (toolCallId: string, params: { text: string }) => {
        toolRuns.push({ toolCallId, arguments: { text: params.text } });
        return {
          content: [{ type: "text", text: corpus.scenario.tool.result }],
          details: { text: params.text },
        };
      },
    };

    const created = await sdkModule.createAgentSession({
      cwd,
      agentDir,
      model,
      thinkingLevel: "off",
      tools: [corpus.scenario.tool.name],
      customTools: [echoTool],
      resourceLoader,
      sessionManager,
      settingsManager,
      modelRuntime: modelTestModule.getModelRuntime(modelRegistry),
    });
    const session = created.session;
    const events: any[] = [];
    session.subscribe((event: any) => events.push(event));

    await session.prompt(corpus.scenario.firstPrompt, {
      images: [{ type: "image", mimeType: corpus.scenario.image.mimeType, data: corpus.scenario.image.base64 }],
    });
    await session.prompt(corpus.scenario.secondPrompt);
    if (faux.state.callCount !== 3 || streamOptions.length !== 3) {
      throw new Error(`provider calls ${faux.state.callCount}/${streamOptions.length}, want 3`);
    }

    const entries = sessionManager.getEntries();
    const ids: EntryIDMap = new Map(entries.map((entry: any, index: number) => [entry.id, `entry-${index + 1}`]));
    const sessionFile = session.sessionFile;
    if (!sessionFile) throw new Error("persistent AgentSession did not publish a session file");
    const fileLines = readFileSync(sessionFile, "utf8").trimEnd().split("\n").map((line) => JSON.parse(line));
    const header = fileLines[0];
    const fileEntries = fileLines.slice(1);
    const stats = session.getSessionStats();
    const finalState = {
      isStreaming: session.isStreaming,
      pendingMessageCount: session.pendingMessageCount,
      model: { provider: session.model.provider, api: session.model.api, id: session.model.id },
      thinkingLevel: session.thinkingLevel,
      activeTools: session.getActiveToolNames(),
      systemPrompt: normalizePathText(session.systemPrompt, root, cwd),
      messages: session.messages.map(normalizeMessage),
      stats: normalizeStats(stats),
    };
    session.dispose();

    const reopened = sessionModule.SessionManager.open(sessionFile, sessionDir);
    const reopenedContext = reopened.buildSessionContext();
    const reopenedEntries = reopened.getEntries();

    const queueAbortScenario = await runQueueAbortScenario(
      root,
      sdkModule,
      sessionModule,
      settingsModule,
      authModule,
      utilityModule,
      modelTestModule,
      harnessModule,
      eventStreamModule,
    );
    const retryScenario = await runRetryScenario(
      root,
      sdkModule,
      sessionModule,
      settingsModule,
      authModule,
      utilityModule,
      modelTestModule,
      harnessModule,
    );
    const manualCompactionScenario = await runManualCompactionScenario(
      root,
      sdkModule,
      sessionModule,
      settingsModule,
      authModule,
      utilityModule,
      modelTestModule,
      harnessModule,
    );
    const overflowCompactionScenario = await runOverflowCompactionScenario(
      root,
      sdkModule,
      sessionModule,
      settingsModule,
      authModule,
      utilityModule,
      modelTestModule,
      harnessModule,
    );
    const turnSnapshotScenario = await runTurnSnapshotScenario(
      root,
      sdkModule,
      sessionModule,
      settingsModule,
      authModule,
      utilityModule,
      modelTestModule,
      harnessModule,
      eventStreamModule,
      Type,
    );
    const treeForkScenario = await runTreeForkScenario(
      root,
      runtimeModule,
      sessionModule,
      settingsModule,
      authModule,
      modelTestModule,
      harnessModule,
    );
    const output = {
      upstreamCommit: corpus.upstreamCommit,
      generatedBy: "pinned packages/coding-agent createAgentSession with deterministic stream/tool inputs",
      generator: { nodeVersion: process.version, corpus: "upstream_workflow_corpus.json" },
      scenario: {
        name: corpus.scenario.name,
        input: corpus.scenario,
        providerInputs: faux.state.contexts.map((context: any, index: number) =>
          normalizeProviderInput(model, context, streamOptions[index], root, cwd, corpus.scenario.sessionId)),
        toolRuns,
        events: events.map((event) => normalizeEvent(event, ids)),
        finalState,
        session: {
          header: normalizeHeader(header, root, cwd),
          entries: entries.map((entry: any) => normalizeEntry(entry, ids)),
          fileEntries: fileEntries.map((entry: any) => normalizeEntry(entry, ids)),
          reopened: {
            header: normalizeHeader(reopened.getHeader(), root, cwd),
            entries: reopenedEntries.map((entry: any) => normalizeEntry(entry, ids)),
            context: {
              messages: reopenedContext.messages.map(normalizeMessage),
              model: reopenedContext.model,
              thinkingLevel: reopenedContext.thinkingLevel,
            },
          },
        },
      },
      queueAbortScenario,
      retryScenario,
      manualCompactionScenario,
      overflowCompactionScenario,
      turnSnapshotScenario,
      treeForkScenario,
    };
    process.stdout.write(`${JSON.stringify(output, null, 2)}\n`);
  } finally {
    Math.random = originalRandom;
    rmSync(root, { recursive: true, force: true });
  }
}

void main().catch((error) => {
  process.stderr.write(`${error instanceof Error ? error.stack ?? error.message : String(error)}\n`);
  process.exitCode = 1;
});
