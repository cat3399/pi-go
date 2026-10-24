import type { ResourceResponse } from "../contracts";

interface CacheEntry<T = unknown> {
  value?: T;
  token: string;
  cursor?: string;
  dirty: boolean;
  revision: number;
  checkedAt: number;
  bytes: number;
  flight?: { revision: number; promise: Promise<ResourceResponse<T>> };
}

// Conservative retained-value estimate; strings are counted as UTF-16. Shared
// references may be counted twice across resources, never silently excluded.
export function retainedBytes(value: unknown, seen = new Set<object>()): number {
  if (typeof value === "string") return 24 + value.length * 2;
  if (!value || typeof value !== "object") return 8;
  if (seen.has(value)) return 0;
  seen.add(value);
  if (Array.isArray(value)) return 32 + value.reduce((sum, item) => sum + retainedBytes(item, seen), 0);
  let size = 48;
  for (const [key, item] of Object.entries(value)) size += 16 + key.length * 2 + retainedBytes(item, seen);
  return size;
}

export class StaleResourceRead extends Error {
  constructor() { super("资源已变化，正在重新读取"); }
}

// This is an evictable presentation cache. Every read keeps its invalidation
// stamp, so an earlier request can neither validate nor overwrite newer data.
export class ResourceCache {
  private readonly entries = new Map<string, CacheEntry>();
  private readonly pins = new Map<string, number>();
  private bytes = 0;
  readonly budget: number;

  constructor(budget = 48 * 1024 * 1024) { this.budget = budget; }

  get retainedBytes(): number { return this.bytes; }
  get size(): number { return this.entries.size; }
  keys(): string[] { return [...this.entries.keys()]; }

  peek<T>(key: string): T | undefined {
    const entry = this.entries.get(key);
    if (!entry) return undefined;
    this.touch(key, entry);
    return entry.value as T | undefined;
  }

  isFresh(key: string, maxAge = Infinity): boolean {
    const entry = this.entries.get(key);
    return Boolean(entry?.value !== undefined && !entry.dirty && Date.now() - entry.checkedAt < maxAge);
  }

  pin(key: string): () => void {
    this.pins.set(key, (this.pins.get(key) ?? 0) + 1);
    return () => {
      const count = this.pins.get(key) ?? 0;
      if (count > 1) this.pins.set(key, count - 1);
      else this.pins.delete(key);
      this.evict();
    };
  }

  put<T>(key: string, value: T, token = "", cursor?: string): void {
    const previous = this.entries.get(key);
    const entry: CacheEntry<T> = {
      value, token, cursor, dirty: false, revision: (previous?.revision ?? 0) + 1,
      checkedAt: Date.now(), bytes: retainedBytes(value) + 128 + key.length * 2,
    };
    this.bytes += entry.bytes - (previous?.bytes ?? 0);
    this.touch(key, entry);
    this.evict();
  }

  invalidate(matches: string | ((key: string) => boolean)): void {
    for (const [key, entry] of this.entries) {
      if (typeof matches === "string" ? key !== matches : !matches(key)) continue;
      entry.dirty = true;
      entry.revision++;
    }
  }

  forget(matches: string | ((key: string) => boolean)): void {
    for (const [key, entry] of this.entries) {
      if (typeof matches === "string" ? key !== matches : !matches(key)) continue;
      entry.revision++;
      this.bytes -= entry.bytes;
      this.entries.delete(key);
    }
  }

  async read<T>(key: string, load: (known: string) => Promise<ResourceResponse<T>>, options: { force?: boolean; maxAge?: number } = {}): Promise<ResourceResponse<T> & { data: T }> {
    if (options.force && this.entries.get(key)?.dirty === false) this.invalidate(key);
    for (let attempt = 0; attempt < 4; attempt++) {
      let entry = this.entries.get(key) as CacheEntry<T> | undefined;
      if (entry && this.isFresh(key, options.maxAge)) {
        this.touch(key, entry);
        return { token: entry.token, data: entry.value!, cursor: entry.cursor };
      }
      if (!entry) {
        entry = { token: "", dirty: true, revision: 0, checkedAt: 0, bytes: 128 + key.length * 2 };
        this.entries.set(key, entry);
        this.bytes += entry.bytes;
      }
      const captured = entry;
      if (!entry.flight) {
        const flight = { revision: captured.revision, promise: Promise.resolve().then(() => load(captured.value === undefined ? "" : captured.token)) };
        captured.flight = flight;
        // All waiters observe the same request. Only this completion installs
        // its result; invalidation while loading keeps the old data dirty.
        void flight.promise.finally(() => {
          if (captured.flight === flight) captured.flight = undefined;
        }).catch(() => undefined);
      }
      const flight = entry.flight!;
      let result: ResourceResponse<T>;
      try { result = await flight.promise; }
      catch (error) { captured.dirty = true; throw error; }
      if (this.entries.get(key) !== captured || captured.revision !== flight.revision) continue;
      const value = result.unchanged ? captured.value : result.data;
      if (value === undefined) {
        captured.token = "";
        captured.dirty = true;
        continue;
      }
      // Preserve the entry object for other waiters of this same flight.
      const size = retainedBytes(value) + 128 + key.length * 2;
      this.bytes += size - captured.bytes;
      Object.assign(captured, { value, token: result.token, cursor: result.cursor, dirty: false, checkedAt: Date.now(), bytes: size });
      this.touch(key, captured);
      this.evict();
      return { ...result, data: value };
    }
    throw new StaleResourceRead();
  }

  private touch(key: string, entry: CacheEntry): void {
    this.entries.delete(key);
    this.entries.set(key, entry);
  }

  private evict(): void {
    for (const [key, entry] of this.entries) {
      if (this.bytes <= this.budget && this.entries.size <= 2048) break;
      if (this.pins.has(key) || entry.flight) continue;
      entry.revision++;
      this.entries.delete(key);
      this.bytes -= entry.bytes;
    }
  }
}
