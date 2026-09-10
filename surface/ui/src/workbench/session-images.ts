import type { AgentMessage, MessageContentBlock } from "../contracts";

function referenceKey(block: MessageContentBlock): string | null {
  return block.imageRef ? JSON.stringify([block.imageRef.entryId, block.imageRef.blockIndex]) : null;
}

function inlineSource(block: MessageContentBlock): string | null {
  if (typeof block.data === "string" && typeof block.mimeType === "string") {
    return `data:${block.mimeType};base64,${block.data}`;
  }
  const source = block.source as Record<string, unknown> | undefined;
  if (source?.type === "base64" && typeof source.data === "string" && typeof source.media_type === "string") {
    return `data:${source.media_type};base64,${source.data}`;
  }
  return null;
}

function liveMessageKey(message: AgentMessage): string | null {
  if (message.pendingPrompt || typeof message.timestamp !== "number") return null;
  const content = Array.isArray(message.content) ? message.content.map((block) => {
    if (block.type !== "image") return [block.type, block.text];
    const source = block.source as Record<string, unknown> | undefined;
    const data = typeof block.data === "string" ? block.data : source?.data;
    const size = typeof data === "string"
      ? Math.floor(data.length * 3 / 4) - (data.endsWith("==") ? 2 : data.endsWith("=") ? 1 : 0)
      : block.byteSize;
    return ["image", block.mimeType ?? source?.media_type, size];
  }) : message.content;
  return JSON.stringify([message.role, message.timestamp, message.toolCallId, message.customType, content]);
}

// Display-only cache for the current session. History remains authoritative for
// messages; refreshing it must not discard image bytes already held by the UI.
export class SessionImageCache {
  private sessionId = "";
  private readonly sources = new Map<string, string>();

  selectSession(sessionId: string): void {
    if (this.sessionId === sessionId) return;
    this.sessionId = sessionId;
    this.sources.clear();
  }

  availableSource(block: MessageContentBlock): string | null {
    const key = referenceKey(block);
    return (key ? this.sources.get(key) : null) ?? inlineSource(block);
  }

  source(block: MessageContentBlock): string {
    const available = this.availableSource(block);
    if (available) return available;
    if (typeof block.url === "string") return block.url;
    const source = block.source as Record<string, unknown> | undefined;
    return typeof source?.url === "string" ? source.url : "";
  }

  rememberLoaded(block: MessageContentBlock, source: string): void {
    const key = referenceKey(block);
    if (key && source) this.sources.set(key, source);
  }

  reconcile(current: AgentMessage[], history: AgentMessage[]): void {
    const live = new Map<string, AgentMessage | null>();
    for (const message of current) {
      const key = liveMessageKey(message);
      // Live events do not carry entry IDs. Only use an unambiguous identity
      // until the first history refresh supplies the durable image reference.
      if (key) live.set(key, live.has(key) ? null : message);
      if (!Array.isArray(message.content)) continue;
      for (const block of message.content) {
        if (!block.imageRef) continue;
        const available = this.availableSource(block);
        if (available) this.rememberLoaded(block, available);
      }
    }
    const occurrences = new Map<string, number>();
    for (const message of history) {
      const key = liveMessageKey(message);
      if (key) occurrences.set(key, (occurrences.get(key) ?? 0) + 1);
    }
    for (const message of history) {
      const key = liveMessageKey(message);
      const previous = key && occurrences.get(key) === 1 ? live.get(key) : null;
      if (!previous || !Array.isArray(previous.content) || !Array.isArray(message.content)) continue;
      if (previous.content.length !== message.content.length) continue;
      for (let index = 0; index < message.content.length; index++) {
        const block = message.content[index];
        const before = previous.content[index];
        if (block?.type !== "image" || !block.imageRef || before?.type !== "image") continue;
        // Existing references must match exactly across branch changes.
        if (before.imageRef && referenceKey(before) !== referenceKey(block)) continue;
        const available = this.availableSource(before);
        if (available) this.rememberLoaded(block, available);
      }
    }
  }
}
