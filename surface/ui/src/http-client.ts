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
  ModelsView,
  SessionView,
} from "./contracts";

type ErrorBody = {
  error?: string;
  retryAfterMs?: number;
};

export interface RemoteRequestInit {
  method?: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
  body?: string;
}

export interface RemoteTransportResponse {
  status: number;
  body: string;
  retryAfterMs?: number;
}

export interface RemoteApplicationTransport {
  request(
    endpoint: string,
    path: string,
    token: string,
    init?: RemoteRequestInit,
  ): Promise<RemoteTransportResponse>;
  subscribe(
    endpoint: string,
    token: string,
    after: number,
    observer: EventObserver,
  ): EventSubscription;
  close(): void;
}

export class ApplicationRequestError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly retryAfterMs = 0,
  ) {
    super(message);
    this.name = "ApplicationRequestError";
  }
}

export function normalizeRemoteEndpoint(value: string): string {
  const input = value.trim();
  if (!input) throw new Error("请输入远程地址");
  const url = new URL(input);
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new Error("远程地址必须使用 http 或 https");
  }
  if (url.username || url.password) {
    throw new Error("远程地址不能包含用户名或密码");
  }
  url.pathname = url.pathname.replace(/\/+$/, "");
  url.search = "";
  url.hash = "";
  return url.toString().replace(/\/$/, "");
}

function encodeFilePathForAPI(filePath: string): string {
  const normalized = /^[a-zA-Z]:[\\/]/.test(filePath) || filePath.startsWith("\\\\")
    ? filePath.replace(/\\/g, "/")
    : filePath;
  return normalized
    .split("/")
    .filter(Boolean)
    .map(encodeURIComponent)
    .join("/");
}

async function sha256Hex(value: string): Promise<string> {
  if (!globalThis.crypto?.subtle) {
    throw new Error("当前 WebView 不支持密码摘要");
  }
  const digest = await globalThis.crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(value),
  );
  return Array.from(new Uint8Array(digest))
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
}

export class RemoteApplicationClient implements ApplicationClient {
  readonly kind = "remote" as const;
  readonly endpoint: string;

  private token: string;

  constructor(
    endpoint: string,
    private readonly transport: RemoteApplicationTransport,
    token = "",
  ) {
    this.endpoint = normalizeRemoteEndpoint(endpoint);
    this.token = token;
  }

  async authStatus(): Promise<AuthState> {
    return this.request<AuthState>("/api/v1/auth/status");
  }

  async login(password: string): Promise<void> {
    const response = await this.request<{ token?: string }>(
      "/api/v1/auth/login",
      {
        method: "POST",
        body: JSON.stringify({ passwordHash: await sha256Hex(password) }),
      },
    );
    this.token = response.token ?? "";
  }

  snapshot(): Promise<ApplicationSnapshot> {
    return this.request("/api/v1/snapshot");
  }

  sessionView(sessionId: string, leafId = ""): Promise<SessionView> {
    const query = leafId ? `?leafId=${encodeURIComponent(leafId)}` : "";
    return this.request(`/api/v1/sessions/${encodeURIComponent(sessionId)}${query}`);
  }

  createSession(input: CreateSessionRequest): Promise<CreateSessionResult> {
    return this.request("/api/v1/sessions", {
      method: "POST",
      body: JSON.stringify(input),
    });
  }

  models(cwd: string): Promise<ModelsView> {
    const query = new URLSearchParams({ cwd });
    return this.request(`/api/v1/models?${query.toString()}`);
  }

  browseDirectories(path = ""): Promise<DirectoryView> {
    const query = path ? `?path=${encodeURIComponent(path)}` : "";
    return this.request(`/api/v1/system/cwd/browse${query}`);
  }

  listFiles(path: string): Promise<FileList> {
    return this.request(`/api/v1/files/${encodeFilePathForAPI(path)}?type=list`);
  }

  async renameSession(sessionId: string, name: string): Promise<void> {
    await this.request(`/api/v1/sessions/${encodeURIComponent(sessionId)}`, {
      method: "PATCH",
      body: JSON.stringify({ name }),
    });
  }

  async deleteSession(sessionId: string): Promise<void> {
    await this.request(`/api/v1/sessions/${encodeURIComponent(sessionId)}`, {
      method: "DELETE",
    });
  }

  async dispatch<T>(sessionId: string, command: Record<string, unknown>): Promise<T> {
    const response = await this.request<{ data: T }>(
      `/api/v1/sessions/${encodeURIComponent(sessionId)}/commands`,
      {
        method: "POST",
        body: JSON.stringify(command),
      },
    );
    return response.data;
  }

