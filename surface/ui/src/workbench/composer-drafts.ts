import type { ApplicationClient, DraftText } from "../contracts";
import { DraftImageStore } from "./composer-draft-images.ts";

export interface DraftImageRef {
  id: string;
  mimeType: string;
  durable: boolean;
}

export interface DraftFile {
  name: string;
  path: string;
}

export interface DraftContent {
  text: string;
  images: DraftImageRef[];
  files: DraftFile[];
}

interface PendingDraft {
  id: string;
  content: DraftContent;
}

interface StoredDraft {
  version: 1;
  revision: string;
  baseRevision: string;
  baseText: string;
  textChanged: boolean;
  content: DraftContent;
  pending?: PendingDraft;
  recoveries?: PendingDraft[];
}

export interface DraftSnapshot {
  content: DraftContent;
  submitting: boolean;
  syncing: boolean;
  textChanged: boolean;
  remoteKnown: boolean;
  localError: string;
  remoteError: string;
  recoveries: { id: string; content: DraftContent }[];
}

type DraftClient = Pick<ApplicationClient, "readDraftText" | "writeDraftText">;

const emptyContent = (): DraftContent => ({ text: "", images: [], files: [] });
const copyContent = (value: DraftContent): DraftContent => ({ text: value.text, images: [...value.images], files: [...value.files] });
const hasContent = (value: DraftContent) => Boolean(value.text || value.images.length || value.files.length);
const equalContent = (left: DraftContent, right: DraftContent) => JSON.stringify(left) === JSON.stringify(right);

export function draftRevision(): string {
  return globalThis.crypto?.randomUUID?.() ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}

function mergeContent(older: DraftContent, newer: DraftContent): DraftContent {
  return {
    text: older.text && newer.text && older.text !== newer.text ? `${older.text}\n\n${newer.text}` : newer.text || older.text,
    images: [...new Map([...older.images, ...newer.images].map((image) => [image.id, image])).values()],
    files: [...new Map([...older.files, ...newer.files].map((file) => [file.path, file])).values()],
  };
}

function freshDraft(): StoredDraft {
  return { version: 1, revision: draftRevision(), baseRevision: "", baseText: "", textChanged: false, content: emptyContent() };
}

function parseDraft(value: string | null): StoredDraft | null {
  if (!value) return null;
  try {
    const record = JSON.parse(value) as StoredDraft;
    if (record.version !== 1 || typeof record.revision !== "string" || typeof record.content?.text !== "string"
      || !Array.isArray(record.content.images) || !Array.isArray(record.content.files)) return null;
    return record;
  } catch { return null; }
}

function browserStorage(): Storage | null {
  try { return globalThis.localStorage ?? null; } catch { return null; }
}

export class ComposerDraftStore {
  readonly images: DraftImageStore;
  readonly documents = new Map<string, ComposerDraft>();
  readonly pendingSends = new Set<string>();
  readonly delay: number;
  readonly maxDelay: number;
  client: DraftClient;
  private readonly prefix: string;
  private readonly storage: Storage | null;
  private readonly recoveryCache = new Map<string, PendingDraft[]>();

  constructor(client: DraftClient, namespace: string, options: { storage?: Storage | null; delay?: number; maxDelay?: number } = {}) {
    this.client = client;
    this.prefix = `pi.composer-draft.v1:${encodeURIComponent(namespace)}:`;
    this.storage = options.storage === undefined ? browserStorage() : options.storage;
    this.delay = options.delay ?? 1200;
    this.maxDelay = options.maxDelay ?? 5000;
    this.images = new DraftImageStore(namespace);
    if (typeof window !== "undefined") {
      window.addEventListener("storage", (event) => {
        for (const doc of this.documents.values()) {
          if (event.key === this.storageKey(doc.key)) doc.receiveLocal(parseDraft(event.newValue));
          else if (event.key?.startsWith(`${this.storageKey(doc.key)}:recovery:`)) {
            this.recoveryCache.delete(doc.key); doc.refreshRecoveries();
          }
        }
      });
      const flush = () => { for (const doc of this.documents.values()) void doc.flush(); };
      window.addEventListener("pagehide", flush);
      window.addEventListener("online", flush);
      window.addEventListener("focus", () => {
        for (const doc of this.documents.values()) if (doc.observed) void doc.refresh();
      });
      document.addEventListener("visibilitychange", () => {
        if (document.visibilityState === "hidden") flush();
      });
    }
  }

  private storageKey(key: string): string { return `${this.prefix}${encodeURIComponent(key)}`; }

  read(key: string): StoredDraft | null {
    try { return parseDraft(this.storage?.getItem(this.storageKey(key)) ?? null); } catch { return null; }
  }

