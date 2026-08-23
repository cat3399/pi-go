import { type CSSProperties, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Folder, LoaderCircle, PanelLeft, Settings } from "lucide-react";
import type { ApplicationClient, ImageAttachment } from "../contracts";
import { HTTPApplicationClient, normalizeRemoteEndpoint } from "../http-client";
import {
  readStreamingInputBehavior,
  writeStreamingInputBehavior,
  type StreamingInputBehavior,
} from "../streaming-input-behavior";
import { AuthGate } from "./AuthGate";
import { Composer, type ComposerHandle } from "./Composer";
import { DirectoryPicker } from "./DirectoryPicker";
import { FilePreviewPanel } from "./FilePreviewPanel";
import { MessageList } from "./MessageList";
import { latestAssistantUsage } from "./message";
import { SessionPointPicker } from "./SessionPointPicker";
import { SessionStatsPanel } from "./SessionStatsPanel";
import { SettingsDrawer } from "./SettingsDrawer";
import { Sidebar } from "./Sidebar";
import { useApplicationController, type SendBehavior } from "./useApplicationController";
import { useMobilePanelGestures } from "./useMobilePanelGestures";
import {
  SIDEBAR_MAX_WIDTH,
  SIDEBAR_MIN_WIDTH,
  useResizableSidebar,
} from "./useResizableSidebar";

export interface PiWorkbenchProps {
  localClient?: ApplicationClient;
  localAvailable: boolean;
  localError?: string;
  defaultRemoteEndpoint?: string;
  version: string;
  hostKind?: "desktop" | "web" | "mobile";
  createRemoteClient?(endpoint: string): ApplicationClient;
  onEdgeGesturesEnabledChange?(enabled: boolean): void;
}

const unavailableClient: ApplicationClient = {
  kind: "remote",
  endpoint: "",
  async authStatus() { throw new Error("请先配置远程地址"); },
  async login() { throw new Error("请先配置远程地址"); },
  async snapshot() { throw new Error("请先配置远程地址"); },
  async sessionView() { throw new Error("请先配置远程地址"); },
  async createSession() { throw new Error("请先配置远程地址"); },
  async models() { throw new Error("请先配置远程地址"); },
  async browseDirectories() { throw new Error("请先配置远程地址"); },
  async listFiles() { throw new Error("请先配置远程地址"); },
  async previewFile() { throw new Error("请先配置远程地址"); },
  async addProject() { throw new Error("请先配置远程地址"); },
  async removeProject() { throw new Error("请先配置远程地址"); },
  async renameSession() { throw new Error("请先配置远程地址"); },
  async deleteSession() { throw new Error("请先配置远程地址"); },
  async dispatch<T = unknown>(): Promise<T> { throw new Error("请先配置远程地址"); },
  subscribe() {
    return { ready: Promise.reject(new Error("请先配置远程地址")), close() {} };
  },
  close() {},
};

function savedRemoteEndpoint(): string {
  try {
    return localStorage.getItem("pi.remote.endpoint") ?? "";
  } catch {
    return "";
  }
}

function activeTitle(
  sessionId: string | null,
  sessions: { id: string; name?: string; firstMessage: string }[],
): string {
  if (!sessionId) return "新会话";
  const session = sessions.find((value) => value.id === sessionId);
  return session?.name?.trim() || session?.firstMessage?.trim() || "会话";
}