  subscribe(after: number, observer: EventObserver): EventSubscription {
    return this.transport.subscribe(this.endpoint, this.token, after, observer);
  }

  close(): void {
    this.transport.close();
  }

  private async request<T>(
    path: string,
    init: RemoteRequestInit = {},
  ): Promise<T> {
    const response = await this.transport.request(
      this.endpoint,
      path,
      this.token,
      init,
    );
    let body: (T & ErrorBody) | null = null;
    if (response.body.trim()) {
      try {
        body = JSON.parse(response.body) as T & ErrorBody;
      } catch {
        if (response.status >= 200 && response.status < 300) {
          throw new Error("远程返回了无效 JSON");
        }
      }
    }
    if (response.status < 200 || response.status >= 300) {
      throw new ApplicationRequestError(
        body?.error || `HTTP ${response.status}`,
        response.status,
        body?.retryAfterMs ?? response.retryAfterMs ?? 0,
      );
    }
    if (body === null) throw new Error("远程返回了无效 JSON");
    return body;
  }
}

class BrowserRemoteTransport implements RemoteApplicationTransport {
  private readonly sources = new Set<EventSource>();
  private readonly requests = new Set<AbortController>();

  async request(
    endpoint: string,
    path: string,
    token: string,
    init: RemoteRequestInit = {},
  ): Promise<RemoteTransportResponse> {
    const controller = new AbortController();
    this.requests.add(controller);
    const headers = new Headers({ Accept: "application/json" });
    if (init.body !== undefined) headers.set("Content-Type", "application/json");
    if (token) headers.set("Authorization", `Bearer ${token}`);
    try {
      const response = await fetch(new URL(path, endpoint), {
        method: init.method ?? "GET",
        body: init.body,
        headers,
        cache: "no-store",
        credentials: "include",
        signal: controller.signal,
      });
      const retryAfterSeconds = Number.parseInt(response.headers.get("Retry-After") ?? "", 10);
      return {
        status: response.status,
        body: await response.text(),
        retryAfterMs: Number.isFinite(retryAfterSeconds) ? retryAfterSeconds * 1000 : undefined,
      };
    } finally {
      this.requests.delete(controller);
    }
  }

  subscribe(
    endpoint: string,
    token: string,
    after: number,
    observer: EventObserver,
  ): EventSubscription {
    let closed = false;
    let source: EventSource | null = null;
    let resolveReady: () => void = () => undefined;
    let rejectReady: (error: unknown) => void = () => undefined;
    const ready = new Promise<void>((resolve, reject) => {
      resolveReady = resolve;
      rejectReady = reject;
    });

    try {
      const url = new URL("/api/v1/events", endpoint);
      url.searchParams.set("after", String(after));
      if (token) url.searchParams.set("token", token);
      source = new EventSource(url, { withCredentials: true });
      this.sources.add(source);
      source.onopen = () => resolveReady();
      source.onmessage = (message) => {
        if (closed) return;
        applyEventData(message.data, observer, resolveReady);
      };
      source.onerror = () => {
        if (!closed && source?.readyState === EventSource.CLOSED) {
          rejectReady(new Error("远程事件连接失败"));
        }
      };
    } catch (error) {
      rejectReady(error);
    }

    return {
      ready,
      close: () => {
        if (closed) return;
        closed = true;
        if (source) {
          this.sources.delete(source);
          source.close();
        }
      },
    };
  }

  close(): void {
    for (const controller of this.requests) controller.abort();
    this.requests.clear();
    for (const source of this.sources) source.close();
    this.sources.clear();
  }
}

export function applyEventData(
  data: string,
  observer: EventObserver,
  onConnected: () => void = () => undefined,
): void {
  let value: unknown;
  try {
    value = JSON.parse(data);
  } catch {
    return;
  }
  if (!value || typeof value !== "object") return;
  const object = value as Record<string, unknown>;
  if (object.type === "connected") {
    onConnected();
    return;
  }
  if (object.type === "reset_required") {
    observer.onReset(
      typeof object.revision === "number" ? object.revision : 0,
      "远程事件游标已过期",
    );
    return;
  }
  if (
    typeof object.sequence === "number"
    && typeof object.sessionId === "string"
    && object.event
    && typeof object.event === "object"
  ) {
    observer.onEvent(object as unknown as ApplicationEventEnvelope);
  }
}

export class HTTPApplicationClient extends RemoteApplicationClient {
  constructor(endpoint: string, token = "") {
    super(endpoint, new BrowserRemoteTransport(), token);
  }
}