  write(key: string, value: StoredDraft, previousRevision: string): string {
    if (!this.storage) return "本地保存不可用，当前草稿暂留在窗口中";
    try {
      const current = this.read(key);
      if (current && current.revision !== previousRevision && current.revision !== value.revision
        && (hasContent(current.content) || current.pending || current.recoveries?.length)
        && (!equalContent(current.content, value.content) || current.pending?.id !== value.pending?.id)
        && !this.backup(key, current)) {
        return "另一份本机草稿未能备份，当前输入暂留在窗口中";
      }
      this.storage.setItem(this.storageKey(key), JSON.stringify(value));
      return "";
    } catch { return "本地保存失败，当前草稿仍保留在窗口中"; }
  }

  backup(key: string, value: StoredDraft): boolean {
    if (!this.storage) return false;
    try {
      let content = value.pending ? mergeContent(value.pending.content, value.content) : value.content;
      for (const recovery of value.recoveries ?? []) content = mergeContent(recovery.content, content);
      this.storage.setItem(`${this.storageKey(key)}:recovery:${value.revision}`, JSON.stringify({ ...value, content, pending: undefined }));
      this.recoveryCache.delete(key);
      return true;
    } catch { return false; }
  }

  recoveryCopies(key: string): PendingDraft[] {
    const cached = this.recoveryCache.get(key);
    if (cached) return cached;
    const result: PendingDraft[] = [];
    const prefix = `${this.storageKey(key)}:recovery:`;
    try {
      for (let i = 0; i < (this.storage?.length ?? 0); i++) {
        const name = this.storage!.key(i);
        if (!name?.startsWith(prefix)) continue;
        const value = parseDraft(this.storage!.getItem(name));
        if (value && hasContent(value.content)) result.push({ id: value.revision, content: value.content });
      }
    } catch { /* Keep the current live draft when storage is unavailable. */ }
    this.recoveryCache.set(key, result);
    return result;
  }

  forgetRecovery(key: string, id: string): void {
    try { this.storage?.removeItem(`${this.storageKey(key)}:recovery:${id}`); } catch { /* A duplicate recovery is safer than losing it. */ }
    this.recoveryCache.delete(key);
  }

  open(key: string): ComposerDraft {
    let doc = this.documents.get(key);
    if (!doc) { doc = new ComposerDraft(this, key); this.documents.set(key, doc); }
    return doc;
  }

  move(from: string, to: string): void {
    if (from === to) return;
    const doc = this.documents.get(from);
    if (!doc) return;
    const source = doc.record;
    const recovery = source.pending;
    const empty = freshDraft();
    empty.baseRevision = source.baseRevision;
    empty.baseText = source.baseText;
    empty.textChanged = true;
    if (recovery) empty.recoveries = [recovery];
    doc.moveTo(to, from, recovery);
    this.documents.set(to, doc);
    // Publish the destination first, so a crash between the two writes leaves
    // a duplicate rather than erasing input prepared during session creation.
    if (!doc.getSnapshot().localError) this.write(from, empty, source.revision);
    this.documents.delete(from);
  }

  async finishOrigin(key: string, pending: PendingDraft, succeeded: boolean): Promise<void> {
    if (!succeeded) return;
    const doc = this.documents.get(key);
    if (doc) doc.removePendingRecovery(pending.id);
    else {
      const value = this.read(key);
      if (value) { value.recoveries = value.recoveries?.filter((entry) => entry.id !== pending.id); this.write(key, value, value.revision); }
    }
    // A new draft may already be in progress in the scratch composer. Only
    // clear the remote scratch text if it still matches the accepted message.
    const local = this.read(key);
    if (local && (hasContent(local.content) || local.pending)) return;
    try {
      const remote = await this.client.readDraftText(key);
      if (remote.text !== pending.content.text) return;
      const result = await this.client.writeDraftText(key, { baseRevision: remote.revision, revision: draftRevision(), text: "" });
      if (result.saved && !doc) {
        const current = this.read(key);
        if (current && !hasContent(current.content) && !current.pending) {
          current.baseRevision = result.draft.revision; current.baseText = ""; current.textChanged = false;
          this.write(key, current, current.revision);
        }
      }
    } catch { /* Local recovery remains independent from a failed cloud cleanup. */ }
  }
}

export class ComposerDraft {
  key: string;
  record: StoredDraft;
  private storedRevision: string;
  readonly store: ComposerDraftStore;
  private readonly listeners = new Set<() => void>();
  private state: DraftSnapshot;
  private hasLocalDraft: boolean;
  private remoteKnown = false;
  private remoteError = "";
  private localError = "";
  private syncing: Promise<void> | null = null;
  private reading: Promise<void> | null = null;
  private lastRead = 0;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private maxTimer: ReturnType<typeof setTimeout> | undefined;
  private origins: { key: string; pending: PendingDraft }[] = [];

