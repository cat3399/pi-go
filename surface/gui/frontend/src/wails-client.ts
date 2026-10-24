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
  FileList,
  FilePreview,
  ModelsView,
  ProjectInfo,
  SessionView,
  SessionResources,
  ResourceResponse,
  ReadResourceRequest,
  UploadConflictStrategy,
  UploadResult,
  UploadTargetInspection,
} from "@cat3399/pi-workbench";
import { encodeUploadFiles } from "@cat3399/pi-workbench";

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
  cursor: string;
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
  cursor: string;
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

  readResource<K extends keyof SessionResources>(sessionId: string, kind: K, query: ReadResourceRequest = {}, known = ""): Promise<ResourceResponse<SessionResources[K]>> {
    return call("ReadSessionResource", sessionId, kind, JSON.stringify(query), known);
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

  listFiles(path: string): Promise<FileList> {
    return call("ListFiles", path);
  }

  previewFile(path: string): Promise<FilePreview> {
    return call("PreviewFile", path);
  }

  deleteFile(path: string): Promise<void> {
    return call("DeleteFile", path);
  }

  inspectUploadTargets(directory: string, fileNames: string[]): Promise<UploadTargetInspection> {
    return call("InspectUploadTargets", directory, fileNames);
  }

  async uploadFiles(
    directory: string,
    files: File[],
    strategy: UploadConflictStrategy,
  ): Promise<UploadResult> {
    const encoded = await encodeUploadFiles(files);
    return call("UploadFiles", directory, JSON.stringify(encoded), strategy);
  }

  addProject(path: string): Promise<ProjectInfo> {
    return call("AddProject", path);
  }

  removeProject(path: string): Promise<void> {
    return call("RemoveProject", path);
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

  subscribe(after: number | string, observer: EventObserver, sessionIds?: string[]): EventSubscription {
    let closed = false;
    let streamId = 0;
    const offApplicationEvent = Events.On(applicationEventName, (event) => {
      const payload = event.data as GUIApplicationEvent;
      if (!closed && payload.streamId === streamId) observer.onEvent(payload.envelope);
    });
    const offReset = Events.On(streamResetEventName, (event) => {
      const payload = event.data as GUIStreamReset;
      if (!closed && payload.streamId === streamId) {
        observer.onReset(payload.revision, payload.reason, payload.cursor);
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

    const opening = typeof after === "string"
      ? call<OpenedEventStream>("OpenReadEventStream", after, sessionIds ? JSON.stringify(sessionIds) : "")
      : sessionIds ? Promise.reject(new Error("详情订阅需要包含流代次的游标")) : call<OpenedEventStream>("OpenEventStream", after);
    const ready = opening.then(async (opened) => {
      if (closed) {
        await call("CloseEventStream", opened.streamId);
        return;
      }
      streamId = opened.streamId;
      if (opened.resetRequired) observer.onReset(opened.revision, "本地事件游标已过期", opened.cursor);
      if (closed) return;
      for (const envelope of opened.replay) observer.onEvent(envelope);
      observer.onConnected?.(opened.cursor);
      if (closed) return;
      await call("ResumeEventStream", streamId);
    });

    return { ready, close };
  }

  close(): void {
    for (const close of [...this.subscriptions]) close();
  }
}
