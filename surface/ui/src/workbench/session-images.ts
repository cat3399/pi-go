import type { AgentMessage, MessageContentBlock, ReadResourceRequest, ResourceResponse, SessionResources } from "../contracts";
import { ResourceCache } from "./resource-cache.ts";

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

type ImageLoader = (id: string, query: ReadResourceRequest, known: string) => Promise<ResourceResponse<SessionResources["image"]>>;

// Media uses the same byte budget as the shared presentation cache. A live
// image becomes an entry image only through the core's exact append receipt.
export class SessionImageCache {
  private sessionId = "";
  private generation = "";
  private readonly cache: ResourceCache;
  private readonly loader?: ImageLoader;

  constructor(loader?: ImageLoader, cache = new ResourceCache(24 * 1024 * 1024)) { this.loader = loader; this.cache = cache; }

  selectSession(sessionId: string, generation = ""): void {
    this.sessionId = sessionId;
    this.generation = generation;
  }

  private referenceKey(block: MessageContentBlock): string | null {
    const ref = block.imageRef;
    return ref ? JSON.stringify(["image", this.sessionId, ref.generation ?? this.generation, ref.entryId, ref.blockIndex]) : null;
  }

  availableSource(block: MessageContentBlock): string | null {
    const key = this.referenceKey(block);
    return (key ? this.cache.peek<string>(key) : null) ?? inlineSource(block);
  }

  source(block: MessageContentBlock): string {
    const available = this.availableSource(block);
    if (available) return available;
    if (typeof block.url === "string") return block.url;
    const source = block.source as Record<string, unknown> | undefined;
    return typeof source?.url === "string" ? source.url : "";
  }

  rememberLoaded(block: MessageContentBlock, source: string): void {
    const key = this.referenceKey(block);
    if (key && source) this.cache.put(key, source);
  }

  rememberMessage(sessionId: string, messageId: string, message: AgentMessage): void {
    if (!Array.isArray(message.content) || !message.content.some((block) => block.type === "image")) return;
    this.cache.put(JSON.stringify(["live-images", sessionId, messageId]), message.content.map(inlineSource));
  }

  associate(sessionId: string, messageId: string, entryId: string, generation: string): void {
    const key = JSON.stringify(["live-images", sessionId, messageId]);
    const sources = this.cache.peek<(string | null)[]>(key);
    sources?.forEach((source, blockIndex) => {
      if (source) this.cache.put(JSON.stringify(["image", sessionId, generation, entryId, blockIndex]), source);
    });
    this.cache.forget(key);
  }

  async load(block: MessageContentBlock): Promise<string> {
    const source = this.source(block);
    if (source) return source;
    const ref = block.imageRef, key = this.referenceKey(block), id = this.sessionId;
    if (!ref || !key || !this.loader) throw new Error("图片引用不可用");
    const generation = ref.generation ?? this.generation;
    const result = await this.cache.read(JSON.stringify(["image-resource", id, generation, ref.entryId, ref.blockIndex]), (known) => this.loader!(id, {
      entryId: ref.entryId, blockIndex: ref.blockIndex, generation,
    }, known));
    if (!result.data) throw new Error("图片数据不可用");
    const url = `data:${result.data.mimeType};base64,${result.data.data}`;
    this.cache.put(key, url);
    return url;
  }
}