export function PiWorkbench(props: PiWorkbenchProps) {
  const hostKind = props.hostKind ?? "desktop";
  const mobile = hostKind === "mobile";
  const initialRemote = props.defaultRemoteEndpoint?.trim() || savedRemoteEndpoint();
  const createRemoteClient = props.createRemoteClient ?? ((endpoint: string) => new HTTPApplicationClient(endpoint));
  const [remoteEndpoint, setRemoteEndpoint] = useState(initialRemote);
  const [client, setClient] = useState<ApplicationClient>(() => {
    if (props.defaultRemoteEndpoint) return createRemoteClient(props.defaultRemoteEndpoint);
    if (props.localAvailable && props.localClient) return props.localClient;
    if (initialRemote) return createRemoteClient(initialRemote);
    return unavailableClient;
  });
  const [sidebarOpen, setSidebarOpen] = useState(() => !mobile && window.innerWidth >= 800);
  const [sidebarSection, setSidebarSection] = useState<"sessions" | "files">("sessions");
  const [previewPath, setPreviewPath] = useState("");
  const [projectPickerOpen, setProjectPickerOpen] = useState(false);
  const [pointPicker, setPointPicker] = useState<"tree" | "fork" | null>(null);
  const [settingsOpen, setSettingsOpen] = useState(hostKind === "mobile" && !initialRemote);
  const [streamingInputBehavior, setStreamingInputBehavior] = useState(readStreamingInputBehavior);
  const composerRef = useRef<ComposerHandle>(null);
  const controller = useApplicationController(client);
  const sidebar = useResizableSidebar();
  const workbenchStyle = {
    "--pi-sidebar-width": `${sidebar.width}px`,
  } as CSSProperties;
  const workbenchClass = `pi-workbench is-${hostKind} ${sidebar.resizing ? "is-resizing" : ""}`;

  useEffect(() => () => client.close(), [client]);
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setSettingsOpen(false);
        setPointPicker(null);
        setPreviewPath("");
        setProjectPickerOpen(false);
        if (mobile || window.innerWidth < 800) {
          setSidebarOpen(false);
        }
      }
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [mobile]);

  const sessions = controller.snapshot?.sessions ?? [];
  const title = useMemo(
    () => activeTitle(controller.activeSessionId, sessions),
    [controller.activeSessionId, sessions],
  );
  const latestUsage = useMemo(
    () => latestAssistantUsage(controller.messages, controller.streamingMessage),
    [controller.messages, controller.streamingMessage],
  );
  const empty = controller.messages.length === 0 && !controller.streamingMessage;
  const mobileGesturesEnabled = mobile
    && controller.status === "ready"
    && !settingsOpen
    && !previewPath
    && !projectPickerOpen
    && pointPicker === null
    && !controller.sessionStatsOpen;
  const anchorGesturesEnabled = mobileGesturesEnabled && !sidebarOpen;
  const listFiles = useCallback((path: string) => client.listFiles(path), [client]);
  const previewFile = useCallback((path: string) => client.previewFile(path), [client]);
  const gestures = useMobilePanelGestures({
    enabled: mobileGesturesEnabled,
    sidebarOpen,
    setSidebarOpen,
  });

  useEffect(() => {
    props.onEdgeGesturesEnabledChange?.(mobileGesturesEnabled);
  }, [mobileGesturesEnabled, props.onEdgeGesturesEnabledChange]);

  useEffect(() => () => {
    props.onEdgeGesturesEnabledChange?.(false);
  }, [props.onEdgeGesturesEnabledChange]);

  useEffect(() => {
    setPreviewPath("");
  }, [client, controller.workingDirectory]);

  const closeMobileSidebar = () => {
    if (mobile || window.innerWidth < 800) setSidebarOpen(false);
  };

  const useLocal = () => {
    if (!props.localAvailable || !props.localClient) return;
    setClient(props.localClient);
    setSettingsOpen(false);
  };

  const useRemote = (endpoint: string) => {
    const normalized = normalizeRemoteEndpoint(endpoint);
    try {
      localStorage.setItem("pi.remote.endpoint", normalized);
    } catch {
      // Storage is optional; the active connection still works.
    }
    setRemoteEndpoint(normalized);
    setClient(createRemoteClient(normalized));
    setSettingsOpen(false);
  };

  const closePointPicker = useCallback(() => setPointPicker(null), []);
  const changeStreamingInputBehavior = useCallback((value: StreamingInputBehavior) => {
    setStreamingInputBehavior(value);
    writeStreamingInputBehavior(value);
  }, []);
  const send = useCallback(async (
    text: string,
    behavior?: SendBehavior,
    images: ImageAttachment[] = [],
  ) => {
    const match = images.length === 0 ? text.trim().match(/^\/([^\s]+)(?:\s+([\s\S]*))?$/) : null;
    const command = match?.[1]?.toLocaleLowerCase();
    const argument = (match?.[2] ?? "").trim();

    switch (command) {
      case "help":
      case "hotkeys":
        composerRef.current?.setDraft("/");
        return;
      case "new":
        controller.beginNewSession();
        setSidebarOpen(false);
        return;
      case "resume":
        if (argument) {
          await controller.selectSession(argument);
        } else {
          setSidebarSection("sessions");
          setSidebarOpen(true);
        }
        return;
      case "tree":
      case "fork":
        if (controller.activeSessionId) setPointPicker(command);
        return;
      case "clone":
        if (controller.activeSessionId) await controller.clone();
        return;
      case "model": {
        if (!argument) {
          composerRef.current?.setDraft("/model ");
          return;
        }
        const separator = argument.indexOf("/");
        const model = separator > 0
          ? { provider: argument.slice(0, separator), modelId: argument.slice(separator + 1) }
          : null;
        if (!model || !controller.models?.modelList.some((item) => item.provider === model.provider && item.id === model.modelId)) {
          composerRef.current?.setDraft("/model ");
          return;
        }
        await controller.setModel(model);
        return;
      }
      case "thinking":
        if (!argument) {
          composerRef.current?.setDraft("/thinking ");
          return;
        }
        await controller.setThinkingLevel(argument);
        return;
      case "tools":
        if (argument !== "none" && argument !== "default" && argument !== "full") {
          composerRef.current?.setDraft("/tools ");
          return;
        }
        await controller.setToolPreset(argument);
        return;
      case "settings":
        setSettingsOpen(true);
        return;
      case "abort":
        await controller.abort();
        return;
      case "clear-queue":
      case "dequeue":
        await controller.clearQueue();
        return;
      case "stats":
      case "session":
        await controller.openSessionStats();
        return;
      case "compact":
        await controller.compact(argument);
        return;
      case "reload":
        await controller.reload();
        return;
      case "name":
        if (!argument) {
          composerRef.current?.setDraft("/name ");
          return;
        }
        if (controller.activeSessionId) await controller.renameSession(controller.activeSessionId, argument);
        return;
      case "copy":
        await controller.copyLastAssistant();
        return;
      default:
        await controller.send(text, behavior, images);
    }
  }, [controller]);

  const settings = (
    <SettingsDrawer
      open={settingsOpen}
      kind={client.kind}
      endpoint={remoteEndpoint}
      version={props.version}
      localAvailable={props.localAvailable}
      localError={props.localError}
      hostKind={hostKind}
      streamingInputBehavior={streamingInputBehavior}
      onClose={() => setSettingsOpen(false)}
      onUseLocal={useLocal}
      onUseRemote={useRemote}
      onStreamingInputBehaviorChange={changeStreamingInputBehavior}
    />
  );

  if (controller.status === "auth") {
    return (
      <div className={workbenchClass} style={workbenchStyle}>
        <AuthGate
          title="请输入访问密码。"
          error={controller.error}
          onLogin={controller.login}
          onOpenSettings={() => setSettingsOpen(true)}
        />
        {settings}
      </div>
    );
  }

  if (controller.status === "connecting") {
    return (
      <div className={workbenchClass} style={workbenchStyle}>
        <main className="pi-entry" aria-live="polite">
          <section className="pi-entry-panel">
            <h1>pi</h1>
            <div className="pi-entry-connecting">
              <LoaderCircle size={16} />
              <p className="pi-entry-status">正在连接 {client.kind === "local" ? "此设备" : client.endpoint}…</p>
            </div>
            <div className="pi-entry-actions">
              <button type="button" onClick={controller.cancelConnection}>停止连接</button>
              <button
                type="button"
                onClick={() => {
                  controller.cancelConnection();
                  setSettingsOpen(true);
                }}
              >
                {mobile ? "切换节点" : "连接设置"}
              </button>
            </div>
          </section>
        </main>
        {settings}
      </div>
    );
  }

  if (controller.status === "error" && !controller.snapshot) {
    return (
      <div className={workbenchClass} style={workbenchStyle}>
        <main className="pi-entry" aria-live="polite">
          <button className="pi-floating-settings" type="button" aria-label="打开设置" onClick={() => setSettingsOpen(true)}>
            <Settings size={17} />
          </button>
          <section className="pi-entry-panel">
            <h1>pi</h1>
            <p className="pi-entry-status">无法连接</p>
            <p className="pi-entry-error">{controller.error}</p>
            <button type="button" onClick={() => void controller.retry()}>重试</button>
          </section>
        </main>
        {settings}
      </div>
    );
  }

  return (
    <div {...gestures} className={workbenchClass} style={workbenchStyle}>
      <button
        className={`pi-sidebar-toggle ${sidebarOpen ? "is-sidebar" : "is-main"}`}
        type="button"
        aria-label={sidebarOpen ? "收起侧栏" : "展开侧栏"}
        aria-controls="pi-workbench-sidebar"
        aria-expanded={sidebarOpen}
        onClick={() => setSidebarOpen((open) => !open)}
      >
        <PanelLeft size={18} strokeWidth={1.8} />
      </button>
      <Sidebar
        open={sidebarOpen}
        section={sidebarSection}
        projects={controller.snapshot?.projects ?? []}
        sessions={sessions}
        runningSessionIds={controller.snapshot?.runningSessionIds ?? []}
        activeSessionId={controller.activeSessionId}
        activePreviewPath={previewPath}
        workingDirectory={controller.workingDirectory}
        listFiles={listFiles}
        onMentionFile={(value) => {
          composerRef.current?.insertText(value);
          closeMobileSidebar();
        }}
        onPreviewFile={(path) => {
          setPreviewPath(path);
          closeMobileSidebar();
        }}
        onSectionChange={setSidebarSection}
        onClose={() => setSidebarOpen(false)}
        onAddProject={() => setProjectPickerOpen(true)}
        onRemoveProject={controller.removeProject}
        onNewSession={(cwd) => {
          controller.beginNewSession(cwd);
          closeMobileSidebar();
        }}
        onSelect={(sessionId) => {
          void controller.selectSession(sessionId);
          closeMobileSidebar();
        }}
        onRename={controller.renameSession}
        onDelete={controller.deleteSession}
        onOpenSettings={() => setSettingsOpen(true)}
      />
      {sidebarOpen && (
        <div
          {...sidebar.separatorProps}
          className="pi-sidebar-resizer"
          role="separator"
          tabIndex={0}
          aria-label="调整侧栏宽度"
          aria-controls="pi-workbench-sidebar"
          aria-orientation="vertical"
          aria-valuemin={SIDEBAR_MIN_WIDTH}
          aria-valuemax={SIDEBAR_MAX_WIDTH}
          aria-valuenow={sidebar.width}
        />
      )}
      <main className={`pi-main ${sidebarOpen ? "has-sidebar" : ""} ${previewPath ? "has-preview" : ""}`}>
        <header className="pi-topbar">
          <div className="pi-topbar-heading">
            {!mobile && <Folder size={18} />}
            <div className="pi-topbar-title" title={title}>{title}</div>
          </div>
        </header>

        <section className={`pi-conversation ${empty ? "is-empty" : ""}`}>
          {controller.sessionLoading ? (
            <div className="pi-session-loading" role="status" aria-live="polite">
              <LoaderCircle size={20} />
              <span>正在加载会话…</span>
            </div>
          ) : !empty && controller.activeSessionId ? (
            <MessageList
              sessionId={controller.activeSessionId}
              messages={controller.messages}
              pendingMessages={controller.pendingMessages}
              entryIds={controller.sessionView?.context.entryIds ?? []}
              streamingMessage={controller.streamingMessage}
              busy={controller.busy}
              mobile={mobile}
              anchorsEnabled={!mobile || anchorGesturesEnabled}
              onFork={controller.fork}
              onEdit={controller.editAndResend}
            />
          ) : null}
          {!controller.sessionLoading && (
            <Composer
              ref={composerRef}
              centered={empty}
              active={controller.activeSessionId !== null}
              mobile={mobile}
              models={controller.models}
              model={controller.selectedModel}
              thinkingLevel={controller.thinkingLevel}
              contextUsage={controller.runtimeState?.contextUsage ?? controller.sessionStats?.contextUsage ?? null}
              latestUsage={latestUsage}
              sessionStats={controller.sessionStats}
              busy={controller.busy}
              streamingInputBehavior={streamingInputBehavior}
              sessions={sessions}
              projects={controller.snapshot?.projects ?? []}
              workingDirectory={controller.workingDirectory}
              toolPreset={controller.toolPreset}
              slashCommands={controller.slashCommands}
              onSend={send}
              onAbort={controller.abort}
              onModelChange={controller.setModel}
              onThinkingLevelChange={controller.setThinkingLevel}
              onProjectChange={controller.setWorkingDirectory}
            />
          )}
          {controller.error && <div className="pi-inline-error" role="alert">{controller.error}</div>}
        </section>
      </main>
      {previewPath && (
        <FilePreviewPanel
          path={previewPath}
          previewFile={previewFile}
          onClose={() => setPreviewPath("")}
        />
      )}
      <SessionPointPicker
        mode={pointPicker}
        messages={controller.messages}
        entryIds={controller.sessionView?.context.entryIds ?? []}
        tree={controller.sessionView?.tree ?? []}
        activeLeafId={controller.sessionView?.leafId ?? null}
        onClose={closePointPicker}
        onSelect={(entryId) => pointPicker === "fork" ? controller.forkBefore(entryId) : controller.navigateTree(entryId)}
      />
      <SessionStatsPanel
        open={controller.sessionStatsOpen}
        stats={controller.sessionStats}
        onClose={controller.closeSessionStats}
      />
      {projectPickerOpen && (
        <DirectoryPicker
          initialPath={controller.workingDirectory || controller.snapshot?.defaultCwd || ""}
          title="添加项目"
          selectLabel="添加此文件夹"
          load={controller.browseDirectories}
          onCancel={() => setProjectPickerOpen(false)}
          onSelect={async (path) => {
            await controller.addProject(path);
            controller.beginNewSession(path);
            setProjectPickerOpen(false);
            closeMobileSidebar();
          }}
        />
      )}
      {settings}
    </div>
  );
}
