import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type {
  AgentMessage,
  ApplicationClient,
  ApplicationEventEnvelope,
  ApplicationSnapshot,
  DirectoryView,
  ImageAttachment,
  ModelsView,
  ProjectInfo,
  QueuedMessages,
  SelectedModel,
  SessionStatsInfo,
  SessionRuntimeState,
  SessionInfo,
  NodesPage,
  ReadResourceRequest,
  LiveSnapshot,
  SlashCommandInfo,
} from "../contracts";
import {
  getPresetFromTools,
  getToolNamesForPreset,
  type ToolEntry,
  type ToolPreset,
} from "../tool-presets";
import { assistantResponseKey, assistantUsage, type LatestAssistantUsageInfo } from "./message";
import { SessionImageCache } from "./session-images";
import { RefreshScheduler } from "./refresh-scheduler";
import { applicationDataFor, type ApplicationDataStore, type DirectoryWindow, type HistoryWindow } from "./application-data";

type ControllerStatus = "connecting" | "auth" | "ready" | "error";
export type SendBehavior = "prompt" | "steer" | "follow_up";

interface WorkbenchSession {
  sessionId: string;
  filePath: string;
  info: SessionInfo;
  leafId: string | null;
  context: { messages: AgentMessage[]; entryIds: string[]; thinkingLevel: string; model: SelectedModel | null };
  running: boolean;
  state?: SessionRuntimeState;
}

