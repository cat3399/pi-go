const API_ROOT = "/api/v1";
const CONNECT_TIMEOUT_MS = 5_000;

export interface ApplicationEvent {
  type: string;
  [key: string]: unknown;
}

export interface ApplicationEventEnvelope {
  sequence: number;
  sessionId: string;
  event: ApplicationEvent;
}

type EventListener = (envelope: ApplicationEventEnvelope) => void;
type ResetListener = (revision: number) => void;

export interface EventSubscription {
  ready(): Promise<void>;
  close(): void;
}

export class ApplicationRequestError extends Error {
  constructor(message: string, public readonly status: number) {
    super(message);
    this.name = "ApplicationRequestError";
  }
}

type RegisteredListener = {
  sessionId?: string;
  onEvent: EventListener;
  onReset?: ResetListener;
  after?: number;
};

class ApplicationEventClient {
  private source: EventSource | null = null;
  private connected = false;
  private nextListenerId = 0;
  private readonly listeners = new Map<number, RegisteredListener>();
  private readonly connectionWaiters = new Set<() => void>();
  private readonly history: ApplicationEventEnvelope[] = [];
  private lastSequence = 0;

  subscribe(listener: RegisteredListener): EventSubscription {
    const id = ++this.nextListenerId;
    this.listeners.set(id, listener);
    this.open(listener.after);
    this.replay(listener);
    let closed = false;
    return {
      ready: () => this.waitUntilConnected(),
      close: () => {
        if (closed) return;
        closed = true;
        this.listeners.delete(id);
      },
    };
  }

  private open(after?: number): void {
    if (this.source || typeof window === "undefined") return;
    const query = after === undefined ? "" : `?after=${encodeURIComponent(String(after))}`;
    const source = new EventSource(`${API_ROOT}/events${query}`);
    this.source = source;
    source.onmessage = (message) => this.receive(message);
    source.onerror = () => {
      this.connected = false;
      // Native EventSource reconnects with Last-Event-ID. The server replays
      // retained events and requests a snapshot only when that cursor expired.
    };
  }

  private receive(message: MessageEvent<string>): void {
    let value: unknown;
    try {
      value = JSON.parse(message.data);
    } catch {
      return;
    }
    if (!value || typeof value !== "object") return;
    const object = value as Record<string, unknown>;
    if (object.type === "connected") {
      this.connected = true;
      if (typeof object.revision === "number") {
        this.lastSequence = Math.max(this.lastSequence, object.revision);
      }
      for (const resolve of this.connectionWaiters) resolve();
      this.connectionWaiters.clear();
      return;
    }
    if (object.type === "reset_required") {
      const revision = typeof object.revision === "number" ? object.revision : 0;
      this.lastSequence = revision;
      this.history.length = 0;
      for (const listener of this.listeners.values()) {
        listener.after = revision;
        listener.onReset?.(revision);
      }
      return;
    }

    const sequence = object.sequence;
    const sessionId = object.sessionId;
    const event = object.event;
    if (
      typeof sequence !== "number"
      || typeof sessionId !== "string"
      || !event
      || typeof event !== "object"
      || typeof (event as { type?: unknown }).type !== "string"
    ) return;

    const envelope: ApplicationEventEnvelope = {
      sequence,
      sessionId,
      event: event as ApplicationEvent,
    };
    if (sequence <= this.lastSequence) return;
    this.lastSequence = Math.max(this.lastSequence, sequence);
    this.history.push(envelope);
    if (this.history.length > 1_024) this.history.splice(0, this.history.length - 1_024);
    for (const listener of this.listeners.values()) {
      this.deliver(listener, envelope);
    }
  }

  private replay(listener: RegisteredListener): void {
    const after = listener.after;
    if (after === undefined || this.lastSequence <= after) return;
    const replay = this.history.filter((envelope) => envelope.sequence > after);
    if (replay.length === 0 || replay[0].sequence > after + 1) {
      listener.after = this.lastSequence;
      listener.onReset?.(this.lastSequence);
      return;
    }
    for (const envelope of replay) this.deliver(listener, envelope);
  }

  private deliver(listener: RegisteredListener, envelope: ApplicationEventEnvelope): void {
    if (listener.after !== undefined && envelope.sequence <= listener.after) return;
    listener.after = envelope.sequence;
    if (!listener.sessionId || listener.sessionId === envelope.sessionId) {
      listener.onEvent(envelope);
    }
  }

  private waitUntilConnected(): Promise<void> {
    this.open();
    if (this.connected) return Promise.resolve();
    return new Promise<void>((resolve, reject) => {
      let settled = false;
      const complete = () => {
        if (settled) return;
        settled = true;
        clearTimeout(timeout);
        this.connectionWaiters.delete(complete);
        resolve();
      };
      const timeout = window.setTimeout(() => {
        if (settled) return;
        settled = true;
        this.connectionWaiters.delete(complete);
        reject(new Error("Timed out connecting to the application event stream"));
      }, CONNECT_TIMEOUT_MS);
      this.connectionWaiters.add(complete);
    });
  }
}

const eventClient = new ApplicationEventClient();

export function subscribeSessionEvents(
  sessionId: string,
  onEvent: (event: ApplicationEvent, sequence: number) => void,
  options: { after?: number; onReset?: ResetListener } = {},
): EventSubscription {
  return eventClient.subscribe({
    sessionId,
    onEvent: ({ event, sequence }) => onEvent(event, sequence),
    after: options.after,
    onReset: options.onReset,
  });
}

export function subscribeApplicationEvents(
  onEvent: EventListener,
  options: { after?: number; onReset?: ResetListener } = {},
): EventSubscription {
  return eventClient.subscribe({ onEvent, after: options.after, onReset: options.onReset });
}

export async function sendSessionCommand<T = unknown>(
  sessionId: string,
  command: Record<string, unknown>,
): Promise<T> {
  const response = await fetch(`${API_ROOT}/sessions/${encodeURIComponent(sessionId)}/commands`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(command),
  });
  const body = (await response.json().catch(() => ({}))) as {
    data?: T;
    error?: string;
  };
  if (!response.ok || body.error) {
    throw new ApplicationRequestError(body.error ?? `HTTP ${response.status}`, response.status);
  }
  return body.data as T;
}
