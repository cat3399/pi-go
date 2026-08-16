import { type CSSProperties, useEffect, useMemo, useState } from "react";
import { Folder, PanelLeft, Settings } from "lucide-react";
import type { ApplicationClient } from "../contracts";
import { HTTPApplicationClient, normalizeRemoteEndpoint } from "../http-client";
import { AuthGate } from "./AuthGate";
import { Composer } from "./Composer";
import { MessageList } from "./MessageList";
import { SettingsDrawer } from "./SettingsDrawer";
import { Sidebar } from "./Sidebar";
import { useApplicationController } from "./useApplicationController";
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
  const initialRemote = props.defaultRemoteEndpoint?.trim() || savedRemoteEndpoint();
  const createRemoteClient = props.createRemoteClient ?? ((endpoint: string) => new HTTPApplicationClient(endpoint));
  const [remoteEndpoint, setRemoteEndpoint] = useState(initialRemote);
  const [client, setClient] = useState<ApplicationClient>(() => {
    if (props.defaultRemoteEndpoint) return createRemoteClient(props.defaultRemoteEndpoint);
    if (props.localAvailable && props.localClient) return props.localClient;
    if (initialRemote) return createRemoteClient(initialRemote);
    return unavailableClient;
  });
  const [sidebarOpen, setSidebarOpen] = useState(() => window.innerWidth >= 800);
  const [settingsOpen, setSettingsOpen] = useState(hostKind === "mobile" && !initialRemote);
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
        if (window.innerWidth < 800) setSidebarOpen(false);
      }
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, []);

  const sessions = controller.snapshot?.sessions ?? [];
  const title = useMemo(
    () => activeTitle(controller.activeSessionId, sessions),
    [controller.activeSessionId, sessions],
  );
  const empty = controller.messages.length === 0 && !controller.streamingMessage;

  const closeMobileSidebar = () => {
    if (window.innerWidth < 800) setSidebarOpen(false);
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

  const settings = (
    <SettingsDrawer
      open={settingsOpen}
      kind={client.kind}
      endpoint={remoteEndpoint}
      version={props.version}
      localAvailable={props.localAvailable}
      localError={props.localError}
      hostKind={hostKind}
      onClose={() => setSettingsOpen(false)}
      onUseLocal={useLocal}
      onUseRemote={useRemote}
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
            <p className="pi-entry-status">正在连接 {client.kind === "local" ? "此设备" : client.endpoint}…</p>
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
    <div className={workbenchClass} style={workbenchStyle}>
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
        sessions={sessions}
        runningSessionIds={controller.snapshot?.runningSessionIds ?? []}
        activeSessionId={controller.activeSessionId}
        onClose={() => setSidebarOpen(false)}
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
      <main className={`pi-main ${sidebarOpen ? "has-sidebar" : ""}`}>
        <header className="pi-topbar">
          <div className="pi-topbar-heading">
            <Folder size={18} />
            <div className="pi-topbar-title" title={title}>{title}</div>
          </div>
        </header>

        <section className={`pi-conversation ${empty ? "is-empty" : ""}`}>
          {!empty && (
            <MessageList
              messages={controller.messages}
              entryIds={controller.sessionView?.context.entryIds ?? []}
              streamingMessage={controller.streamingMessage}
              busy={controller.busy}
              onFork={controller.fork}
            />
          )}
          <Composer
            centered={empty}
            active={controller.activeSessionId !== null}
            models={controller.models}
            model={controller.selectedModel}
            thinkingLevel={controller.thinkingLevel}
            contextUsage={controller.runtimeState?.contextUsage ?? controller.sessionStats?.contextUsage ?? null}
            busy={controller.busy}
            onSend={controller.send}
            onAbort={controller.abort}
            onModelChange={controller.setModel}
            onThinkingLevelChange={controller.setThinkingLevel}
          />
          {controller.error && <div className="pi-inline-error" role="alert">{controller.error}</div>}
        </section>
      </main>
      {settings}
    </div>
  );
}