export interface ApplicationController {
  data: ApplicationDataStore;
  historyWindow: HistoryWindow | null;
  toolProgress: LiveSnapshot["toolsRunning"];
  loadHistory(direction: "older" | "newer" | "latest", aroundId?: string): Promise<void>;
  loadNodes(query?: ReadResourceRequest): Promise<NodesPage>;
  directory(project: string, search?: string): DirectoryWindow | undefined;
  watchDirectory(project: string, search?: string): () => void;
  loadDirectory(project: string, search?: string, more?: boolean, force?: boolean): Promise<DirectoryWindow>;
  hasMoreProjects: boolean;
  loadProjects(): Promise<void>;
  imageCache: SessionImageCache;
  status: ControllerStatus;
  error: string;
  snapshot: ApplicationSnapshot | null;
  models: ModelsView | null;
  sessionView: WorkbenchSession | null;
  sessionLoading: boolean;
  sessionRefreshing: boolean;
  runtimeState: SessionRuntimeState | null;
  activeSessionId: string | null;
  workingDirectory: string;
  selectedModel: SelectedModel | null;
  thinkingLevel: string;
  toolPreset: ToolPreset;
  queuedMessages: QueuedMessages;
  slashCommands: SlashCommandInfo[];
  slashCommandsLoading: boolean;
  sessionStats: SessionStatsInfo | null;
  sessionStatsOpen: boolean;
  messages: AgentMessage[];
  pendingMessages: AgentMessage[];
  streamingMessage: AgentMessage | null;
  latestGenerationSpeed: AssistantGenerationSpeed | null;
  latestUsageInfo: LatestAssistantUsageInfo | null;
  busy: boolean;
  login(password: string): Promise<void>;
  selectSession(sessionId: string): Promise<void>;
  beginNewSession(cwd?: string): void;
  addProject(path: string): Promise<ProjectInfo>;
  removeProject(path: string): Promise<void>;
  send(text: string, behavior?: SendBehavior, images?: ImageAttachment[]): Promise<void>;
  abort(): Promise<void>;
  clearQueue(): Promise<string[]>;
  setModel(model: SelectedModel): Promise<void>;
  setThinkingLevel(level: string): Promise<void>;
  setToolPreset(preset: ToolPreset): Promise<void>;
  browseDirectories(path?: string): Promise<DirectoryView>;
  setWorkingDirectory(path: string): Promise<void>;
  compact(customInstructions?: string): Promise<void>;
  reload(): Promise<void>;
  copyLastAssistant(): Promise<void>;
  fork(entryId: string): Promise<void>;
  forkBefore(entryId: string): Promise<void>;
  editAndResend(entryId: string, text: string): Promise<void>;
  clone(): Promise<void>;
  navigateTree(entryId: string): Promise<void>;
  openSessionStats(): Promise<void>;
  closeSessionStats(): void;
  renameSession(sessionId: string, name: string): Promise<void>;
  deleteSession(sessionId: string): Promise<void>;
  cancelConnection(): void;
  retry(): Promise<void>;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

const emptyQueue: QueuedMessages = { steering: [], followUp: [] };

export interface AssistantGenerationSpeed {
  responseKey: string;
  tokensPerSecond: number;
}

interface ActiveGenerationTiming {
  sessionId: string;
  firstOutputAt: number | null;
}

function generationClockNow(): number {
  return globalThis.performance?.now?.() ?? Date.now();
}

function hasEffectiveAssistantDelta(event: ApplicationEventEnvelope["event"]): boolean {
  const value = event.assistantMessageEvent;
  if (!value || typeof value !== "object") return false;
  const update = value as Record<string, unknown>;
  const type = update.type;
  return (
    type === "text_delta"
    || type === "thinking_delta"
    || type === "toolcall_delta"
  ) && typeof update.delta === "string" && update.delta.length > 0;
}

function defaultModel(models: ModelsView | null): SelectedModel | null {
  if (models?.defaultModel) return models.defaultModel;
  const first = models?.modelList[0];
  return first ? { provider: first.provider, modelId: first.id } : null;
}

function defaultThinkingLevel(models: ModelsView | null, model: SelectedModel | null): string {
  if (!models || !model) return "auto";
  return models.thinkingLevelPins[`${model.provider}/${model.modelId}`] ?? "auto";
}

function sameProjectPath(left: string, right: string): boolean {
  const normalize = (value: string) => value.replace(/\\/g, "/").replace(/\/+$/, "");
  return normalize(left) === normalize(right);
}

export function useApplicationController(client: ApplicationClient): ApplicationController {
  const data = useMemo(() => applicationDataFor(client), [client]);
  const scopeOwner = useMemo(() => Symbol("workbench"), [client]);
  const imageCache = useMemo(() => new SessionImageCache((id, query, known) => client.readResource(id, "image", query, known), data.cache), [client, data]);
  const [historyWindow, setHistoryWindow] = useState<HistoryWindow | null>(null);
  const [toolProgress, setToolProgress] = useState<LiveSnapshot["toolsRunning"]>([]);
  const [status, setStatus] = useState<ControllerStatus>("connecting");
  const [error, setError] = useState("");
  const [snapshot, setSnapshot] = useState<ApplicationSnapshot | null>(null);
  const [models, setModels] = useState<ModelsView | null>(null);
  const [sessionView, setSessionView] = useState<WorkbenchSession | null>(null);
  const [sessionLoading, setSessionLoading] = useState(false);
  const [sessionRefreshing, setSessionRefreshing] = useState(false);
  const [runtimeState, setRuntimeState] = useState<SessionRuntimeState | null>(null);
  const [activeSessionId, setActiveSessionId] = useState<string | null>(null);
  const [newSessionCwd, setNewSessionCwd] = useState("");
  const [newSessionModel, setNewSessionModel] = useState<SelectedModel | null>(null);
  const [newSessionThinkingLevel, setNewSessionThinkingLevel] = useState("auto");
  const [toolPreset, setToolPresetState] = useState<ToolPreset>("default");
  const [slashCommands, setSlashCommands] = useState<SlashCommandInfo[]>([]);
  const [slashCommandsLoading, setSlashCommandsLoading] = useState(false);
  const [sessionStats, setSessionStats] = useState<SessionStatsInfo | null>(null);
  const [sessionStatsOpen, setSessionStatsOpen] = useState(false);
  const [messages, setMessages] = useState<AgentMessage[]>([]);
  const [streamingMessage, setStreamingMessage] = useState<AgentMessage | null>(null);
  const [latestGenerationSpeed, setLatestGenerationSpeed] = useState<AssistantGenerationSpeed | null>(null);
  const [busy, setBusy] = useState(false);

  const activeSessionRef = useRef<string | null>(null);
  const generationRef = useRef(0);
  const newSessionModelOverriddenRef = useRef(false);
  const newSessionThinkingOverriddenRef = useRef(false);
  const sessionStatsOpenRef = useRef(false);
  const activeGenerationTimingRef = useRef<ActiveGenerationTiming | null>(null);
  const modelsRequestRef = useRef(0);
  const modelsCwdRef = useRef("");
  const configurationRevisionRef = useRef(data.configurationVersion);
  const storeErrorRef = useRef("");

  const loadModels = useCallback(async (cwd: string, generation = generationRef.current) => {
    const request = ++modelsRequestRef.current;
    modelsCwdRef.current = cwd;
    const value = await data.cached("models", cwd, () => client.models(cwd));
    if (generation !== generationRef.current || request !== modelsRequestRef.current) return null;
    setModels(value);
    if (activeSessionRef.current === null) {
      const model = defaultModel(value);
      if (!newSessionModelOverriddenRef.current) setNewSessionModel(model);
      if (!newSessionThinkingOverriddenRef.current) {
        setNewSessionThinkingLevel(defaultThinkingLevel(value, model));
      }
    }
    return value;
  }, [client, data]);

  const loadTools = useCallback(async (sessionId: string, generation = generationRef.current) => {
    const tools = await data.cached("tools", sessionId, () => client.dispatch<ToolEntry[]>(sessionId, { type: "get_tools" }));
    if (generation !== generationRef.current || activeSessionRef.current !== sessionId) return;
    setToolPresetState(getPresetFromTools(tools ?? []));
  }, [client, data]);

  const loadSlashCommands = useCallback(async (
    sessionId: string,
    generation = generationRef.current,
  ) => {
    setSlashCommandsLoading(true);
    try {
      const result = await data.cached("commands", sessionId, () => client.dispatch<{ commands?: SlashCommandInfo[] }>(sessionId, { type: "get_commands" }));
      if (generation !== generationRef.current || activeSessionRef.current !== sessionId) return;
      setSlashCommands(result?.commands ?? []);
    } finally {
      if (generation === generationRef.current && activeSessionRef.current === sessionId) {
        setSlashCommandsLoading(false);
      }
    }
  }, [client, data]);

  const loadSessionStats = useCallback(async (
    sessionId: string,
    generation = generationRef.current,
  ) => {
    const value = await data.cached("stats", sessionId, () => client.dispatch<SessionStatsInfo>(sessionId, { type: "get_session_stats" }));
    if (generation !== generationRef.current || activeSessionRef.current !== sessionId) return null;
    setSessionStats(value);
    return value;
  }, [client, data]);

  const applyCachedSession = useCallback((sessionId: string): WorkbenchSession | null => {
    const cached = data.session(sessionId);
    if (!cached || activeSessionRef.current !== sessionId) return null;
    const { metadata, live, window } = cached;
    const generation = window.version.generation;
    imageCache.selectSession(sessionId, generation);
    const loaded: AgentMessage[] = [], entryIds: string[] = [];
    for (const page of window.pages) {
      for (const item of page.items) {
        const message = data.messageFor(sessionId, generation, item);
        if (message) { loaded.push(message); entryIds.push(item.node.id); }
      }
    }
    for (const message of cached.pending) {
      imageCache.rememberMessage(sessionId, message.id, message.message);
      if (window.following && (!message.entryId || !entryIds.includes(message.entryId))) {
        loaded.push({ ...message.message, id: message.id });
        entryIds.push(message.entryId ?? "");
      }
    }
    const state = live.state;
    const view: WorkbenchSession = {
      sessionId, filePath: metadata.filePath, info: metadata.info,
      leafId: live.active && live.history ? live.history.leafId : metadata.leafId,
      context: { messages: loaded, entryIds, thinkingLevel: state?.thinkingLevel ?? "off",
        model: state?.model ? { provider: state.model.provider, modelId: state.model.id } : null },
      running: live.active, state,
    };
    setSessionView(view);
    setHistoryWindow(window);
    setMessages(loaded);
    setStreamingMessage(window.following && live.streaming ? { ...live.streaming.message, id: live.streaming.id } : null);
    setToolProgress(window.following ? live.toolsRunning : []);
    setRuntimeState(state ?? null);
    setBusy(Boolean(state && (state.isPromptRunning || state.isStreaming || state.isBashRunning || state.isCompacting || state.retryWaiting)));
    return view;
  }, [data, imageCache]);

  const loadSession = useCallback(async (
    sessionId: string,
    generation = generationRef.current,
    _leafId = "",
    force = true,
  ) => {
    await data.loadSession(sessionId, force);
    if (generation !== generationRef.current || activeSessionRef.current !== sessionId) return null;
    return applyCachedSession(sessionId);
  }, [data, applyCachedSession]);

  const refreshSnapshot = useCallback(async (generation = generationRef.current) => {
    const value = await data.refreshOverview(true);
    if (generation !== generationRef.current) return null;
    setSnapshot(value);
    return value;
  }, [data]);

  const refreshTurnProjection = useCallback(async (sessionId: string, generation = generationRef.current) => {
    data.cache.invalidate(JSON.stringify(["configuration", "stats", sessionId]));
    await loadSessionStats(sessionId, generation);
  }, [data, loadSessionStats]);

  useEffect(() => data.observe(() => {
    setSnapshot(data.snapshot);
    const activeId = activeSessionRef.current;
    if (activeId && data.isMissing(activeId)) {
      activeSessionRef.current = null;
      setActiveSessionId(null);
      setSessionView(null); setRuntimeState(null); setHistoryWindow(null);
      setMessages([]); setStreamingMessage(null); setBusy(false); setSessionLoading(false);
      setSlashCommands([]); setSessionStats(null); setLatestGenerationSpeed(null);
      sessionStatsOpenRef.current = false; setSessionStatsOpen(false);
    } else if (activeId) applyCachedSession(activeId);
    const previousError = storeErrorRef.current;
    storeErrorRef.current = data.lastError;
    if (data.lastError) setError(data.lastError);
    else if (previousError) setError((current) => current === previousError ? "" : current);
    if (configurationRevisionRef.current !== data.configurationVersion) {
      configurationRevisionRef.current = data.configurationVersion;
      const id = activeSessionRef.current;
      const cwd = id ? data.session(id)?.metadata.info.cwd : modelsCwdRef.current;
      const work: Promise<unknown>[] = [];
      if (cwd) work.push(loadModels(cwd));
      if (id) work.push(loadTools(id), loadSlashCommands(id), loadSessionStats(id));
      void Promise.allSettled(work).then((results) => {
        const failure = results.find((result) => result.status === "rejected");
        if (failure?.status === "rejected" && activeSessionRef.current === id) setError(errorMessage(failure.reason));
      });
    }
  }), [data, applyCachedSession, loadModels, loadTools, loadSlashCommands, loadSessionStats]);

  const eventRefreshes = useMemo(() => new RefreshScheduler(async (requests) => {
    const generation = generationRef.current;
    const current = requests.filter((request) => request.generation === generation);
    const work: Promise<unknown>[] = [];
    if (current.some((request) => request.snapshot)) work.push(refreshSnapshot(generation));
    const sessionId = activeSessionRef.current;
    const sessionRequests = current.filter((request) => request.sessionId === sessionId);
    if (sessionId && sessionRequests.some((request) => request.session)) {
      work.push(loadSession(sessionId, generation), loadSessionStats(sessionId, generation));
    } else if (sessionId && sessionRequests.some((request) => request.turn)) {
      work.push(refreshTurnProjection(sessionId, generation));
    }
    const results = await Promise.allSettled(work);
    const failure = results.find((result) => result.status === "rejected");
    if (failure?.status === "rejected" && generation === generationRef.current && sessionId === activeSessionRef.current) {
      setError(errorMessage(failure.reason));
    }
  }), [loadSession, loadSessionStats, refreshSnapshot, refreshTurnProjection]);

  useEffect(() => {
    eventRefreshes.start();
    return () => eventRefreshes.dispose();
  }, [eventRefreshes]);

  const handleEvent = useCallback((envelope: ApplicationEventEnvelope) => {
    const event = envelope.event;
    if (event.type === "message_end" && typeof event.messageId === "string" && event.message && typeof event.message === "object") {
      imageCache.rememberMessage(envelope.sessionId, event.messageId, event.message as AgentMessage);
    }
    if (event.type === "message_receipt" && event.accepted && typeof event.messageId === "string" && typeof event.entryId === "string" && typeof event.generation === "string") {
      imageCache.associate(envelope.sessionId, event.messageId, event.entryId, event.generation);
    }
    if (envelope.sessionId !== activeSessionRef.current) return;
    if (event.type === "message_start" && (event.message as AgentMessage | undefined)?.role === "assistant") {
      activeGenerationTimingRef.current = { sessionId: envelope.sessionId, firstOutputAt: null };
    } else if (event.type === "message_update" && hasEffectiveAssistantDelta(event)) {
      const timing = activeGenerationTimingRef.current;
      if (!timing || timing.sessionId !== envelope.sessionId) activeGenerationTimingRef.current = { sessionId: envelope.sessionId, firstOutputAt: generationClockNow() };
      else if (timing.firstOutputAt === null) timing.firstOutputAt = generationClockNow();
    } else if (event.type === "message_end" && (event.message as AgentMessage | undefined)?.role === "assistant") {
      const message = event.message as AgentMessage, timing = activeGenerationTimingRef.current;
      activeGenerationTimingRef.current = null;
      const usage = assistantUsage(message), responseKey = assistantResponseKey(message);
      if (timing?.sessionId === envelope.sessionId && timing.firstOutputAt !== null && usage && responseKey && usage.output > 0) {
        const duration = generationClockNow() - timing.firstOutputAt;
        if (duration > 0) setLatestGenerationSpeed({ responseKey, tokensPerSecond: usage.output * 1000 / duration });
      }
    } else if (event.type === "turn_end" || event.type === "agent_settled") {
      eventRefreshes.request({ generation: generationRef.current, sessionId: envelope.sessionId, turn: true });
    } else if (event.type === "operation" && event.status === "failed") {
      setError(typeof event.errorMessage === "string" ? event.errorMessage : "指令执行失败");
    }
  }, [eventRefreshes, imageCache]);

  useEffect(() => data.observeEvents(handleEvent), [data, handleEvent]);

  const connect = useCallback(async () => {
    const generation = ++generationRef.current;
    data.stop();
    data.select(scopeOwner, null);
    activeSessionRef.current = null;
    newSessionModelOverriddenRef.current = false;
    newSessionThinkingOverriddenRef.current = false;
    setStatus("connecting");
    setError("");
    setSnapshot(null);
    setModels(null);
    setSessionView(null);
    setSessionLoading(false);
    setRuntimeState(null);
    setActiveSessionId(null);
    setNewSessionCwd("");
    setNewSessionModel(null);
    setNewSessionThinkingLevel("auto");
    setToolPresetState("default");
    setSlashCommands([]);
    setSlashCommandsLoading(false);
    setSessionStats(null);
    sessionStatsOpenRef.current = false;
    setSessionStatsOpen(false);
    setMessages([]);
    setStreamingMessage(null);
    activeGenerationTimingRef.current = null;
    setLatestGenerationSpeed(null);
    setBusy(false);
    try {
      const auth = await client.authStatus();
      if (generation !== generationRef.current) return;
      if (auth.authRequired && !auth.authenticated) {
        setStatus("auth");
        return;
      }
      await data.start();
      const value = data.snapshot;
      if (!value || generation !== generationRef.current) return;
      setSnapshot(value);
      setNewSessionCwd(value.defaultCwd);
      setStatus("ready");
      void loadModels(value.defaultCwd, generation).catch((modelsError) => {
        if (generation === generationRef.current) setError(errorMessage(modelsError));
      });
    } catch (connectError) {
      if (generation !== generationRef.current) return;
      setError(errorMessage(connectError));
      setStatus("error");
    }
  }, [client, data, scopeOwner, loadModels]);

  useEffect(() => {
    void connect();
    return () => {
      generationRef.current++;
      data.select(scopeOwner, null);
      data.stop();
    };
  }, [connect, data, scopeOwner]);

  const cancelConnection = useCallback(() => {
    generationRef.current++;
    data.stop();
    data.select(scopeOwner, null);
    client.close();
    setError("连接已停止");
    setStatus("error");
  }, [client, data]);

  const login = useCallback(async (password: string) => {
    const generation = generationRef.current;
    setError("");
    try {
      await client.login(password);
      if (generation !== generationRef.current) return;
      await connect();
    } catch (loginError) {
      if (generation !== generationRef.current) return;
      setError(errorMessage(loginError));
      throw loginError;
    }
  }, [client, connect]);

  const selectSession = useCallback(async (sessionId: string) => {
    const refreshing = activeSessionRef.current === sessionId;
    activeSessionRef.current = sessionId;
    data.select(scopeOwner, sessionId);
    setActiveSessionId(sessionId);
    const cached = applyCachedSession(sessionId);
    setSessionLoading(!cached);
    setSessionRefreshing(refreshing && Boolean(cached));
    if (!cached) { setSessionView(null); setRuntimeState(null); setHistoryWindow(null); setMessages([]); setStreamingMessage(null); }
    setSlashCommands([]);
    setSlashCommandsLoading(false);
    setSessionStats(null);
    sessionStatsOpenRef.current = false;
    setSessionStatsOpen(false);
    activeGenerationTimingRef.current = null;
    setLatestGenerationSpeed(null);
    setError("");
    try {
      if (refreshing) await data.refreshSession(sessionId);
      const view = await loadSession(sessionId, generationRef.current, "", false);
      if (view) {
        setSessionLoading(false);
        const capabilities = await Promise.allSettled([
          loadModels(view.info.cwd),
          loadTools(sessionId),
          loadSlashCommands(sessionId),
          loadSessionStats(sessionId),
        ]);
        if (activeSessionRef.current === sessionId) {
          // Capabilities may admit the Runtime. Validate its owner first, then
          // read the full metadata index once instead of downloading both generations.
          await data.loadSession(sessionId, true);
          await data.ensureNavigation(sessionId);
        }
        const failure = capabilities.find((result) => result.status === "rejected");
        if (failure?.status === "rejected" && activeSessionRef.current === sessionId) {
          setError(errorMessage(failure.reason));
        }
      }
    } catch (sessionError) {
      if (activeSessionRef.current === sessionId) setError(errorMessage(sessionError));
    } finally {
      if (activeSessionRef.current === sessionId) { setSessionLoading(false); setSessionRefreshing(false); }
    }
  }, [data, scopeOwner, applyCachedSession, loadModels, loadSession, loadSessionStats, loadSlashCommands, loadTools]);

  const beginNewSession = useCallback((cwd?: string) => {
    const nextCwd = cwd?.trim() || sessionView?.info.cwd || newSessionCwd || snapshot?.defaultCwd || "";
    activeSessionRef.current = null;
    data.select(scopeOwner, null);
    setHistoryWindow(null);
    newSessionModelOverriddenRef.current = false;
    newSessionThinkingOverriddenRef.current = false;
    setActiveSessionId(null);
    setSessionLoading(false);
    setSessionView(null);
    setRuntimeState(null);
    setSlashCommands([]);
    setSlashCommandsLoading(false);
    setSessionStats(null);
    sessionStatsOpenRef.current = false;
    setSessionStatsOpen(false);
    setNewSessionCwd(nextCwd);
    const model = defaultModel(models);
    setNewSessionModel(model);
    setNewSessionThinkingLevel(defaultThinkingLevel(models, model));
    setToolPresetState("default");
    setMessages([]);
    setStreamingMessage(null);
    activeGenerationTimingRef.current = null;
    setLatestGenerationSpeed(null);
    setBusy(false);
    setError("");
    if (nextCwd) {
      void loadModels(nextCwd).catch((modelsError) => {
        if (activeSessionRef.current === null) setError(errorMessage(modelsError));
      });
    }
  }, [data, scopeOwner, loadModels, models, newSessionCwd, sessionView?.info.cwd, snapshot?.defaultCwd]);

  const runCompact = useCallback(async (sessionId: string, customInstructions = "") => {
    setBusy(true);
    setRuntimeState((current) => ({ ...current, isCompacting: true }));
    try {
      await client.dispatch(sessionId, {
        type: "compact",
        ...(customInstructions ? { customInstructions } : {}),
      });
      await loadSession(sessionId);
    } finally {
      setBusy(false);
      setRuntimeState((current) => ({ ...current, isCompacting: false }));
    }
  }, [client, loadSession]);

  const reloadSessionResources = useCallback(async (sessionId: string) => {
    await client.dispatch(sessionId, { type: "reload" });
    data.invalidateConfiguration();
    const view = await loadSession(sessionId);
    if (!view) return;
    const capabilities = await Promise.allSettled([
      loadModels(view.info.cwd),
      loadTools(sessionId),
      loadSlashCommands(sessionId),
    ]);
    const failure = capabilities.find((result) => result.status === "rejected");
    if (failure?.status === "rejected") throw failure.reason;
  }, [client, data, loadModels, loadSession, loadSlashCommands, loadTools]);

  const handleBuiltinSlashCommand = useCallback(async (sessionId: string, text: string) => {
    const match = text.match(/^\/([^\s]+)(?:\s+([\s\S]*))?$/);
    if (!match) return false;
    const command = match[1]?.toLocaleLowerCase();
    const argument = (match[2] ?? "").trim();

    switch (command) {
      case "compact":
        await runCompact(sessionId, argument);
        return true;
      case "abort": {
        const type = runtimeState?.isBashRunning
          ? "abort_bash"
          : runtimeState?.isCompacting
            ? "abort_compaction"
            : "abort";
        await client.dispatch(sessionId, { type });
        return true;
      }
      case "clear-queue":
      case "dequeue":
        await client.dispatch(sessionId, { type: "clear_queue" });
        setRuntimeState((current) => ({ ...current, queuedMessages: emptyQueue }));
        return true;
      case "reload":
        await reloadSessionResources(sessionId);
        return true;
      case "name":
        if (!argument) throw new Error("用法：/name <名称>");
        await client.dispatch(sessionId, { type: "set_session_name", name: argument });
        await Promise.all([refreshSnapshot(), loadSession(sessionId)]);
        return true;
      case "stats":
      case "session":
        setSessionStats(null);
        sessionStatsOpenRef.current = true;
        setSessionStatsOpen(true);
        await loadSessionStats(sessionId);
        return true;
      case "copy": {
        const result = await client.dispatch<{ text?: string | null }>(sessionId, {
          type: "get_last_assistant_text",
        });
        if (!result?.text) throw new Error("还没有可复制的助手消息");
        if (!navigator.clipboard) throw new Error("当前环境不支持剪贴板写入");
        await navigator.clipboard.writeText(result.text);
        return true;
      }
      case "model": {
        const slash = argument.indexOf("/");
        if (slash <= 0 || slash === argument.length - 1) throw new Error("用法：/model <提供商/模型>");
        const provider = argument.slice(0, slash).trim();
        const modelId = argument.slice(slash + 1).trim();
        const available = models?.modelList.some((model) => model.provider === provider && model.id === modelId);
        if (!available) throw new Error(`未找到模型：${provider}/${modelId}`);
        await client.dispatch(sessionId, { type: "set_model", provider, modelId });
        setRuntimeState((current) => ({
          ...current,
          model: { id: modelId, provider, name: models?.models[`${provider}:${modelId}`] },
        }));
        await loadSession(sessionId);
        return true;
      }
      case "thinking":
        if (!argument) throw new Error("用法：/thinking <等级>");
        await client.dispatch(sessionId, { type: "set_thinking_level", level: argument });
        setRuntimeState((current) => ({ ...current, thinkingLevel: argument }));
        return true;
      case "tools": {
        if (argument !== "none" && argument !== "default" && argument !== "full") {
          throw new Error("用法：/tools <none|default|full>");
        }
        await client.dispatch(sessionId, { type: "set_tools", toolNames: getToolNamesForPreset(argument) });
        setToolPresetState(argument);
        return true;
      }
      default:
        return false;
    }
  }, [client, loadSession, loadSessionStats, models, refreshSnapshot, reloadSessionResources, runCompact, runtimeState?.isBashRunning, runtimeState?.isCompacting]);

  const send = useCallback(async (
    rawText: string,
    behavior: SendBehavior = "prompt",
    images: ImageAttachment[] = [],
  ) => {
    const text = rawText.trim();
    if (!text || !snapshot) return;
    setError("");
    let sessionID = activeSessionRef.current;
    let createdSession = false;
    try {
      if (!sessionID) {
        if (behavior !== "prompt") return;
        const created = await client.createSession({
          cwd: newSessionCwd || snapshot.defaultCwd,
          ...(newSessionModel ? {
            provider: newSessionModel.provider,
            modelId: newSessionModel.modelId,
          } : {}),
          ...(newSessionThinkingLevel !== "auto" ? {
            thinkingLevel: newSessionThinkingLevel,
          } : {}),
          toolNames: getToolNamesForPreset(toolPreset),
        });
        sessionID = created.sessionId;
        createdSession = true;
        activeSessionRef.current = sessionID;
        data.select(scopeOwner, sessionID);
        setActiveSessionId(sessionID);
        setRuntimeState({
          sessionId: sessionID,
          thinkingLevel: created.thinkingLevel,
          model: created.model ? {
            id: created.model.modelId,
            provider: created.model.provider,
          } : undefined,
          queuedMessages: emptyQueue,
        });
        await refreshSnapshot();
      }

      if (behavior === "steer" || behavior === "follow_up") {
        await client.dispatch(sessionID, {
          type: behavior, message: text,
          ...(images.length ? { images: images.map((image) => ({ type: "image", data: image.data, mimeType: image.mimeType })) } : {}),
        });
        await loadSession(sessionID);
        return;
      }

      if (
        !busy
        && behavior === "prompt"
        && images.length === 0
        && await handleBuiltinSlashCommand(sessionID, text)
      ) {
        return;
      }

      const bashExcluded = images.length === 0 && text.startsWith("!!");
      const bashCommand = images.length === 0 && text.startsWith("!")
        ? text.slice(bashExcluded ? 2 : 1).trim()
        : "";
      if (!busy && bashCommand) {
        setBusy(true);
        setRuntimeState((current) => ({ ...current, isBashRunning: true }));
        try {
          await client.dispatch(sessionID, {
            type: "bash",
            command: bashCommand,
            excludeFromContext: bashExcluded,
          });
          await loadSession(sessionID);
        } finally {
          setBusy(false);
          setRuntimeState((current) => ({ ...current, isBashRunning: false }));
        }
        return;
      }

      if (busy) {
        if (images.length) throw new Error("Agent 运行期间不能把图片加入队列");
        await client.dispatch(sessionID, {
          type: "prompt",
          message: text,
          streamingBehavior: "steer",
        });
        return;
      }

      setBusy(true);
      setRuntimeState((current) => ({ ...current, isPromptRunning: true }));
      await client.dispatch(sessionID, {
        type: "prompt",
        message: text,
        ...(images.length ? {
          images: images.map((image) => ({
            type: "image",
            data: image.data,
            mimeType: image.mimeType,
          })),
        } : {}),
      });
      if (createdSession) {
        void loadSlashCommands(sessionID).catch(() => {
          if (activeSessionRef.current === sessionID) setSlashCommands([]);
        });
      }
    } catch (sendError) {
      if (!busy) setBusy(false);
      setError(errorMessage(sendError));
      throw sendError;
    }
  }, [
    busy,
    client,
    newSessionModel,
    newSessionCwd,
    newSessionThinkingLevel,
    handleBuiltinSlashCommand,
    loadSession,
    loadSlashCommands,
    refreshSnapshot,
    snapshot,
    toolPreset,
    data,
    scopeOwner,
  ]);

  const abort = useCallback(async () => {
    const sessionID = activeSessionRef.current;
    if (!sessionID) return;
    setError("");
    try {
      const type = runtimeState?.isBashRunning
        ? "abort_bash"
        : runtimeState?.isCompacting
          ? "abort_compaction"
          : "abort";
      await client.dispatch(sessionID, { type });
    } catch (abortError) {
      setError(errorMessage(abortError));
      throw abortError;
    }
  }, [client, runtimeState?.isBashRunning, runtimeState?.isCompacting]);

  const clearQueue = useCallback(async () => {
    const sessionID = activeSessionRef.current;
    if (!sessionID) return [];
    setError("");
    try {
      const queue = await client.dispatch<QueuedMessages>(sessionID, { type: "clear_queue" });
      setRuntimeState((current) => ({ ...current, queuedMessages: emptyQueue }));
      data.invalidateSession(sessionID, false);
      await data.loadSession(sessionID);
      return [...(queue?.steering ?? []), ...(queue?.followUp ?? [])];
    } catch (queueError) {
      setError(errorMessage(queueError));
      throw queueError;
    }
  }, [client, data]);

  const changeModel = useCallback(async (model: SelectedModel) => {
    setError("");
    const sessionID = activeSessionRef.current;
    if (!sessionID) {
      newSessionModelOverriddenRef.current = true;
      setNewSessionModel(model);
      if (!newSessionThinkingOverriddenRef.current) {
        setNewSessionThinkingLevel(defaultThinkingLevel(models, model));
      }
      return;
    }
    try {
      await client.dispatch(sessionID, {
        type: "set_model",
        provider: model.provider,
        modelId: model.modelId,
      });
      const modelName = models?.models[`${model.provider}:${model.modelId}`];
      setRuntimeState((current) => ({
        ...current,
        model: { id: model.modelId, provider: model.provider, name: modelName },
      }));
      await loadSession(sessionID);
    } catch (modelError) {
      setError(errorMessage(modelError));
      throw modelError;
    }
  }, [client, loadSession, models]);

  const changeThinkingLevel = useCallback(async (level: string) => {
    setError("");
    const sessionID = activeSessionRef.current;
    if (!sessionID) {
      newSessionThinkingOverriddenRef.current = true;
      setNewSessionThinkingLevel(level);
      return;
    }
    if (level === "auto") return;
    try {
      await client.dispatch(sessionID, { type: "set_thinking_level", level });
      setRuntimeState((current) => ({ ...current, thinkingLevel: level }));
    } catch (thinkingError) {
      setError(errorMessage(thinkingError));
      throw thinkingError;
    }
  }, [client, data]);

  const changeToolPreset = useCallback(async (preset: ToolPreset) => {
    setError("");
    const sessionID = activeSessionRef.current;
    setToolPresetState(preset);
    if (!sessionID) return;
    try {
      await client.dispatch(sessionID, {
        type: "set_tools",
        toolNames: getToolNamesForPreset(preset),
      });
    } catch (toolsError) {
      setError(errorMessage(toolsError));
      throw toolsError;
    }
  }, [client, data]);

  const browseDirectories = useCallback((path = "") => {
    return client.browseDirectories(path);
  }, [client, data]);

  const addProject = useCallback(async (path: string) => {
    setError("");
    try {
      const project = await client.addProject(path);
      await refreshSnapshot();
      return project;
    } catch (projectError) {
      setError(errorMessage(projectError));
      throw projectError;
    }
  }, [client, refreshSnapshot]);

  const removeProject = useCallback(async (path: string) => {
    setError("");
    try {
      await client.removeProject(path);
      const value = await refreshSnapshot();
      if (!activeSessionRef.current && sameProjectPath(newSessionCwd, path)) {
        const fallback = value?.projects[0]?.path || value?.defaultCwd || "";
        setNewSessionCwd(fallback);
        if (fallback) await loadModels(fallback);
      }
    } catch (projectError) {
      setError(errorMessage(projectError));
      throw projectError;
    }
  }, [client, loadModels, newSessionCwd, refreshSnapshot]);

  const changeWorkingDirectory = useCallback(async (path: string) => {
    if (activeSessionRef.current) return;
    setError("");
    setNewSessionCwd(path);
    try {
      await loadModels(path);
    } catch (directoryError) {
      setError(errorMessage(directoryError));
      throw directoryError;
    }
  }, [loadModels]);

  const compact = useCallback(async (customInstructions?: string) => {
    const sessionID = activeSessionRef.current;
    if (!sessionID) return;
    setError("");
    try {
      await runCompact(sessionID, customInstructions?.trim());
    } catch (compactError) {
      setError(errorMessage(compactError));
      throw compactError;
    }
  }, [runCompact]);

  const reload = useCallback(async () => {
    const sessionID = activeSessionRef.current;
    if (!sessionID) return;
    setError("");
    try {
      await reloadSessionResources(sessionID);
    } catch (reloadError) {
      setError(errorMessage(reloadError));
      throw reloadError;
    }
  }, [reloadSessionResources]);

  const copyLastAssistant = useCallback(async () => {
    const sessionID = activeSessionRef.current;
    if (!sessionID) return;
    setError("");
    try {
      const result = await client.dispatch<{ text?: string | null }>(sessionID, {
        type: "get_last_assistant_text",
      });
      if (!result?.text) throw new Error("还没有可复制的助手消息");
      if (!navigator.clipboard?.writeText) throw new Error("当前环境不支持剪贴板写入");
      await navigator.clipboard.writeText(result.text);
    } catch (copyError) {
      setError(errorMessage(copyError));
      throw copyError;
    }
  }, [client, data]);

  const forkAt = useCallback(async (entryId: string, position: "at" | "before") => {
    const sessionID = activeSessionRef.current;
    if (!sessionID || !entryId) return;
    setError("");
    try {
      const result = await client.dispatch<{
        cancelled?: boolean;
        newSessionId?: string;
      }>(sessionID, {
        type: "fork",
        entryId,
        position,
      });
      if (!result?.newSessionId || result.cancelled) return;
      await refreshSnapshot();
      await selectSession(result.newSessionId);
    } catch (forkError) {
      setError(errorMessage(forkError));
      throw forkError;
    }
  }, [client, refreshSnapshot, selectSession]);

  const fork = useCallback((entryId: string) => forkAt(entryId, "at"), [forkAt]);
  const forkBefore = useCallback((entryId: string) => forkAt(entryId, "before"), [forkAt]);

  const clone = useCallback(async () => {
    const leafId = sessionView?.leafId;
    if (!leafId) throw new Error("当前会话还没有可克隆的位置");
    await forkAt(leafId, "at");
  }, [forkAt, sessionView?.leafId]);

  const navigateTree = useCallback(async (entryId: string) => {
    const sessionID = activeSessionRef.current;
    if (!sessionID || !entryId || runtimeState?.isBashRunning) return;
    setError("");
    try {
      const result = await client.dispatch<{ cancelled?: boolean }>(sessionID, {
        type: "navigate_tree",
        targetId: entryId,
      });
      if (result?.cancelled) return;
      await Promise.all([
        loadSession(sessionID, generationRef.current, entryId),
        loadSessionStats(sessionID),
      ]);
      await data.pageHistory(sessionID, "latest");
    } catch (navigateError) {
      setError(errorMessage(navigateError));
      throw navigateError;
    }
  }, [client, data, loadSession, loadSessionStats, runtimeState?.isBashRunning]);

  const editAndResend = useCallback(async (entryId: string, text: string): Promise<void> => {
    const sessionID = activeSessionRef.current;
    if (!sessionID || !entryId || busy || runtimeState?.isBashRunning) return;
    setError("");
    setBusy(true);
    setRuntimeState((current) => ({ ...current, isPromptRunning: true }));
    try {
      await client.dispatch(sessionID, {
        type: "edit_and_resend",
        entryId,
        message: text,
      });
    } catch (editError) {
      setBusy(false);
      setRuntimeState((current) => ({ ...current, isPromptRunning: false, isStreaming: false }));
      await Promise.allSettled([
        loadSession(sessionID),
        loadSessionStats(sessionID),
      ]);
      setError(errorMessage(editError));
      throw editError;
    }
    await Promise.allSettled([
      loadSession(sessionID),
      loadSessionStats(sessionID),
    ]);
    await data.pageHistory(sessionID, "latest");
  }, [busy, client, data, loadSession, loadSessionStats, runtimeState?.isBashRunning]);

  const openSessionStats = useCallback(async () => {
    const sessionID = activeSessionRef.current;
    if (!sessionID) return;
    setError("");
    sessionStatsOpenRef.current = true;
    setSessionStatsOpen(true);
    try {
      await loadSessionStats(sessionID);
    } catch (statsError) {
      setError(errorMessage(statsError));
      throw statsError;
    }
  }, [loadSessionStats]);

  const closeSessionStats = useCallback(() => {
    sessionStatsOpenRef.current = false;
    setSessionStatsOpen(false);
  }, []);

  const renameSession = useCallback(async (sessionId: string, rawName: string) => {
    const name = rawName.trim();
    if (!name) return;
    setError("");
    try {
      await client.renameSession(sessionId, name);
      await refreshSnapshot();
      if (activeSessionRef.current === sessionId) await loadSession(sessionId);
    } catch (renameError) {
      setError(errorMessage(renameError));
      throw renameError;
    }
  }, [client, loadSession, refreshSnapshot]);

  const deleteSession = useCallback(async (sessionId: string) => {
    setError("");
    try {
      await client.deleteSession(sessionId);
      if (activeSessionRef.current === sessionId) beginNewSession();
      await refreshSnapshot();
    } catch (deleteError) {
      setError(errorMessage(deleteError));
      throw deleteError;
    }
  }, [beginNewSession, client, refreshSnapshot]);

  const selectedModel = activeSessionId
    ? runtimeState?.model
      ? { provider: runtimeState.model.provider, modelId: runtimeState.model.id }
      : sessionView?.context.model ?? null
    : newSessionModel;
  const thinkingLevel = activeSessionId
    ? runtimeState?.thinkingLevel ?? sessionView?.context.thinkingLevel ?? "off"
    : newSessionThinkingLevel;
  const queuedMessages = runtimeState?.queuedMessages ?? emptyQueue;
  const pendingMessages: AgentMessage[] = activeSessionId
    ? [...queuedMessages.steering, ...queuedMessages.followUp].map((text, index) => ({ id: `queue-${index}`, role: "user", content: text, pending: true }))
    : [];
  const loadHistory = useCallback(async (direction: "older" | "newer" | "latest", aroundId?: string) => {
    const id = activeSessionRef.current;
    if (!id) return;
    try { await data.pageHistory(id, direction, aroundId); }
    catch (error) { setError(errorMessage(error)); throw error; }
  }, [data]);
  const loadNodes = useCallback((query: ReadResourceRequest = {}) => {
    const id = activeSessionRef.current;
    if (!id) return Promise.reject(new Error("请选择会话"));
    return data.nodes(id, query);
  }, [data]);
  const loadDirectory = useCallback(async (project: string, search = "", more = false, force = false) => {
    try { return await data.loadDirectory(project, search, more, force); }
    catch (error) { setError(errorMessage(error)); throw error; }
  }, [data]);

  const loadProjects = useCallback(async () => {
    try { await data.loadProjects(); }
    catch (error) { setError(errorMessage(error)); throw error; }
  }, [data]);
  const directory = useCallback((project: string, search = "") => data.directory(project, search), [data]);
  const watchDirectory = useCallback((project: string, search = "") => data.watchDirectory(project, search), [data]);

  return {
    data,
    historyWindow,
    toolProgress,
    loadHistory,
    loadNodes,
    loadDirectory,
    hasMoreProjects: data.hasMoreProjects,
    loadProjects,
    directory,
    watchDirectory,
    imageCache,
    status,
    error,
    snapshot,
    models,
    sessionView,
    sessionLoading,
    sessionRefreshing,
    runtimeState,
    activeSessionId,
    workingDirectory: activeSessionId
      ? sessionView?.info.cwd ?? runtimeState?.cwd ?? ""
      : newSessionCwd,
    selectedModel,
    thinkingLevel,
    toolPreset,
    queuedMessages,
    slashCommands,
    slashCommandsLoading,
    sessionStats,
    sessionStatsOpen,
    messages,
    pendingMessages,
    streamingMessage,
    latestGenerationSpeed,
    latestUsageInfo: activeSessionId ? data.latestUsage(activeSessionId) : null,
    busy,
    login,
    selectSession,
    beginNewSession,
    addProject,
    removeProject,
    send,
    abort,
    clearQueue,
    setModel: changeModel,
    setThinkingLevel: changeThinkingLevel,
    setToolPreset: changeToolPreset,
    browseDirectories,
    setWorkingDirectory: changeWorkingDirectory,
    compact,
    reload,
    copyLastAssistant,
    fork,
    forkBefore,
    editAndResend,
    clone,
    navigateTree,
    openSessionStats,
    closeSessionStats,
    renameSession,
    deleteSession,
    cancelConnection,
    retry: connect,
  };
}
