import { execFileSync } from "node:child_process";
import { createRequire } from "node:module";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

type ResponseInput = {
  text: string;
  toolCall: boolean;
  inputTokens: number;
  outputTokens: number;
};

type ThinkingLevelInput = "off" | "minimal" | "low" | "medium" | "high" | "xhigh" | "max";

type ModelControlInput = {
  id: string;
  name: string;
  reasoning: boolean;
  thinkingLevelMap?: Partial<Record<ThinkingLevelInput, string | null>>;
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
  modelControlScenario: {
    name: string;
    sessionId: string;
    systemPrompt: string;
    initialThinkingLevel: ThinkingLevelInput;
    clampRequest: ThinkingLevelInput;
    models: [ModelControlInput, ModelControlInput, ModelControlInput];
    scopedThinkingLevel: ThinkingLevelInput;
    steeringMode: "all" | "one-at-a-time";
    followUpMode: "all" | "one-at-a-time";
    prompt: string;
    response: { text: string; inputTokens: number; outputTokens: number };
  };
  retryAbortScenario: {
    name: string;
    sessionId: string;
    systemPrompt: string;
    firstPrompt: string;
    secondPrompt: string;
    maxRetries: number;
    baseDelayMs: number;
    failure: { message: string; httpStatus: number };
    response: { text: string; inputTokens: number; outputTokens: number };
  };
  runtimeReplacementScenario: {
    name: string;
    sourceSessionId: string;
    systemPrompt: string;
    initialPrompt: string;
    newPrompt: string;
    resumePrompt: string;
    importPrompt: string;
    abortError: string;
    responses: Array<{ text: string; inputTokens: number; outputTokens: number }>;
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
  damagedSessionScenario: {
    name: string;
    sessionId: string;
    systemPrompt: string;
    rootPrompt: string;
    rootResponse: { text: string; inputTokens: number; outputTokens: number };
    malformedLine: string;
    orphanPrompt: string;
    continuationPrompt: string;
    response: { text: string; inputTokens: number; outputTokens: number };
  };
  requestAssemblyScenario: {
    name: string;
    sessionId: string;
    systemPrompt: string;
    thinkingLevel: "high";
    thinkingBudgets: { minimal: number; low: number; medium: number; high: number };
    image: { mimeType: string; base64: string };
    skill: { name: string; description: string; body: string; argument: string };
    template: { name: string; content: string; argument: string };
    tool: { name: string; callId: string; description: string; argument: string; resultText: string };
    responses: Array<{ text: string; toolCall: boolean; inputTokens: number; outputTokens: number }>;
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

function normalizePathStrings(value: unknown, root: string, cwd: string): unknown {
  if (typeof value === "string") return normalizePathText(value, root, cwd);
  if (Array.isArray(value)) return value.map((item) => normalizePathStrings(item, root, cwd));
  if (value !== null && typeof value === "object") {
    return Object.fromEntries(
      Object.entries(value).map(([key, item]) => [key, normalizePathStrings(item, root, cwd)]),
    );
  }
  return value;
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
  includeThinkingBudgets = false,
): Record<string, unknown> {
  const requestSessionId = options?.sessionId ?? "";
  const stream: Record<string, unknown> = {
    sessionId: requestSessionId === sessionId || requestSessionId === "" ? requestSessionId : foreignSessionIdLabel,
    reasoning: options?.reasoning ?? null,
    transport: options?.transport ?? "",
  };
  if (includeThinkingBudgets) stream.thinkingBudgets = options?.thinkingBudgets ?? null;
  return {
    model: { provider: model.provider, api: model.api, id: model.id },
    systemPrompt: normalizePathText(context.systemPrompt, root, cwd),
    messages: context.messages.map(normalizeMessage),
    tools: (context.tools ?? []).map(normalizeTool),
    stream,
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

function normalizeModelCycleResult(result: any): Record<string, unknown> | null {
  if (result === undefined) return null;
  return {
    model: { provider: result.model.provider, api: result.model.api, id: result.model.id },
    thinkingLevel: result.thinkingLevel,
    isScoped: result.isScoped,
  };
}

function modelControlSnapshot(session: any): Record<string, unknown> {
  return {
    model: { provider: session.model.provider, api: session.model.api, id: session.model.id },
    thinkingLevel: session.thinkingLevel,
    availableThinkingLevels: [...session.getAvailableThinkingLevels()],
    supportsThinking: session.supportsThinking(),
    steeringMode: session.steeringMode,
    followUpMode: session.followUpMode,
    scopedModels: session.scopedModels.map((scoped: any) => ({
      model: { provider: scoped.model.provider, api: scoped.model.api, id: scoped.model.id },
      thinkingLevel: scoped.thinkingLevel ?? null,
    })),
  };
}

function modelControlSettingsSnapshot(settingsManager: any): Record<string, unknown> {
  return {
    defaultProvider: settingsManager.getDefaultProvider() ?? null,
    defaultModel: settingsManager.getDefaultModel() ?? null,
    defaultThinkingLevel: settingsManager.getDefaultThinkingLevel() ?? null,
    steeringMode: settingsManager.getSteeringMode(),
    followUpMode: settingsManager.getFollowUpMode(),
  };
}

async function runModelControlScenario(
  root: string,
  sdkModule: any,
  sessionModule: any,
  settingsModule: any,
  authModule: any,
  utilityModule: any,
  modelTestModule: any,
  harnessModule: any,
): Promise<Record<string, unknown>> {
  const scenario = corpus.modelControlScenario;
  if (scenario.models.length !== 3 || !scenario.models[0].reasoning || scenario.models[1].reasoning || !scenario.models[2].reasoning) {
    throw new Error("model control workflow requires reasoning, plain, and reasoning models in that order");
  }

  const scenarioRoot = join(root, "model-control");
  const cwd = join(scenarioRoot, "project");
  const agentDir = join(scenarioRoot, "agent");
  const sessionDir = join(scenarioRoot, "sessions");
  mkdirSync(cwd, { recursive: true });
  mkdirSync(agentDir, { recursive: true });
  mkdirSync(sessionDir, { recursive: true });

  const models = scenario.models.map((input) => ({
    ...harnessModule.fauxModel,
    id: input.id,
    name: input.name,
    reasoning: input.reasoning,
    thinkingLevelMap: input.thinkingLevelMap,
  }));
  const authStorage = authModule.AuthStorage.inMemory();
  await authStorage.modify(models[0].provider, async () => ({ type: "api_key", key: "faux-key" }));
  const modelRegistry = await modelTestModule.createInMemoryModelRegistry(authStorage);
  const controlStream = harnessModule.createFauxStreamFn([{
    text: scenario.response.text,
    stopReason: "stop",
    model: { provider: models[0].provider, id: models[0].id },
    usage: {
      input: scenario.response.inputTokens,
      output: scenario.response.outputTokens,
      cacheRead: 0,
      cacheWrite: 0,
      totalTokens: scenario.response.inputTokens + scenario.response.outputTokens,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
    },
  }]);
  const providerModels: any[] = [];
  const streamOptions: any[] = [];
  const streamSimple = (model: any, context: any, options: any) => {
    providerModels.push(model);
    streamOptions.push(options);
    return controlStream.streamFn(model, context, options);
  };
  modelRegistry.registerProvider(models[0].provider, {
    baseUrl: models[0].baseUrl,
    apiKey: "faux-key",
    api: models[0].api,
    streamSimple,
    models: models.map((model) => ({
      id: model.id,
      name: model.name,
      api: model.api,
      reasoning: model.reasoning,
      thinkingLevelMap: model.thinkingLevelMap,
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
        pi.on("model_select", async (event: any) => {
          controlActions.push(`model_select:${event.previousModel?.id ?? "none"}->${event.model.id}:${event.source}`);
        });
        pi.on("thinking_level_select", async (event: any) => {
          controlActions.push(`thinking_level_select:${event.previousLevel}->${event.level}`);
        });
      },
      path: "<model-control-extension>",
    },
  ], cwd);
  const settingsManager = settingsModule.SettingsManager.inMemory({
    compaction: { enabled: false },
    retry: { enabled: false },
    transport: "sse",
    steeringMode: "one-at-a-time",
    followUpMode: "all",
  });
  const sessionManager = sessionModule.SessionManager.create(cwd, sessionDir, { id: scenario.sessionId });
  const resourceLoader = {
    ...utilityModule.createTestResourceLoader({ extensionsResult }),
    getSystemPrompt: () => scenario.systemPrompt,
  };
  const created = await sdkModule.createAgentSession({
    cwd,
    agentDir,
    model: models[0],
    thinkingLevel: scenario.initialThinkingLevel,
    scopedModels: [
      { model: models[0] },
      { model: models[1] },
      { model: models[2], thinkingLevel: scenario.scopedThinkingLevel },
    ],
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
  await session.bindExtensions({ shutdownHandler: () => {} });

  const initial = modelControlSnapshot(session);
  session.setThinkingLevel(scenario.clampRequest);
  const afterClamp = modelControlSnapshot(session);
  const thinkingCycle = session.cycleThinkingLevel() ?? null;
  const afterThinkingCycle = modelControlSnapshot(session);
  const scopedPlain = normalizeModelCycleResult(await session.cycleModel("forward"));
  const afterScopedPlain = modelControlSnapshot(session);
  const plainThinkingCycle = session.cycleThinkingLevel() ?? null;
  const scopedReasoning = normalizeModelCycleResult(await session.cycleModel("forward"));
  const afterScopedReasoning = modelControlSnapshot(session);
  await session.setModel(models[0]);
  const afterDirectSet = modelControlSnapshot(session);
  session.setSteeringMode(scenario.steeringMode);
  session.setFollowUpMode(scenario.followUpMode);
  const afterQueueModes = modelControlSnapshot(session);
  const settings = modelControlSettingsSnapshot(settingsManager);
  await session.prompt(scenario.prompt);
  const promptReturn = {
    isStreaming: session.isStreaming,
    isIdle: session.isIdle,
    ...queueSnapshot(session),
  };
  if (controlStream.state.callCount !== 1 || providerModels.length !== 1 || streamOptions.length !== 1) {
    throw new Error(
      `model control provider calls ${controlStream.state.callCount}/${providerModels.length}/${streamOptions.length}, want 1/1/1; state=${JSON.stringify(modelControlSnapshot(session))}; events=${events.map((event) => event.type).join(",")}`,
    );
  }

  const entries = sessionManager.getEntries();
  const ids: EntryIDMap = new Map(entries.map((entry: any, index: number) => [entry.id, `entry-${index + 1}`]));
  const sessionFile = session.sessionFile;
  if (!sessionFile) throw new Error("persistent model control AgentSession did not publish a session file");
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
      initial,
      afterClamp,
      thinkingCycle,
      afterThinkingCycle,
      scopedPlain,
      afterScopedPlain,
      plainThinkingCycle,
      scopedReasoning,
      afterScopedReasoning,
      afterDirectSet,
      afterQueueModes,
      settings,
      promptReturn,
      controlActions,
    },
    providerInputs: controlStream.state.contexts.map((context: any, index: number) =>
      normalizeProviderInput(
        providerModels[index],
        context,
        streamOptions[index],
        scenarioRoot,
        cwd,
        scenario.sessionId,
      )),
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

async function runRetryAbortScenario(
  root: string,
  sdkModule: any,
  sessionModule: any,
  settingsModule: any,
  authModule: any,
  utilityModule: any,
  modelTestModule: any,
  harnessModule: any,
): Promise<Record<string, unknown>> {
  const scenario = corpus.retryAbortScenario;
  const scenarioRoot = join(root, "retry-abort");
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
      provider: { maxRetries: 0 },
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
  let signalRetryScheduled!: () => void;
  const retryScheduled = new Promise<void>((resolve) => {
    signalRetryScheduled = resolve;
  });
  session.subscribe((event: any) => {
    events.push(event);
    if (event.type === "auto_retry_start") signalRetryScheduled();
    if (event.type === "agent_settled") {
      settledSnapshots.push({
        isStreaming: session.isStreaming,
        isIdle: session.isIdle,
        isRetrying: session.isRetrying,
        ...queueSnapshot(session),
      });
    }
  });

  const firstRun = session.prompt(scenario.firstPrompt);
  await retryScheduled;
  for (let attempt = 0; attempt < 100 && !session.isRetrying; attempt++) {
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
  }
  if (!session.isRetrying) throw new Error("retry cancellation workflow did not enter retry sleep");
  const beforeAbortRetry = {
    isStreaming: session.isStreaming,
    isIdle: session.isIdle,
    isRetrying: session.isRetrying,
    ...queueSnapshot(session),
  };
  session.abortRetry();
  session.abortRetry();
  await firstRun;
  const firstPromptReturn = {
    isStreaming: session.isStreaming,
    isIdle: session.isIdle,
    isRetrying: session.isRetrying,
    settledEventCount: settledSnapshots.length,
    ...queueSnapshot(session),
  };
  await session.prompt(scenario.secondPrompt);
  const secondPromptReturn = {
    isStreaming: session.isStreaming,
    isIdle: session.isIdle,
    isRetrying: session.isRetrying,
    settledEventCount: settledSnapshots.length,
    ...queueSnapshot(session),
  };
  if (faux.state.callCount !== 2 || streamOptions.length !== 2) {
    throw new Error(`retry cancellation provider calls ${faux.state.callCount}/${streamOptions.length}, want 2/2`);
  }
  if (settledSnapshots.length !== 2) {
    throw new Error(`retry cancellation settled events ${settledSnapshots.length}, want 2`);
  }

  const entries = sessionManager.getEntries();
  const ids: EntryIDMap = new Map(entries.map((entry: any, index: number) => [entry.id, `entry-${index + 1}`]));
  const sessionFile = session.sessionFile;
  if (!sessionFile) throw new Error("persistent retry cancellation AgentSession did not publish a session file");
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
    actions: { beforeAbortRetry, firstPromptReturn, secondPromptReturn, settledSnapshots },
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

function runtimeReplacementSnapshot(runtimeHost: any, owner: string): Record<string, unknown> {
  const session = runtimeHost.session;
  return {
    owner,
    sessionFile: session.sessionFile,
    sessionId: session.sessionId,
    cwd: runtimeHost.cwd,
    model: { provider: session.model.provider, api: session.model.api, id: session.model.id },
    thinkingLevel: session.thinkingLevel,
    messageRoles: session.messages.map((message: any) => message.role),
  };
}

function normalizeRuntimeReplacementAction(
  action: Record<string, unknown>,
  fileLabels: Map<string, string>,
  root: string,
  cwd: string,
): Record<string, unknown> {
  const normalized: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(action)) {
    if ((key === "sessionFile" || key === "targetSessionFile" || key === "previousSessionFile") && typeof value === "string") {
      normalized[key] = fileLabels.get(value) ?? normalizePathText(value, root, cwd);
    } else {
      normalized[key] = value;
    }
  }
  return normalized;
}

function normalizeRuntimeReplacementSnapshot(
  snapshot: Record<string, unknown>,
  fileLabels: Map<string, string>,
  sourceSessionId: string,
  root: string,
  cwd: string,
): Record<string, unknown> {
  const normalized = normalizeRuntimeReplacementAction(snapshot, fileLabels, root, cwd);
  normalized.cwd = normalizePathText(String(snapshot.cwd), root, cwd);
  normalized.sessionId = snapshot.sessionId === sourceSessionId ? sourceSessionId : "<replacement-session-id>";
  return normalized;
}

function normalizeRuntimeReplacementHeader(
  header: any,
  sourceSessionId: string,
  root: string,
  cwd: string,
): Record<string, unknown> {
  const normalized = normalizeHeader(header, root, cwd);
  if (normalized.id !== sourceSessionId) normalized.id = "<replacement-session-id>";
  return normalized;
}

function normalizeRuntimeReplacementProjection(
  manager: any,
  fileData: string,
  sourceSessionId: string,
  root: string,
  cwd: string,
): Record<string, unknown> {
  const entries = manager.getEntries();
  const ids: EntryIDMap = new Map(entries.map((entry: any, index: number) => [entry.id, `entry-${index + 1}`]));
  const lines = fileData.trimEnd().split("\n").map((line) => JSON.parse(line));
  const context = manager.buildSessionContext();
  return {
    header: normalizeRuntimeReplacementHeader(manager.getHeader(), sourceSessionId, root, cwd),
    entries: entries.map((entry: any) => normalizeEntry(entry, ids)),
    fileEntries: lines.slice(1).map((entry: any) => normalizeEntry(entry, ids)),
    context: {
      messages: context.messages.map(normalizeMessage),
      model: context.model,
      thinkingLevel: context.thinkingLevel,
    },
  };
}

async function runRuntimeReplacementScenario(
  root: string,
  runtimeModule: any,
  sessionModule: any,
  settingsModule: any,
  authModule: any,
  modelTestModule: any,
  harnessModule: any,
  eventStreamModule: any,
): Promise<Record<string, unknown>> {
  const scenario = corpus.runtimeReplacementScenario;
  if (scenario.responses.length !== 3) {
    throw new Error("runtime replacement workflow requires new, resumed, and imported responses");
  }
  const scenarioRoot = join(root, "runtime-replacement");
  const cwd = join(scenarioRoot, "project");
  const agentDir = join(scenarioRoot, "agent");
  const sessionDir = join(scenarioRoot, "sessions");
  const externalDir = join(scenarioRoot, "external");
  mkdirSync(cwd, { recursive: true });
  mkdirSync(agentDir, { recursive: true });
  mkdirSync(sessionDir, { recursive: true });
  mkdirSync(externalDir, { recursive: true });

  const normal = harnessModule.createFauxStreamFn(scenario.responses.map((response) => ({
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
  const providerContexts: any[] = [];
  const streamOptions: any[] = [];
  let signalFirstCallStarted!: () => void;
  const firstCallStarted = new Promise<void>((resolve) => {
    signalFirstCallStarted = resolve;
  });
  const streamSimple = (streamModel: any, context: any, options: any) => {
    const callIndex = providerContexts.length;
    providerModels.push(streamModel);
    providerContexts.push(context);
    streamOptions.push(options);
    if (callIndex !== 0) return normal.streamFn(streamModel, context, options);

    const stream = eventStreamModule.createAssistantMessageEventStream();
    let finished = false;
    const abort = () => {
      if (finished) return;
      finished = true;
      stream.push({
        type: "error",
        reason: "aborted",
        error: {
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
        },
      });
    };
    if (options?.signal?.aborted) abort();
    else options?.signal?.addEventListener("abort", abort, { once: true });
    signalFirstCallStarted();
    return stream;
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
    retry: { enabled: false, provider: { maxRetries: 0 } },
    transport: "sse",
  });

  const lifecycle: Array<Record<string, unknown>> = [];
  const hostActions: Array<Record<string, unknown>> = [];
  const owners = new WeakMap<object, string>();
  const generations = ["source", "new", "source-resume", "import"];
  let factoryCalls = 0;
  const createRuntime = async ({ cwd: runtimeCwd, sessionManager, sessionStartEvent }: any) => {
    const owner = generations[factoryCalls] ?? `generation-${factoryCalls}`;
    factoryCalls++;
    const sessionFile = sessionManager.getSessionFile();
    const factoryAction: Record<string, unknown> = {
      type: "factory",
      owner,
      reason: sessionStartEvent?.reason ?? "startup",
    };
    if (sessionFile !== undefined) factoryAction.sessionFile = sessionFile;
    if (sessionStartEvent?.previousSessionFile !== undefined) {
      factoryAction.previousSessionFile = sessionStartEvent.previousSessionFile;
    }
    hostActions.push(factoryAction);
    const extensionFactory = (pi: any) => {
      pi.on("session_before_switch", (event: any) => {
        const action: Record<string, unknown> = { type: "session_before_switch", owner, reason: event.reason };
        if (event.targetSessionFile !== undefined) action.targetSessionFile = event.targetSessionFile;
        lifecycle.push(action);
      });
      pi.on("session_shutdown", (event: any) => {
        const action: Record<string, unknown> = { type: "session_shutdown", owner, reason: event.reason };
        if (event.targetSessionFile !== undefined) action.targetSessionFile = event.targetSessionFile;
        lifecycle.push(action);
      });
      pi.on("session_start", (event: any) => {
        const action: Record<string, unknown> = { type: "session_start", owner, reason: event.reason };
        if (event.previousSessionFile !== undefined) action.previousSessionFile = event.previousSessionFile;
        lifecycle.push(action);
      });
    };
    const services = await runtimeModule.createAgentSessionServices({
      cwd: runtimeCwd,
      agentDir,
      modelRuntime: modelTestModule.getModelRuntime(modelRegistry),
      settingsManager,
      resourceLoaderOptions: {
        extensionFactories: [extensionFactory],
        noSkills: true,
        noPromptTemplates: true,
        noThemes: true,
        noContextFiles: true,
        systemPrompt: scenario.systemPrompt,
      },
    });
    const created = await runtimeModule.createAgentSessionFromServices({
      services,
      sessionManager,
      sessionStartEvent,
      model,
      thinkingLevel: "off",
      tools: [],
      customTools: [],
    });
    owners.set(created.session, owner);
    return { ...created, services, diagnostics: services.diagnostics };
  };

  const sourceManager = sessionModule.SessionManager.create(cwd, sessionDir, { id: scenario.sourceSessionId });
  const runtimeHost = await runtimeModule.createAgentSessionRuntime(createRuntime, {
    cwd,
    agentDir,
    sessionManager: sourceManager,
  });
  await runtimeHost.session.bindExtensions({ shutdownHandler: () => {} });
  const sourceSession = runtimeHost.session;
  const sourceFile = sourceSession.sessionFile;
  if (!sourceFile) throw new Error("runtime replacement source has no session file");

  await sourceSession.reload({
    beforeSessionStart: async () => {
      hostActions.push({ type: "rebind", owner: "source", reason: "reload", sessionFile: sourceFile });
    },
  });
  runtimeHost.setBeforeSessionInvalidate(() => {
    const current = runtimeHost.session;
    hostActions.push({
      type: "invalidate",
      owner: owners.get(current) ?? "unknown",
      sessionFile: current.sessionFile,
    });
  });
  runtimeHost.setRebindSession(async (replacement: any) => {
    await replacement.bindExtensions({ shutdownHandler: () => {} });
    hostActions.push({
      type: "rebind",
      owner: owners.get(replacement) ?? "unknown",
      reason: "replacement",
      sessionFile: replacement.sessionFile,
    });
  });

  const initialRun = sourceSession.prompt(scenario.initialPrompt);
  await firstCallStarted;
  const newResult = await runtimeHost.newSession({
    withSession: async () => {
      hostActions.push({ type: "with_session", owner: "new" });
    },
  });
  await initialRun;
  const sourceRunReturn = {
    isStreaming: sourceSession.isStreaming,
    isIdle: sourceSession.isIdle,
    ...queueSnapshot(sourceSession),
  };
  const afterNew = runtimeReplacementSnapshot(runtimeHost, "new");
  await runtimeHost.session.prompt(scenario.newPrompt);
  const newFile = runtimeHost.session.sessionFile;
  if (!newFile) throw new Error("runtime replacement new session has no session file");
  const newData = readFileSync(newFile, "utf8");
  const externalImportFile = join(externalDir, "runtime-import.jsonl");
  writeFileSync(externalImportFile, newData, "utf8");

  const switchResult = await runtimeHost.switchSession(sourceFile, {
    withSession: async () => {
      hostActions.push({ type: "with_session", owner: "source-resume" });
    },
  });
  const afterSwitch = runtimeReplacementSnapshot(runtimeHost, "source-resume");
  await runtimeHost.session.prompt(scenario.resumePrompt);

  const importResult = await runtimeHost.importFromJsonl(externalImportFile);
  const afterImport = runtimeReplacementSnapshot(runtimeHost, "import");
  await runtimeHost.session.prompt(scenario.importPrompt);
  const importFile = runtimeHost.session.sessionFile;
  if (!importFile) throw new Error("runtime replacement imported session has no session file");
  const importData = readFileSync(importFile, "utf8");
  const finalStats = runtimeHost.session.getSessionStats();
  const finalState = {
    isStreaming: runtimeHost.session.isStreaming,
    pendingMessageCount: runtimeHost.session.pendingMessageCount,
    model: {
      provider: runtimeHost.session.model.provider,
      api: runtimeHost.session.model.api,
      id: runtimeHost.session.model.id,
    },
    thinkingLevel: runtimeHost.session.thinkingLevel,
    activeTools: runtimeHost.session.getActiveToolNames(),
    systemPrompt: normalizePathText(runtimeHost.session.systemPrompt, scenarioRoot, cwd),
    messages: runtimeHost.session.messages.map(normalizeMessage),
    stats: normalizeStats(finalStats),
  };
  await runtimeHost.dispose();

  if (providerContexts.length !== 4 || providerModels.length !== 4 || streamOptions.length !== 4 || normal.state.callCount !== 3) {
    throw new Error(
      `runtime replacement provider calls ${providerContexts.length}/${providerModels.length}/${streamOptions.length}/${normal.state.callCount}, want 4/4/4/3`,
    );
  }
  if (factoryCalls !== 4) throw new Error(`runtime replacement factory calls ${factoryCalls}, want 4`);

  const fileLabels = new Map<string, string>([
    [sourceFile, "<source-session-file>"],
    [newFile, "<new-session-file>"],
    [importFile, "<import-session-file>"],
    [externalImportFile, "<external-import-file>"],
  ]);
  const sourceData = readFileSync(sourceFile, "utf8");
  const sourceReopened = sessionModule.SessionManager.open(sourceFile, sessionDir);
  const newReopened = sessionModule.SessionManager.open(newFile, sessionDir);
  const importReopened = sessionModule.SessionManager.open(importFile, sessionDir);
  const normalizedStats = normalizeStats(finalStats);
  normalizedStats.sessionId = "<replacement-session-id>";
  finalState.stats = normalizedStats;
  const normalizeAction = (action: Record<string, unknown>) =>
    normalizeRuntimeReplacementAction(action, fileLabels, scenarioRoot, cwd);
  return {
    name: scenario.name,
    input: scenario,
    actions: {
      sourceRunReturn,
      newResult,
      switchResult,
      importResult,
      afterNew: normalizeRuntimeReplacementSnapshot(afterNew, fileLabels, scenario.sourceSessionId, scenarioRoot, cwd),
      afterSwitch: normalizeRuntimeReplacementSnapshot(
        afterSwitch,
        fileLabels,
        scenario.sourceSessionId,
        scenarioRoot,
        cwd,
      ),
      afterImport: normalizeRuntimeReplacementSnapshot(
        afterImport,
        fileLabels,
        scenario.sourceSessionId,
        scenarioRoot,
        cwd,
      ),
      lifecycle: lifecycle.map(normalizeAction),
      hostActions: hostActions.map(normalizeAction),
      files: {
        source: "<source-session-file>",
        created: "<new-session-file>",
        imported: "<import-session-file>",
        external: "<external-import-file>",
        allDistinct: new Set([sourceFile, newFile, importFile, externalImportFile]).size === 4,
        importStartsWithCreated: importData.startsWith(newData),
      },
    },
    providerInputs: providerContexts.map((context: any, index: number) =>
      normalizeProviderInput(
        providerModels[index],
        context,
        streamOptions[index],
        scenarioRoot,
        cwd,
        scenario.sourceSessionId,
        "<replacement-session-id>",
      )),
    finalState,
    sessions: {
      source: normalizeRuntimeReplacementProjection(
        sourceReopened,
        sourceData,
        scenario.sourceSessionId,
        scenarioRoot,
        cwd,
      ),
      created: normalizeRuntimeReplacementProjection(
        newReopened,
        newData,
        scenario.sourceSessionId,
        scenarioRoot,
        cwd,
      ),
      imported: normalizeRuntimeReplacementProjection(
        importReopened,
        importData,
        scenario.sourceSessionId,
        scenarioRoot,
        cwd,
      ),
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

function normalizeDamagedContext(context: any): Record<string, unknown> {
  return {
    messages: context.messages.map(normalizeMessage),
    model: context.model ?? null,
    thinkingLevel: context.thinkingLevel,
  };
}

function normalizeDamagedPhysicalLines(
  data: string,
  ids: EntryIDMap,
  root: string,
  cwd: string,
): Array<Record<string, unknown>> {
  return data.trimEnd().split("\n").map((line) => {
    let value: any;
    try {
      value = JSON.parse(line);
    } catch {
      return { kind: "malformed", text: line };
    }
    if (value.type === "session") {
      return { kind: "header", value: normalizeHeader(value, root, cwd) };
    }
    return { kind: "entry", value: normalizeEntry(value, ids) };
  });
}

async function runDamagedSessionScenario(
  root: string,
  runtimeModule: any,
  sessionModule: any,
  settingsModule: any,
  authModule: any,
  modelTestModule: any,
  harnessModule: any,
): Promise<Record<string, unknown>> {
  const scenario = corpus.damagedSessionScenario;
  const scenarioRoot = join(root, "damaged-session");
  const cwd = join(scenarioRoot, "project");
  const agentDir = join(scenarioRoot, "agent");
  const sessionDir = join(scenarioRoot, "sessions");
  mkdirSync(cwd, { recursive: true });
  mkdirSync(agentDir, { recursive: true });
  mkdirSync(sessionDir, { recursive: true });

  const model = harnessModule.fauxModel;
  const seedUsage = {
    input: scenario.rootResponse.inputTokens,
    output: scenario.rootResponse.outputTokens,
    cacheRead: 0,
    cacheWrite: 0,
    totalTokens: scenario.rootResponse.inputTokens + scenario.rootResponse.outputTokens,
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
  };
  const seedRecords = [
    {
      type: "session", version: 3, id: scenario.sessionId,
      timestamp: "2026-08-10T00:00:00.000Z", cwd,
    },
    {
      type: "model_change", id: "seed-model", parentId: null,
      timestamp: "2026-08-10T00:00:01.000Z", provider: model.provider, modelId: model.id,
    },
    {
      type: "thinking_level_change", id: "seed-thinking", parentId: "seed-model",
      timestamp: "2026-08-10T00:00:02.000Z", thinkingLevel: "off",
    },
    {
      type: "message", id: "seed-user", parentId: "seed-thinking",
      timestamp: "2026-08-10T00:00:03.000Z",
      message: { role: "user", content: scenario.rootPrompt, timestamp: 1000 },
    },
    {
      type: "message", id: "seed-assistant", parentId: "seed-user",
      timestamp: "2026-08-10T00:00:04.000Z",
      message: {
        role: "assistant",
        content: [{ type: "text", text: scenario.rootResponse.text }],
        api: model.api,
        provider: model.provider,
        model: model.id,
        usage: seedUsage,
        stopReason: "stop",
        timestamp: 2000,
      },
    },
    scenario.malformedLine,
    {
      type: "message", id: "seed-orphan", parentId: "missing-parent",
      timestamp: "2026-08-10T00:00:05.000Z",
      message: { role: "user", content: scenario.orphanPrompt, timestamp: 3000 },
    },
  ];
  const sourceData = `${seedRecords.map((record) =>
    typeof record === "string" ? record : JSON.stringify(record)).join("\n")}\n`;
  const sessionFile = join(sessionDir, "damaged.jsonl");
  writeFileSync(sessionFile, sourceData, "utf8");

  const faux = harnessModule.createFauxStreamFn([{
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
  }]);
  const providerModels: any[] = [];
  const streamOptions: any[] = [];
  const streamSimple = (requestModel: any, context: any, options: any) => {
    providerModels.push(requestModel);
    streamOptions.push(options);
    return faux.streamFn(requestModel, context, options);
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
  const manager = sessionModule.SessionManager.open(sessionFile, sessionDir);
  const runtimeHost = await runtimeModule.createAgentSessionRuntime(createRuntime, {
    cwd,
    agentDir,
    sessionManager: manager,
  });
  const agentSession = runtimeHost.session;
  const events: any[] = [];
  agentSession.subscribe((event: any) => events.push(event));
  const captureProjection = (sessionManager: any) => ({
    header: sessionManager.getHeader(),
    leafId: sessionManager.getLeafId(),
    entries: sessionManager.getEntries(),
    tree: sessionManager.getTree(),
    context: sessionManager.buildSessionContext(),
  });
  const beforeResume = captureProjection(agentSession.sessionManager);
  await agentSession.prompt(scenario.continuationPrompt);
  if (faux.state.callCount !== 1 || providerModels.length !== 1 || streamOptions.length !== 1) {
    throw new Error(
      `damaged session provider calls ${faux.state.callCount}/${providerModels.length}/${streamOptions.length}, want 1/1/1`,
    );
  }

  const finalProjection = captureProjection(agentSession.sessionManager);
  const entries = finalProjection.entries;
  const ids: EntryIDMap = new Map();
  addEntryIDs(entries, ids);
  ids.set("missing-parent", "<missing-parent>");
  const stats = agentSession.getSessionStats();
  const finalState = {
    isStreaming: agentSession.isStreaming,
    pendingMessageCount: agentSession.pendingMessageCount,
    model: { provider: agentSession.model.provider, api: agentSession.model.api, id: agentSession.model.id },
    thinkingLevel: agentSession.thinkingLevel,
    activeTools: agentSession.getActiveToolNames(),
    systemPrompt: normalizePathText(agentSession.systemPrompt, scenarioRoot, cwd),
    messages: agentSession.messages.map(normalizeMessage),
    stats: normalizeStats(stats),
  };
  const afterData = readFileSync(sessionFile, "utf8");
  await runtimeHost.dispose();

  const reopened = sessionModule.SessionManager.open(sessionFile, sessionDir);
  const reopenedProjection = captureProjection(reopened);
  const normalizeProjection = (projection: any) => ({
    header: normalizeHeader(projection.header, scenarioRoot, cwd),
    leafId: projection.leafId === null ? null : ids.get(projection.leafId),
    entries: projection.entries.map((entry: any) => normalizeEntry(entry, ids)),
    tree: normalizeSessionTree(projection.tree, ids),
    context: normalizeDamagedContext(projection.context),
  });
  return {
    name: scenario.name,
    input: scenario,
    actions: {
      sourcePrefixPreserved: afterData.startsWith(sourceData),
      malformedLineCountBefore: normalizeDamagedPhysicalLines(sourceData, ids, scenarioRoot, cwd)
        .filter((line) => line.kind === "malformed").length,
      malformedLineCountAfter: normalizeDamagedPhysicalLines(afterData, ids, scenarioRoot, cwd)
        .filter((line) => line.kind === "malformed").length,
    },
    providerInputs: faux.state.contexts.map((context: any, index: number) =>
      normalizeProviderInput(
        providerModels[index],
        context,
        streamOptions[index],
        scenarioRoot,
        cwd,
        scenario.sessionId,
      )),
    events: events.map((event) => normalizeEvent(event, ids)),
    beforeResume: normalizeProjection(beforeResume),
    finalState,
    session: {
      ...normalizeProjection(finalProjection),
      physicalLinesBefore: normalizeDamagedPhysicalLines(sourceData, ids, scenarioRoot, cwd),
      physicalLinesAfter: normalizeDamagedPhysicalLines(afterData, ids, scenarioRoot, cwd),
      reopened: normalizeProjection(reopenedProjection),
    },
  };
}

async function runRequestAssemblyScenario(
  root: string,
  runtimeModule: any,
  sessionModule: any,
  settingsModule: any,
  authModule: any,
  modelTestModule: any,
  harnessModule: any,
  Type: any,
): Promise<Record<string, unknown>> {
  const scenario = corpus.requestAssemblyScenario;
  if (scenario.responses.length !== 3 || !scenario.responses[0]?.toolCall) {
    throw new Error("request assembly workflow requires one tool response followed by two text responses");
  }
  const scenarioRoot = join(root, "request-assembly");
  const cwd = join(scenarioRoot, "project");
  const agentDir = join(scenarioRoot, "agent");
  const sessionDir = join(scenarioRoot, "sessions");
  const explicitDir = join(scenarioRoot, "explicit-resources");
  const skillDir = join(explicitDir, "skills", scenario.skill.name);
  const skillPath = join(skillDir, "SKILL.md");
  const promptPath = join(explicitDir, "prompts", `${scenario.template.name}.md`);
  mkdirSync(cwd, { recursive: true });
  mkdirSync(agentDir, { recursive: true });
  mkdirSync(sessionDir, { recursive: true });
  mkdirSync(skillDir, { recursive: true });
  mkdirSync(dirname(promptPath), { recursive: true });
  writeFileSync(
    skillPath,
    `---\nname: ${scenario.skill.name}\ndescription: ${scenario.skill.description}\n---\n${scenario.skill.body}`,
    "utf8",
  );
  writeFileSync(promptPath, scenario.template.content, "utf8");

  const model = {
    ...harnessModule.fauxModel,
    id: "faux-reasoning",
    name: "Faux Reasoning Model",
    reasoning: true,
  };
  const faux = harnessModule.createFauxStreamFn(scenario.responses.map((response) => ({
    text: response.text,
    toolCalls: response.toolCall
      ? [{ id: scenario.tool.callId, name: scenario.tool.name, args: { label: scenario.tool.argument } }]
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
  })));
  const providerModels: any[] = [];
  const streamOptions: any[] = [];
  const streamSimple = (requestModel: any, context: any, options: any) => {
    providerModels.push(requestModel);
    streamOptions.push(options);
    return faux.streamFn(requestModel, context, options);
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
    images: { autoResize: false, blockImages: true },
    thinkingBudgets: scenario.thinkingBudgets,
    skills: [skillPath],
    prompts: [promptPath],
  });
  const services = await runtimeModule.createAgentSessionServices({
    cwd,
    agentDir,
    modelRuntime: modelTestModule.getModelRuntime(modelRegistry),
    settingsManager,
    resourceLoaderOptions: {
      noExtensions: true,
      noThemes: true,
      noContextFiles: true,
      systemPrompt: scenario.systemPrompt,
    },
  });
  const toolRuns: Array<Record<string, unknown>> = [];
  const readTool = {
    name: "read",
    label: "Read",
    description: "Read deterministic resources",
    parameters: Type.Object(
      { path: Type.String() },
      { additionalProperties: false },
    ),
    execute: async () => ({ content: [{ type: "text", text: "unused read" }], details: null }),
  };
  const imageTool = {
    name: scenario.tool.name,
    label: "Image Probe",
    description: scenario.tool.description,
    parameters: Type.Object(
      { label: Type.String() },
      { additionalProperties: false },
    ),
    execute: async (toolCallId: string, params: { label: string }) => {
      toolRuns.push({ toolCallId, arguments: { label: params.label } });
      return {
        content: [
          { type: "text", text: scenario.tool.resultText },
          { type: "image", mimeType: scenario.image.mimeType, data: scenario.image.base64 },
          { type: "image", mimeType: scenario.image.mimeType, data: scenario.image.base64 },
        ],
        details: { label: params.label },
      };
    },
  };
  const sessionManager = sessionModule.SessionManager.create(cwd, sessionDir, { id: scenario.sessionId });
  const created = await runtimeModule.createAgentSessionFromServices({
    services,
    sessionManager,
    model,
    thinkingLevel: scenario.thinkingLevel,
    tools: [readTool.name, imageTool.name],
    customTools: [readTool, imageTool],
  });
  const agentSession = created.session;
  const events: any[] = [];
  agentSession.subscribe((event: any) => events.push(event));
  const initialBlockImages = settingsManager.getBlockImages();
  await agentSession.prompt(`/skill:${scenario.skill.name} ${scenario.skill.argument}`, {
    images: [
      { type: "image", mimeType: scenario.image.mimeType, data: scenario.image.base64 },
      { type: "image", mimeType: scenario.image.mimeType, data: scenario.image.base64 },
    ],
  });
  settingsManager.setBlockImages(false);
  const finalBlockImages = settingsManager.getBlockImages();
  await agentSession.prompt(`/${scenario.template.name} ${scenario.template.argument}`, {
    images: [{ type: "image", mimeType: scenario.image.mimeType, data: scenario.image.base64 }],
  });
  if (faux.state.callCount !== 3 || providerModels.length !== 3 || streamOptions.length !== 3) {
    throw new Error(
      `request assembly provider calls ${faux.state.callCount}/${providerModels.length}/${streamOptions.length}, want 3/3/3`,
    );
  }

  const entries = sessionManager.getEntries();
  const ids: EntryIDMap = new Map(entries.map((entry: any, index: number) => [entry.id, `entry-${index + 1}`]));
  const sessionFile = agentSession.sessionFile;
  if (!sessionFile) throw new Error("request assembly session did not publish a session file");
  const fileLines = readFileSync(sessionFile, "utf8").trimEnd().split("\n").map((line) => JSON.parse(line));
  const header = fileLines[0];
  const fileEntries = fileLines.slice(1);
  const stats = agentSession.getSessionStats();
  const loadedSkills = services.resourceLoader.getSkills().skills.map((skill: any) => ({
    name: skill.name,
    description: skill.description,
    filePath: normalizePathText(skill.filePath, scenarioRoot, cwd),
    baseDir: normalizePathText(skill.baseDir, scenarioRoot, cwd),
    disableModelInvocation: skill.disableModelInvocation,
  }));
  const loadedTemplates = agentSession.promptTemplates.map((template: any) => ({
    name: template.name,
    description: template.description,
    argumentHint: template.argumentHint ?? null,
    content: template.content,
    filePath: normalizePathText(template.filePath, scenarioRoot, cwd),
  }));
  const finalState = {
    isStreaming: agentSession.isStreaming,
    pendingMessageCount: agentSession.pendingMessageCount,
    model: { provider: agentSession.model.provider, api: agentSession.model.api, id: agentSession.model.id },
    thinkingLevel: agentSession.thinkingLevel,
    activeTools: agentSession.getActiveToolNames(),
    systemPrompt: normalizePathText(agentSession.systemPrompt, scenarioRoot, cwd),
    messages: agentSession.messages.map(normalizeMessage),
    stats: normalizeStats(stats),
  };
  agentSession.dispose();
  const reopened = sessionModule.SessionManager.open(sessionFile, sessionDir);
  const reopenedContext = reopened.buildSessionContext();
  const reopenedEntries = reopened.getEntries();
  return normalizePathStrings({
    name: scenario.name,
    input: scenario,
    actions: {
      initialBlockImages,
      finalBlockImages,
      loadedSkills,
      loadedTemplates,
      toolRuns,
    },
    providerInputs: faux.state.contexts.map((context: any, index: number) =>
      normalizeProviderInput(
        providerModels[index],
        context,
        streamOptions[index],
        scenarioRoot,
        cwd,
        scenario.sessionId,
        "<summary-session-id>",
        true,
      )),
    events: events.map((event) => normalizeEvent(event, ids)),
    finalState,
    session: {
      header: normalizeHeader(header, scenarioRoot, cwd),
      entries: entries.map((entry: any) => normalizeEntry(entry, ids)),
      fileEntries: fileEntries.map((entry: any) => normalizeEntry(entry, ids)),
      reopened: {
        header: normalizeHeader(reopened.getHeader(), scenarioRoot, cwd),
        entries: reopenedEntries.map((entry: any) => normalizeEntry(entry, ids)),
        context: normalizeDamagedContext(reopenedContext),
      },
    },
  }, scenarioRoot, cwd) as Record<string, unknown>;
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
    const modelControlScenario = await runModelControlScenario(
      root,
      sdkModule,
      sessionModule,
      settingsModule,
      authModule,
      utilityModule,
      modelTestModule,
      harnessModule,
    );
    const retryAbortScenario = await runRetryAbortScenario(
      root,
      sdkModule,
      sessionModule,
      settingsModule,
      authModule,
      utilityModule,
      modelTestModule,
      harnessModule,
    );
    const runtimeReplacementScenario = await runRuntimeReplacementScenario(
      root,
      runtimeModule,
      sessionModule,
      settingsModule,
      authModule,
      modelTestModule,
      harnessModule,
      eventStreamModule,
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
    const damagedSessionScenario = await runDamagedSessionScenario(
      root,
      runtimeModule,
      sessionModule,
      settingsModule,
      authModule,
      modelTestModule,
      harnessModule,
    );
    const requestAssemblyScenario = await runRequestAssemblyScenario(
      root,
      runtimeModule,
      sessionModule,
      settingsModule,
      authModule,
      modelTestModule,
      harnessModule,
      Type,
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
      modelControlScenario,
      retryAbortScenario,
      runtimeReplacementScenario,
      manualCompactionScenario,
      overflowCompactionScenario,
      turnSnapshotScenario,
      treeForkScenario,
      damagedSessionScenario,
      requestAssemblyScenario,
    };
    process.stdout.write(`${JSON.stringify(output, null, 2)}\n`);
  } finally {
    Math.random = originalRandom;
    if (process.env.PI_KEEP_ORACLE_TMP === "1") {
      process.stderr.write(`kept oracle temporary directory ${root}\n`);
    } else {
      rmSync(root, { recursive: true, force: true });
    }
  }
}

void main().catch((error) => {
  process.stderr.write(`${error instanceof Error ? error.stack ?? error.message : String(error)}\n`);
  process.exitCode = 1;
});
