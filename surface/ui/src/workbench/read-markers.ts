import type { SessionInfo } from "../contracts";

type Marker = { entryId: string; messageCount: number };
type Summary = Pick<SessionInfo, "id" | "lastAssistantMessageId" | "messageCount">;

function localStorageOrNull(): Storage | null {
  try { return globalThis.localStorage ?? null; } catch { return null; }
}

/** Read state belongs to this device, separate from Agent and durable history. */
export class ReadMarkers {
  private values: Record<string, Marker>;
  readonly key: string;
  private readonly storage: Storage | null;

  constructor(namespace: string, storage = localStorageOrNull()) {
    this.key = `pi.read-markers.v1:${encodeURIComponent(namespace)}`;
    this.storage = storage;
    this.values = this.read();
  }

  private read(): Record<string, Marker> {
    try {
      const value = JSON.parse(this.storage?.getItem(this.key) ?? "{}");
      return value && typeof value === "object" && !Array.isArray(value) ? value : {};
    } catch { return {}; }
  }

  refresh(): void { this.values = this.read(); }

  initialize(info: Summary, completedWhileConnected = false): void {
    if (this.values[info.id]) return;
    this.values = { ...this.values, ...this.read() };
    if (this.values[info.id]) return;
    this.values[info.id] = completedWhileConnected ? { entryId: "", messageCount: 0 }
      : { entryId: info.lastAssistantMessageId ?? "", messageCount: info.messageCount };
    this.persist();
  }

  unread(info: Summary): boolean {
    const seen = this.values[info.id];
    return Boolean(info.lastAssistantMessageId && seen && info.messageCount >= seen.messageCount
      && info.lastAssistantMessageId !== seen.entryId);
  }

  markRead(info: Summary): boolean {
    const saved = this.read()[info.id], previous = this.values[info.id];
    // A window still displaying older history must not undo another window's read.
    if (saved && saved.messageCount > info.messageCount) { this.values[info.id] = saved; return false; }
    if (!info.lastAssistantMessageId || previous?.entryId === info.lastAssistantMessageId) return false;
    this.values = { ...this.values, ...this.read(),
      [info.id]: { entryId: info.lastAssistantMessageId, messageCount: info.messageCount } };
    this.persist();
    return true;
  }

  private persist(): void {
    try { this.storage?.setItem(this.key, JSON.stringify(this.values)); } catch { /* Presentation state remains in memory. */ }
  }
}
