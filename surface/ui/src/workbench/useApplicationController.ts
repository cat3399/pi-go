import { useCallback, useEffect, useRef, useState } from "react";
import type {
  AgentMessage,
  ApplicationClient,
  ApplicationEventEnvelope,
  ApplicationSnapshot,
  EventSubscription,
  DirectoryView,
  ImageAttachment,
  ModelsView,
  QueuedMessages,
  SelectedModel,
  SessionStatsInfo,
  SessionRuntimeState,
  SessionView,
  SlashCommandInfo,
} from "../contracts";
import {
  getPresetFromTools,
  getToolNamesForPreset,
  type ToolEntry,
  type ToolPreset,
} from "../tool-presets";
import { messageText, sameUserText } from "./message";

type ControllerStatus = "connecting" | "auth" | "ready" | "error";
export type SendBehavior = "prompt" | "steer" | "follow_up";

export interface ApplicationController {
  status: ControllerStatus;
  error: string;
  snapshot: ApplicationSnapshot | null;
  models: ModelsView | null;
  sessionView: SessionView | null;
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
  streamingMessage: AgentMessage | null;
  busy: boolean;
  login(password: string): Promise<void>;
  selectSession(sessionId: string): Promise<void>;
  beginNewSession(cwd?: string): void;
  send(text: string, behavior?: SendBehavior, images?: ImageAttachment[]): Promise<void>;
  abort(): Promise<void>;
  clearQueue(): Promise<string[]>;
  setModel(model: SelectedModel): Promise<void>;
  setThinkingLevel(level: string): Promise<void>;
  setToolPreset(preset: ToolPreset): Promise<void>;
  browseDirectories(path?: string): Promise<DirectoryView>;
  setWorkingDirectory(path: string): Promise<void>;
  compact(): Promise<void>;
  fork(entryId: string): Promise<void>;
  navigateTree(entryId: string): Promise<void>;
  openSessionStats(): Promise<void>;
  closeSessionStats(): void;
  renameSession(sessionId: string, name: string): Promise<void>;
  deleteSession(sessionId: string): Promise<void>;
  retry(): Promise<void>;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

const emptyQueue: QueuedMessages = { steering: [], followUp: [] };

function defaultModel(models: ModelsView | null): SelectedModel | null {
  if (models?.defaultModel) return models.defaultModel;
  const first = models?.modelList[0];
  return first ? { provider: first.provider, modelId: first.id } : null;
}

function defaultThinkingLevel(models: ModelsView | null, model: SelectedModel | null): string {
  if (!models || !model) return "auto";
  return models.thinkingLevelPins[`${model.provider}/${model.modelId}`] ?? "auto";
}

export function useApplicationController(client: ApplicationClient): ApplicationController {
  const [status, setStatus] = useState<ControllerStatus>("connecting");
  const [error, setError] = useState("");
  const [snapshot, setSnapshot] = useState<ApplicationSnapshot | null>(null);
  const [models, setModels] = useState<ModelsView | null>(null);
  const [sessionView, setSessionView] = useState<SessionView | null>(null);
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
  const [busy, setBusy] = useState(false);

  const activeSessionRef = useRef<string | null>(null);
  const revisionRef = useRef(0);
  const subscriptionRef = useRef<EventSubscription | null>(null);
  const generationRef = useRef(0);
  const subscribeRef = useRef<(after: number, generation: number) => void>(() => undefined);
  const pendingSessionTitlesRef = useRef(new Map<string, string>());
  const newSessionModelOverriddenRef = useRef(false);
  const newSessionThinkingOverriddenRef = useRef(false);
  const sessionStatsOpenRef = useRef(false);

  const loadModels = useCallback(async (cwd: string, generation = generationRef.current) => {
    const value = await client.models(cwd);
    if (generation !== generationRef.current) return null;
    setModels(value);
    if (activeSessionRef.current === null) {
      const model = defaultModel(value);
      if (!newSessionModelOverriddenRef.current) setNewSessionModel(model);
      if (!newSessionThinkingOverriddenRef.current) {
        setNewSessionThinkingLevel(defaultThinkingLevel(value, model));
      }
    }
    return value;
  }, [client]);

  const loadTools = useCallback(async (sessionId: string, generation = generationRef.current) => {
    const tools = await client.dispatch<ToolEntry[]>(sessionId, { type: "get_tools" });
    if (generation !== generationRef.current || activeSessionRef.current !== sessionId) return;
    setToolPresetState(getPresetFromTools(tools ?? []));
  }, [client]);

  const loadSlashCommands = useCallback(async (
    sessionId: string,
    generation = generationRef.current,
  ) => {
    setSlashCommandsLoading(true);
    try {
      const result = await client.dispatch<{ commands?: SlashCommandInfo[] }>(sessionId, {
        type: "get_commands",
      });
      if (generation !== generationRef.current || activeSessionRef.current !== sessionId) return;
      setSlashCommands(result?.commands ?? []);
    } finally {
      if (generation === generationRef.current && activeSessionRef.current === sessionId) {
        setSlashCommandsLoading(false);
      }
    }
  }, [client]);

  const loadSessionStats = useCallback(async (
    sessionId: string,
    generation = generationRef.current,
  ) => {
    const value = await client.dispatch<SessionStatsInfo>(sessionId, { type: "get_session_stats" });
    if (generation !== generationRef.current || activeSessionRef.current !== sessionId) return null;
    setSessionStats(value);
    return value;
  }, [client]);

  const loadSession = useCallback(async (
    sessionId: string,
    generation = generationRef.current,
    leafId = "",
  ) => {
    const view = await client.sessionView(sessionId, leafId);
    if (generation !== generationRef.current || activeSessionRef.current !== sessionId) return null;
    revisionRef.current = Math.max(revisionRef.current, view.revision);
    setSessionView(view);
    setMessages(view.context.messages ?? []);
    setStreamingMessage(null);
    const state = view.state ?? null;
    setRuntimeState(state);
    setBusy(Boolean(view.running && state && (
      state.isPromptRunning || state.isStreaming || state.isBashRunning || state.isCompacting
    )));
    return view;
  }, [client]);

  const refreshSnapshot = useCallback(async (generation = generationRef.current) => {
    const value = await client.snapshot();
    if (generation !== generationRef.current) return null;
    revisionRef.current = Math.max(revisionRef.current, value.revision);
    const projected = {
      ...value,
      sessions: value.sessions.map((session) => {
        const pendingTitle = pendingSessionTitlesRef.current.get(session.id);
        if (!pendingTitle) return session;
        if (session.firstMessage && session.firstMessage !== "(no messages)") {
          pendingSessionTitlesRef.current.delete(session.id);
          return session;
        }
        return {
          ...session,
          firstMessage: pendingTitle,
          messageCount: Math.max(1, session.messageCount),
        };
      }),
    };
    setSnapshot(projected);
    return projected;
  }, [client]);

  const handleEvent = useCallback((envelope: ApplicationEventEnvelope) => {
    revisionRef.current = Math.max(revisionRef.current, envelope.sequence);
    const event = envelope.event;
    if (event.type === "session_catalog") {
      void refreshSnapshot();
    }
    if (envelope.sessionId !== activeSessionRef.current) return;

    switch (event.type) {
      case "agent_start":
        setBusy(true);
        setRuntimeState((current) => ({ ...current, isPromptRunning: true }));
        break;
      case "message_start":
      case "message_update": {
        const message = event.message;
        if (message && typeof message === "object" && (message as AgentMessage).role !== "user") {
          setStreamingMessage(message as AgentMessage);
        }
        break;
      }
      case "message_end": {
        const completed = event.message;
        if (!completed || typeof completed !== "object") break;
        const message = completed as AgentMessage;
        setMessages((current) => {
          if (message.role === "user" && sameUserText(current[current.length - 1], messageText(message))) {
            return current;
          }
          return [...current, message];
        });
        setStreamingMessage(null);
        break;
      }
      case "agent_end":
        setStreamingMessage(null);
        break;
      case "agent_settled":
        setBusy(false);
        setRuntimeState((current) => ({
          ...current,
          isPromptRunning: false,
          isStreaming: false,
          isCompacting: false,
          retryWaiting: false,
        }));
        void refreshSnapshot();
        if (activeSessionRef.current) {
          void Promise.allSettled([
            loadSession(activeSessionRef.current),
            loadSessionStats(activeSessionRef.current),
          ]);
        }
        break;
      case "queue_update":
        setRuntimeState((current) => ({
          ...current,
          queuedMessages: {
            steering: Array.isArray(event.steering) ? event.steering.filter((value): value is string => typeof value === "string") : [],
            followUp: Array.isArray(event.followUp) ? event.followUp.filter((value): value is string => typeof value === "string") : [],
          },
        }));
        break;
      case "thinking_level_changed":
        if (typeof event.level === "string") {
          setRuntimeState((current) => ({ ...current, thinkingLevel: event.level as string }));
        }
        break;
      case "compaction_start":
        setBusy(true);
        setRuntimeState((current) => ({ ...current, isCompacting: true }));
        break;
      case "compaction_end":
        setRuntimeState((current) => ({ ...current, isCompacting: false }));
        break;
      case "auto_retry_start":
        setRuntimeState((current) => ({
          ...current,
          retryWaiting: true,
          retryAttempt: typeof event.attempt === "number" ? event.attempt : current?.retryAttempt,
        }));
        break;
      case "auto_retry_end":
        setRuntimeState((current) => ({ ...current, retryWaiting: false }));
        break;
      case "session_info_changed":
        void refreshSnapshot();
        break;
      case "operation":
        if (event.command === "prompt" && event.status === "failed") {
          setBusy(false);
          setRuntimeState((current) => ({ ...current, isPromptRunning: false, isStreaming: false }));
          setError(typeof event.errorMessage === "string" ? event.errorMessage : "指令执行失败");
        }
        break;
      default:
        break;
    }
  }, [loadSession, loadSessionStats, refreshSnapshot]);

  const subscribe = useCallback((after: number, generation: number) => {
    subscriptionRef.current?.close();
    const subscription = client.subscribe(after, {
      onEvent: (envelope) => {
        if (generation === generationRef.current) handleEvent(envelope);
      },
      onReset: (revision, reason) => {
        if (generation !== generationRef.current) return;
        revisionRef.current = revision;
        if (reason) setError(reason);
        void (async () => {
          try {
            const value = await refreshSnapshot(generation);
            if (!value || generation !== generationRef.current) return;
            const sessionID = activeSessionRef.current;
            if (sessionID) await loadSession(sessionID, generation);
            if (generation !== generationRef.current) return;
            setError("");
            subscribeRef.current(value.revision, generation);
          } catch (resetError) {
            if (generation === generationRef.current) setError(errorMessage(resetError));
          }
        })();
      },
    });
    subscriptionRef.current = subscription;
    void subscription.ready.catch((eventError) => {
      if (generation !== generationRef.current) return;
      setError(errorMessage(eventError));
    });
  }, [client, handleEvent, loadSession, refreshSnapshot]);
  subscribeRef.current = subscribe;

  const connect = useCallback(async () => {
    const generation = ++generationRef.current;
    subscriptionRef.current?.close();
    subscriptionRef.current = null;
    activeSessionRef.current = null;
    revisionRef.current = 0;
    pendingSessionTitlesRef.current.clear();
    newSessionModelOverriddenRef.current = false;
    newSessionThinkingOverriddenRef.current = false;
    setStatus("connecting");
    setError("");
    setSnapshot(null);
    setModels(null);
    setSessionView(null);
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
    setBusy(false);
    try {
      const auth = await client.authStatus();
      if (generation !== generationRef.current) return;
      if (auth.authRequired && !auth.authenticated) {
        setStatus("auth");
        return;
      }
      const value = await refreshSnapshot(generation);
      if (!value || generation !== generationRef.current) return;
      setNewSessionCwd(value.defaultCwd);
      subscribe(value.revision, generation);
      setStatus("ready");
      void loadModels(value.defaultCwd, generation).catch((modelsError) => {
        if (generation === generationRef.current) setError(errorMessage(modelsError));
      });
    } catch (connectError) {
      if (generation !== generationRef.current) return;
      setError(errorMessage(connectError));
      setStatus("error");
    }
  }, [client, loadModels, refreshSnapshot, subscribe]);

  useEffect(() => {
    void connect();
    return () => {
      generationRef.current++;
      subscriptionRef.current?.close();
      subscriptionRef.current = null;
    };
  }, [connect]);

  const login = useCallback(async (password: string) => {
    setError("");
    try {
      await client.login(password);
      await connect();
    } catch (loginError) {
      setError(errorMessage(loginError));
      throw loginError;
    }
  }, [client, connect]);

  const selectSession = useCallback(async (sessionId: string) => {
    activeSessionRef.current = sessionId;
    setActiveSessionId(sessionId);
    setSessionView(null);
    setRuntimeState(null);
    setSlashCommands([]);
    setSlashCommandsLoading(false);
    setSessionStats(null);
    sessionStatsOpenRef.current = false;
    setSessionStatsOpen(false);
    setMessages([]);
    setStreamingMessage(null);
    setError("");
    try {
      const view = await loadSession(sessionId);
      if (view) {
        const capabilities = await Promise.allSettled([
          loadModels(view.info.cwd),
          loadTools(sessionId),
          loadSlashCommands(sessionId),
          loadSessionStats(sessionId),
        ]);
        const failure = capabilities.find((result) => result.status === "rejected");
        if (failure?.status === "rejected" && activeSessionRef.current === sessionId) {
          setError(errorMessage(failure.reason));
        }
      }
    } catch (sessionError) {
      if (activeSessionRef.current === sessionId) setError(errorMessage(sessionError));
    }
  }, [loadModels, loadSession, loadSessionStats, loadSlashCommands, loadTools]);

  const beginNewSession = useCallback((cwd?: string) => {
    const nextCwd = cwd?.trim() || sessionView?.info.cwd || newSessionCwd || snapshot?.defaultCwd || "";
    activeSessionRef.current = null;
    newSessionModelOverriddenRef.current = false;
    newSessionThinkingOverriddenRef.current = false;
    setActiveSessionId(null);
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
    setBusy(false);
    setError("");
  }, [models, newSessionCwd, sessionView?.info.cwd, snapshot?.defaultCwd]);

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
    const view = await loadSession(sessionId);
    if (!view) return;
    const capabilities = await Promise.allSettled([
      loadModels(view.info.cwd),
      loadTools(sessionId),
      loadSlashCommands(sessionId),
    ]);
    const failure = capabilities.find((result) => result.status === "rejected");
    if (failure?.status === "rejected") throw failure.reason;
  }, [client, loadModels, loadSession, loadSlashCommands, loadTools]);

  const handleBuiltinSlashCommand = useCallback(async (sessionId: string, text: string) => {
    const match = text.match(/^\/([^\s]+)(?:\s+([\s\S]*))?$/);
    if (!match) return false;
    const command = match[1]?.toLocaleLowerCase();
    const argument = (match[2] ?? "").trim();

    switch (command) {
      case "compact":
        await runCompact(sessionId, argument);
        return true;
      case "reload":
        await reloadSessionResources(sessionId);
        return true;
      case "name":
        if (!argument) throw new Error("用法：/name <名称>");
        await client.dispatch(sessionId, { type: "set_session_name", name: argument });
        await Promise.all([refreshSnapshot(), loadSession(sessionId)]);
        return true;
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
      default:
        return false;
    }
  }, [client, loadSession, loadSessionStats, refreshSnapshot, reloadSessionResources, runCompact]);

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
          type: behavior,
          message: text,
          ...(images.length ? {
            images: images.map((image) => ({
              type: "image",
              data: image.data,
              mimeType: image.mimeType,
            })),
          } : {}),
        });
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
        if (createdSession) {
          pendingSessionTitlesRef.current.set(sessionID, text);
          await refreshSnapshot();
        }
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

      if (createdSession) {
        pendingSessionTitlesRef.current.set(sessionID, text);
        await refreshSnapshot();
      }
      setMessages((current) => [...current, { role: "user", content: text, timestamp: Date.now() }]);
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
      return [...(queue?.steering ?? []), ...(queue?.followUp ?? [])];
    } catch (queueError) {
      setError(errorMessage(queueError));
      throw queueError;
    }
  }, [client]);

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
  }, [client]);

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
  }, [client]);

  const browseDirectories = useCallback((path = "") => {
    return client.browseDirectories(path);
  }, [client]);

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

  const compact = useCallback(async () => {
    const sessionID = activeSessionRef.current;
    if (!sessionID) return;
    setError("");
    try {
      await runCompact(sessionID);
    } catch (compactError) {
      setError(errorMessage(compactError));
      throw compactError;
    }
  }, [runCompact]);

  const fork = useCallback(async (entryId: string) => {
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
        position: "at",
      });
      if (!result?.newSessionId || result.cancelled) return;
      await refreshSnapshot();
      await selectSession(result.newSessionId);
    } catch (forkError) {
      setError(errorMessage(forkError));
      throw forkError;
    }
  }, [client, refreshSnapshot, selectSession]);

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
    } catch (navigateError) {
      setError(errorMessage(navigateError));
      throw navigateError;
    }
  }, [client, loadSession, loadSessionStats, runtimeState?.isBashRunning]);

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

  return {
    status,
    error,
    snapshot,
    models,
    sessionView,
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
    streamingMessage,
    busy,
    login,
    selectSession,
    beginNewSession,
    send,
    abort,
    clearQueue,
    setModel: changeModel,
    setThinkingLevel: changeThinkingLevel,
    setToolPreset: changeToolPreset,
    browseDirectories,
    setWorkingDirectory: changeWorkingDirectory,
    compact,
    fork,
    navigateTree,
    openSessionStats,
    closeSessionStats,
    renameSession,
    deleteSession,
    retry: connect,
  };
}
