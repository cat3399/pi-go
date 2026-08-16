import { Call, Events } from "@wailsio/runtime";
import type {
  ApplicationClient,
  ApplicationEventEnvelope,
  ApplicationSnapshot,
  AuthState,
  CreateSessionRequest,
  CreateSessionResult,
  DirectoryView,
  EventObserver,
  EventSubscription,
  ModelsView,
  SessionView,
} from "@cat3399/pi-workbench";

const bridge = "main.GUIBridge";
const applicationEventName = "pi:application-event";
const streamResetEventName = "pi:application-stream-reset";

export interface GUIHostInfo {
  version: string;
  localAvailable: boolean;
  localError?: string;
  defaultRemoteEndpoint?: string;
}

interface OpenedEventStream {
  streamId: number;
  revision: number;
  resetRequired: boolean;
  replay: ApplicationEventEnvelope[];
}

interface GUIApplicationEvent {
  streamId: number;
  envelope: ApplicationEventEnvelope;
}

interface GUIStreamReset {
  streamId: number;
  revision: number;
  reason?: string;
}

function call<T>(method: string, ...args: unknown[]): Promise<T> {
  return Call.ByName(`${bridge}.${method}`, ...args) as Promise<T>;
}

export function readGUIHostInfo(): Promise<GUIHostInfo> {
  return call("HostInfo");
}

export class WailsApplicationClient implements ApplicationClient {
  readonly kind = "local" as const;
  readonly endpoint = "local://embedded";

  private readonly subscriptions = new Set<() => void>();

  authStatus(): Promise<AuthState> {
    return Promise.resolve({ authRequired: false, authenticated: true });
  }

  login(): Promise<void> {
    return Promise.resolve();
  }

  snapshot(): Promise<ApplicationSnapshot> {
    return call("Snapshot");
  }

  sessionView(sessionId: string, leafId = ""): Promise<SessionView> {
    return call("SessionView", sessionId, leafId);
  }

  createSession(input: CreateSessionRequest): Promise<CreateSessionResult> {
    return call("CreateSession", input);
  }

  models(cwd: string): Promise<ModelsView> {
    return call("Models", cwd);
  }

  browseDirectories(path = ""): Promise<DirectoryView> {
    return call("BrowseDirectories", path);
  }

  renameSession(sessionId: string, name: string): Promise<void> {
    return call("RenameSession", sessionId, name);
  }

  deleteSession(sessionId: string): Promise<void> {
    return call("DeleteSession", sessionId);
  }

  async dispatch<T>(sessionId: string, command: Record<string, unknown>): Promise<T> {
    const response = await call<{ data: T }>("Dispatch", sessionId, JSON.stringify(command));
    return response.data;
  }

  subscribe(after: number, observer: EventObserver): EventSubscription {
    let closed = false;
    let streamId = 0;
    const offApplicationEvent = Events.On(applicationEventName, (event) => {
      const payload = event.data as GUIApplicationEvent;
      if (!closed && payload.streamId === streamId) observer.onEvent(payload.envelope);
    });
    const offReset = Events.On(streamResetEventName, (event) => {
      const payload = event.data as GUIStreamReset;
      if (!closed && payload.streamId === streamId) {
        observer.onReset(payload.revision, payload.reason);
      }
    });

    const close = () => {
      if (closed) return;
      closed = true;
      offApplicationEvent();
      offReset();
      this.subscriptions.delete(close);
      if (streamId) void call("CloseEventStream", streamId);
    };
    this.subscriptions.add(close);

    const ready = call<OpenedEventStream>("OpenEventStream", after).then(async (opened) => {
      if (closed) {
        await call("CloseEventStream", opened.streamId);
        return;
      }
      streamId = opened.streamId;
      if (opened.resetRequired) observer.onReset(opened.revision, "本地事件游标已过期");
      for (const envelope of opened.replay) observer.onEvent(envelope);
      await call("ResumeEventStream", streamId);
    });

    return { ready, close };
  }

  close(): void {
    for (const close of [...this.subscriptions]) close();
  }
}