  constructor(store: ComposerDraftStore, key: string) {
    this.store = store; this.key = key;
    const local = store.read(key);
    this.hasLocalDraft = local !== null;
    this.record = local ?? freshDraft();
    this.storedRevision = this.record.revision;
    const abandoned = [this.record.pending, ...(this.record.recoveries ?? [])]
      .filter((entry): entry is PendingDraft => Boolean(entry && !store.pendingSends.has(entry.id)));
    for (const pending of abandoned) this.record.content = mergeContent(pending.content, this.record.content);
    if (abandoned.length) {
      this.record.pending = undefined;
      this.record.recoveries = this.record.recoveries?.filter((entry) => store.pendingSends.has(entry.id));
      this.record.textChanged = true; this.record.revision = draftRevision();
      this.localError = store.write(key, this.record, this.storedRevision);
      this.storedRevision = this.record.revision;
    }
    this.state = this.snapshot();
  }

  get observed(): boolean { return this.listeners.size > 0; }
  private get awaitingOriginSend(): boolean {
    return Boolean(this.record.recoveries?.some((entry) => this.store.pendingSends.has(entry.id)));
  }
  getSnapshot = (): DraftSnapshot => this.state;
  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    void this.refresh();
    return () => { this.listeners.delete(listener); if (!this.observed) void this.flush(); };
  };

  private snapshot(): DraftSnapshot {
    return { content: this.record.content, submitting: Boolean(this.record.pending), syncing: Boolean(this.syncing),
      textChanged: this.record.textChanged, remoteKnown: this.remoteKnown,
      localError: this.localError, remoteError: this.remoteError,
      recoveries: this.store.recoveryCopies(this.key).filter((copy) => !equalContent(copy.content, this.record.content)) };
  }

  private publish(persist = true): void {
    if (persist) {
      this.localError = this.store.write(this.key, this.record, this.storedRevision);
      this.storedRevision = this.record.revision;
    }
    this.state = this.snapshot();
    for (const listener of this.listeners) listener();
  }

  update(change: Partial<DraftContent> | ((current: DraftContent) => DraftContent)): void {
    const content = typeof change === "function" ? change(this.record.content) : { ...this.record.content, ...change };
    if (equalContent(content, this.record.content)) return;
    this.hasLocalDraft = true;
    if (content.text !== this.record.content.text) this.record.textChanged = true;
    this.record = { ...this.record, content: { text: content.text,
      images: content.images === this.record.content.images ? content.images : [...content.images],
      files: content.files === this.record.content.files ? content.files : [...content.files],
    }, revision: draftRevision() };
    this.publish(); this.schedule();
  }

  receiveLocal(incoming: StoredDraft | null): void {
    if (!incoming || incoming.revision === this.record.revision) return;
    this.hasLocalDraft = true;
    if (this.record.textChanged || this.record.pending) {
      this.store.backup(this.key, this.record);
      if (hasContent(incoming.content)) this.store.backup(this.key, incoming);
      this.publish(false);
      return;
    }
    if ((this.record.content.images.length || this.record.content.files.length)
      && (JSON.stringify(this.record.content.images) !== JSON.stringify(incoming.content.images)
        || JSON.stringify(this.record.content.files) !== JSON.stringify(incoming.content.files))) {
      if (!this.store.backup(this.key, this.record)) {
        this.localError = "本机附件草稿未能备份，当前输入已保留";
        this.publish(false); return;
      }
    }
    this.record = incoming; this.storedRevision = incoming.revision;
    this.publish(false);
  }

  private schedule(): void {
    if (!this.record.textChanged || this.record.pending || this.awaitingOriginSend) return;
    clearTimeout(this.timer);
    this.timer = setTimeout(() => { void this.flush(); }, this.store.delay);
    this.maxTimer ??= setTimeout(() => { void this.flush(); }, this.store.maxDelay);
  }

  refresh(force = false): Promise<void> {
    if (this.awaitingOriginSend) return Promise.resolve();
    if (this.reading) return this.reading;
    if (!force && this.remoteKnown && Date.now() - this.lastRead < 30_000) return Promise.resolve();
    const key = this.key;
    const baseRevision = this.record.baseRevision;
    this.reading = this.store.client.readDraftText(key).then((remote) => {
      // A read started before an acknowledged write cannot replace that write.
      if (key !== this.key || baseRevision !== this.record.baseRevision) return;
      this.lastRead = Date.now(); this.remoteKnown = true; this.remoteError = "";
      if (remote.revision === this.record.baseRevision) { this.publish(this.hasLocalDraft); this.schedule(); return; }
      // Cloud text is a fallback only. Even a previously synced local draft
      // (including an intentional empty draft) takes precedence on this device.
      if (!this.hasLocalDraft && !this.record.pending) {
        this.record.content = { ...this.record.content, text: remote.text };
        this.record.revision = draftRevision();
        this.hasLocalDraft = Boolean(remote.text || remote.revision);
      } else if (remote.text !== this.record.baseText && remote.text !== this.record.content.text) {
        this.backupRemote(remote);
      }
      this.record.baseRevision = remote.revision; this.record.baseText = remote.text;
      this.record.textChanged = this.hasLocalDraft && this.record.content.text !== remote.text;
      this.publish(this.hasLocalDraft); this.schedule();
    }).catch(() => {
      if (key === this.key) { this.remoteError = "文字已存本机，等待同步"; this.publish(false); }
    }).finally(() => { this.reading = null; });
    return this.reading;
  }

  flush(): Promise<void> {
    clearTimeout(this.timer); clearTimeout(this.maxTimer); this.timer = undefined; this.maxTimer = undefined;
    if (this.syncing) return this.syncing;
    if (!this.record.textChanged || this.record.pending || this.awaitingOriginSend) return Promise.resolve();
    const key = this.key;
    this.syncing = (async () => {
      if (!this.remoteKnown) await this.refresh(true);
      if (key !== this.key || !this.remoteKnown || !this.record.textChanged || this.record.pending) return;
      const update = { baseRevision: this.record.baseRevision, revision: this.record.revision, text: this.record.content.text };
      const result = await this.store.client.writeDraftText(key, update);
      if (key !== this.key) return;
      if (!result.saved) this.backupRemote(result.draft);
      this.record.baseRevision = result.draft.revision; this.record.baseText = result.draft.text;
      this.record.textChanged = this.record.content.text !== result.draft.text;
      this.remoteError = "";
      this.publish();
    })().catch(() => {
      if (key === this.key) { this.remoteError = "文字已存本机，等待同步"; this.publish(false); }
    }).finally(() => {
      this.syncing = null;
      this.publish(false);
      if (!this.remoteError) this.schedule();
    });
    this.publish(false);
    return this.syncing;
  }

  private backupRemote(remote: DraftText): void {
    if (!remote.text || remote.text === this.record.content.text) return;
    this.store.backup(this.key, {
      ...freshDraft(), revision: remote.revision || draftRevision(), content: { ...emptyContent(), text: remote.text },
    });
  }

  beginSend(): PendingDraft | null {
    if (this.record.pending) return null;
    this.hasLocalDraft = true;
    const pending = { id: draftRevision(), content: copyContent(this.record.content) };
    this.store.pendingSends.add(pending.id);
    this.record = { ...this.record, pending, content: emptyContent(), revision: draftRevision(), textChanged: true };
    this.publish();
    return pending;
  }

  finishSend(id: string, succeeded: boolean): void {
    const pending = this.record.pending;
    if (!pending || pending.id !== id) return;
    if (!succeeded) this.record.content = mergeContent(pending.content, this.record.content);
    this.record.pending = undefined; this.record.revision = draftRevision(); this.record.textChanged = true;
    this.store.pendingSends.delete(id);
    this.publish(); this.schedule();
    const inFlight = this.syncing;
    for (const origin of this.origins) {
      void Promise.resolve(inFlight).then(() => this.store.finishOrigin(origin.key, origin.pending, succeeded));
    }
    this.origins = [];
  }

  moveTo(key: string, origin: string, pending?: PendingDraft): void {
    if (pending) this.origins.push({ key: origin, pending });
    this.hasLocalDraft = true;
    this.key = key; this.remoteKnown = false;
    this.record = { ...this.record, revision: draftRevision(), baseRevision: "", baseText: "", textChanged: true };
    this.storedRevision = ""; this.publish();
  }

  removePendingRecovery(id: string): void {
    this.record.recoveries = this.record.recoveries?.filter((entry) => entry.id !== id);
    this.publish(); this.schedule();
  }

  refreshRecoveries(): void { this.publish(false); }
}

const stores = new Map<string, ComposerDraftStore>();

export function composerDraftsFor(client: ApplicationClient, agentDir: string): ComposerDraftStore {
  const namespace = JSON.stringify([client.kind, client.endpoint, agentDir]);
  let store = stores.get(namespace);
  if (!store) { store = new ComposerDraftStore(client, namespace); stores.set(namespace, store); }
  store.client = client;
  return store;
}

export function composerDraftKey(sessionId: string | null, cwd: string): string {
  return sessionId ? `session:${sessionId}` : `new:${cwd}`;
}
