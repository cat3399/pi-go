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
} from "./contracts";

type ErrorBody = {
  error?: string;
  retryAfterMs?: number;
};

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
  url.pathname = url.pathname.replace(/\/+$/, "");
  url.search = "";
  url.hash = "";
  return url.toString().replace(/\/$/, "");
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

export class HTTPApplicationClient implements ApplicationClient {
  readonly kind = "remote" as const;
  readonly endpoint: string;

  private token = "";
  private readonly sources = new Set<EventSource>();

  constructor(endpoint: string, token = "") {
    this.endpoint = normalizeRemoteEndpoint(endpoint);
    this.token = token;
  }

  async authStatus(): Promise<AuthState> {
    return this.request<AuthState>("/api/v1/auth/status", { method: "GET" });
  }

  async login(password: string): Promise<void> {
    const response = await this.request<{ token?: string }>(
      "/api/v1/auth/login",
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
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
      headers: { "Content-Type": "application/json" },
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

  async renameSession(sessionId: string, name: string): Promise<void> {
    await this.request(`/api/v1/sessions/${encodeURIComponent(sessionId)}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
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
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(command),
      },
    );
    return response.data;
  }

  subscribe(after: number, observer: EventObserver): EventSubscription {
    let closed = false;
    let source: EventSource | null = null;
    let resolveReady: () => void = () => undefined;
    let rejectReady: (error: unknown) => void = () => undefined;
    const ready = new Promise<void>((resolve, reject) => {
      resolveReady = resolve;
      rejectReady = reject;
    });

    try {
      const url = new URL("/api/v1/events", this.endpoint);
      url.searchParams.set("after", String(after));
      if (this.token) url.searchParams.set("token", this.token);
      source = new EventSource(url, { withCredentials: true });
      this.sources.add(source);
      source.onopen = () => resolveReady();
      source.onmessage = (message) => {
        if (closed) return;
        let value: unknown;
        try {
          value = JSON.parse(message.data);
        } catch {
          return;
        }
        if (!value || typeof value !== "object") return;
        const object = value as Record<string, unknown>;
        if (object.type === "connected") {
          resolveReady();
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
    for (const source of this.sources) source.close();
    this.sources.clear();
  }

  private async request<T>(
    path: string,
    init: RequestInit = {},
  ): Promise<T> {
    const headers = new Headers(init.headers);
    headers.set("Accept", "application/json");
    if (this.token) headers.set("Authorization", `Bearer ${this.token}`);
    const response = await fetch(new URL(path, this.endpoint), {
      ...init,
      headers,
      cache: "no-store",
      credentials: "include",
    });
    const body = await response.json().catch(() => null) as (T & ErrorBody) | null;
    if (!response.ok) {
      throw new ApplicationRequestError(
        body?.error || `HTTP ${response.status}`,
        response.status,
        body?.retryAfterMs ?? 0,
      );
    }
    if (body === null) throw new Error("远程返回了无效 JSON");
    return body;
  }
}
