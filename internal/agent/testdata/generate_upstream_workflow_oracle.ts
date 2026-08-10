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
  throw new Error(`unsupported message role ${String(message?.role)}`);
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
): Record<string, unknown> {
  return {
    model: { provider: model.provider, api: model.api, id: model.id },
    systemPrompt: normalizePathText(context.systemPrompt, root, cwd),
    messages: context.messages.map(normalizeMessage),
    tools: (context.tools ?? []).map(normalizeTool),
    stream: {
      sessionId: options?.sessionId ?? "",
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
      normalizeProviderInput(model, context, streamOptions[index], scenarioRoot, cwd)),
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
    const output = {
      upstreamCommit: corpus.upstreamCommit,
      generatedBy: "pinned packages/coding-agent createAgentSession with deterministic stream/tool inputs",
      generator: { nodeVersion: process.version, corpus: "upstream_workflow_corpus.json" },
      scenario: {
        name: corpus.scenario.name,
        input: corpus.scenario,
        providerInputs: faux.state.contexts.map((context: any, index: number) =>
          normalizeProviderInput(model, context, streamOptions[index], root, cwd)),
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
