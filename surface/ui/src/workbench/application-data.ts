import type {
  AgentMessage, AgentReadVersion, ApplicationClient, ApplicationEventEnvelope, ApplicationOverview,
  ApplicationReadRequest, ApplicationResources, ApplicationSnapshot, EventSubscription, HistoryItem,
  EntryNode, HistoryPage, LiveSnapshot, NodesPage, ReadMessage, ReadResourceRequest, ResourceResponse, ResourceRevision,
  ProjectDirectoryPage, ProjectInfo, SessionDirectoryPage, SessionInfo, SessionMetadata, SessionResources, SessionRuntimeState, SessionVersion,
} from "../contracts";
import { ResourceCache, retainedBytes } from "./resource-cache.ts";

const pageLimit = 128;
const pageBytes = 512 * 1024;
const maxWindowPages = 3;
const projectLimit = 8;
const directoryLimit = 8;
const directoryMoreLimit = 20;
const revalidateAfter = 30_000;
const maxBufferedBytes = 8 * 1024 * 1024;

export interface HistoryWindow {
  pages: HistoryPage[];
  version: SessionVersion;
  anchorId: string;
  start: number;
  end: number;
  total: number;
  older?: string;
  newer?: string;
  following: boolean;
  latestAvailable: boolean;
  preview?: boolean;
}

export interface NavigationIndex {
  version: SessionVersion;
  leafId: string;
  items: { id: string; label: string; position: number }[];
}

interface PendingReadMessage extends ReadMessage { entryId?: string; generation?: string }

export interface SessionReadView {
  metadata: SessionMetadata;
  live: LiveSnapshot;
  window: HistoryWindow;
  pending: PendingReadMessage[];
  controls?: { generation: string; controls: number };
}

export interface DirectoryWindow {
  pages: SessionDirectoryPage[];
  sessions: SessionInfo[];
  overrides: Record<string, SessionInfo | null>;
  total: number;
  start: number;
  next?: string;
}

interface ProjectDirectoryWindow {
  pages: ProjectDirectoryPage[];
  projects: ProjectInfo[];
  next?: string;
}

const projectDirectoryKey = '["project-directory"]';

export interface ScrollPosition { entryId: string; offset: number; bottom: boolean }
export interface EditDraft { text: string; original: string; contentToken?: string }

type BufferedEvent = { envelope: ApplicationEventEnvelope } | { connected: string };
type Listener = () => void;

function queryKey(query: object): string {
  return JSON.stringify(Object.fromEntries(Object.entries(query).filter(([, value]) => value !== undefined).sort(([a], [b]) => a.localeCompare(b))));
}

function resourceKey(id: string, kind: string, query: object = {}): string { return JSON.stringify(["session", id, kind, queryKey(query)]); }
function applicationKey(kind: string, query: object = {}): string { return JSON.stringify(["application", kind, queryKey(query)]); }
function navigationKey(id: string): string { return JSON.stringify(["navigation", id]); }
function viewKey(id: string): string { return JSON.stringify(["view", id]); }
function directoryKey(project: string, search: string): string { return JSON.stringify(["directory", project, search.trim().toLowerCase()]); }
function entityKey(id: string, generation: string, entryId: string): string { return JSON.stringify(["entry", id, generation, entryId]); }
function sameVersion(a: SessionVersion | undefined, b: SessionVersion | undefined): boolean {
  return Boolean(a && b && a.generation === b.generation && a.log === b.log && a.view === b.view);
}
function cursorParts(cursor: string): [string, number] {
  const index = cursor.lastIndexOf(":");
  return [cursor.slice(0, index), Number(cursor.slice(index + 1))];
}
function isAfter(cursor: string, previous: string): boolean {
  if (!previous) return true;
  const [epoch, sequence] = cursorParts(cursor), [oldEpoch, oldSequence] = cursorParts(previous);
  return epoch === oldEpoch && sequence > oldSequence;
}
function errorText(error: unknown): string { return error instanceof Error ? error.message : String(error); }
function isConflict(error: unknown): boolean {
  return Boolean(error && typeof error === "object" && "status" in error && error.status === 409)
    || /cursor is unavailable|source changed|session changed/.test(errorText(error));
}
function isMissing(error: unknown): boolean {
  return Boolean(error && typeof error === "object" && "status" in error && error.status === 404)
    || /session not found|session does not exist/.test(errorText(error));
}
function newerRevision(value: ResourceRevision | undefined, current: ResourceRevision | undefined): boolean {
  return Boolean(value && (!current || value.generation === current.generation && value.revision > current.revision));
}
function makeWindow(pages: HistoryPage[], following: boolean, latestAvailable = false): HistoryWindow {
  const first = pages[0]!, last = pages[pages.length - 1]!;
  return { pages, version: last.version, anchorId: last.anchorId, start: first.start, end: last.end, total: last.total,
    older: first.older, newer: last.newer, following, latestAvailable };
}
function appendHistoryPages(pages: HistoryPage[], tail: HistoryPage): HistoryPage[] {
  const previous = pages.at(-1);
  if (previous && previous.end === tail.start && previous.items.length + tail.items.length <= pageLimit
    && retainedBytes(previous.items) + retainedBytes(tail.items) <= pageBytes) {
    return [...pages.slice(0, -1), { ...tail, start: previous.start, older: previous.older, items: [...previous.items, ...tail.items] }];
  }
  return [...pages, tail].slice(-maxWindowPages);
}
function projectMatches(project: string, value: SessionInfo): boolean {
  const clean = (path: string) => path.replace(/\\/g, "/").replace(/\/+$/, "");
  return !project || clean(value.projectRoot || value.cwd) === clean(project);
}
function searchMatches(search: string, value: SessionInfo): boolean {
  return `${value.name ?? ""}\n${value.firstMessage}\n${value.id}`.toLowerCase().includes(search.trim().toLowerCase());
}

// One cache and synchronization owner per ApplicationClient/data source. This
// layer owns only evictable presentation data; all identities and versions come
// from Application, AgentSession and SessionManager.
export class ApplicationDataStore {
  readonly cache: ResourceCache;
  readonly drafts = new Map<string, EditDraft>();
  readonly disclosures = new Map<string, boolean>();
  readonly editing = new Map<string, string>();
  readonly scroll = new Map<string, ScrollPosition>();
  readonly lastScroll = new Map<string, ScrollPosition>();
  private readonly client: ApplicationClient;
  private readonly listeners = new Set<Listener>();
  private readonly eventListeners = new Set<(event: ApplicationEventEnvelope) => void>();
  private readonly scopes = new Map<symbol, string>();
  private readonly scopePins = new Map<symbol, () => void>();
  private readonly navigationFlights = new Map<string, Promise<void>>();
  private readonly sessionFlights = new Map<string, Promise<SessionReadView>>();
  private readonly dirtySessions = new Set<string>();
  private readonly summaryReads = new Set<string>();
  private readonly directoryQueries = new Map<string, { project: string; search: string }>();
  private readonly visibleDirectories = new Map<string, number>();
  private subscription: EventSubscription | null = null;
  private streamSerial = 0;
  private lifecycle = 0;
  private running = false;
  private cursor = "";
  private buffer: BufferedEvent[] = [];
  private bufferBytes = 0;
  private holds = 0;
  private overflowed = false;
  private recovering: Promise<void> | null = null;
  private recoveryTimer: ReturnType<typeof setTimeout> | null = null;
  private recoveryAttempts = 0;
  private needsBaseline = false;
  private recoveryAgain = false;
  private refreshTimer: ReturnType<typeof setTimeout> | null = null;
  private validationTimer: ReturnType<typeof setInterval> | null = null;
  private revision = 0;
  private configurationRevision = 0;
  private overview: ApplicationOverview | null = null;
  private snapshotValue: ApplicationSnapshot | null = null;
  private error = "";

  constructor(client: ApplicationClient, budget?: number) {
    this.client = client; this.cache = new ResourceCache(budget);
    this.cache.pin(projectDirectoryKey);
  }
  get version(): number { return this.revision; }
  get configurationVersion(): number { return this.configurationRevision; }
  get lastAppliedCursor(): string { return this.cursor; }
  get lastError(): string { return this.error; }
  get snapshot(): ApplicationSnapshot | null { return this.snapshotValue; }
  get hasMoreProjects(): boolean { return Boolean(this.cache.peek<ProjectDirectoryWindow>(projectDirectoryKey)?.next); }
  observe(listener: Listener): () => void { this.listeners.add(listener); return () => this.listeners.delete(listener); }
  observeEvents(listener: (event: ApplicationEventEnvelope) => void): () => void { this.eventListeners.add(listener); return () => this.eventListeners.delete(listener); }
  isMissing(id: string): boolean { return this.cache.peek(JSON.stringify(["missing", id])) === true; }
  session(id: string): SessionReadView | undefined { return this.cache.peek(viewKey(id)); }
  navigation(id: string): NavigationIndex | undefined { return this.cache.peek(navigationKey(id)); }
  directory(project: string, search = ""): DirectoryWindow | undefined { return this.cache.peek(directoryKey(project, search)); }
  watchDirectory(project: string, search = ""): () => void {
    const key = directoryKey(project, search);
    this.visibleDirectories.set(key, (this.visibleDirectories.get(key) ?? 0) + 1);
    const unpin = this.cache.pin(key);
    return () => {
      const count = this.visibleDirectories.get(key) ?? 0;
      if (count > 1) this.visibleDirectories.set(key, count - 1); else this.visibleDirectories.delete(key);
      unpin();
    };
  }
  draftKey(id: string, generation: string, entryId: string): string { return JSON.stringify([id, generation, entryId]); }

  async start(): Promise<void> {
    if (this.running) return;
    this.running = true;
    const lifecycle = ++this.lifecycle;
    try {
      if (!this.cursor) {
        const result = await this.readApplication("overview", {}, true);
        if (lifecycle !== this.lifecycle) return;
        if (!result.cursor) throw new Error("服务端未返回事件同步起点");
        this.acceptOverview(result.data);
        this.cursor = result.cursor;
        this.rebuildSnapshot();
      }
      if (lifecycle !== this.lifecycle) return;
      if (this.needsBaseline) await this.recover(); else this.openStream();
      this.validationTimer = setInterval(() => { void this.validateVisible().catch((error) => this.fail(error)); }, revalidateAfter);
    } catch (error) { this.running = false; this.fail(error); throw error; }
  }

  stop(): void {
    this.running = false;
    this.lifecycle++;
    this.streamSerial++;
    this.subscription?.close();
    this.subscription = null;
    if (this.validationTimer) clearInterval(this.validationTimer);
    if (this.refreshTimer) clearTimeout(this.refreshTimer);
    if (this.recoveryTimer) clearTimeout(this.recoveryTimer);
    this.recoveryTimer = null;
    this.validationTimer = this.refreshTimer = null;
    // Data and its applied cursor survive a remount together. In-flight reads
    // are invalidated so a stopped connection cannot publish an old baseline.
    this.cache.invalidate((key) => key.startsWith('["session"') || key.startsWith('["application"'));
    this.buffer = [];
    this.bufferBytes = 0;
    this.sessionFlights.clear();
    this.navigationFlights.clear();
  }

  select(owner: symbol, id: string | null): void {
    const previous = this.scopes.get(owner);
    if (previous === id || !previous && !id) return;
    this.scopePins.get(owner)?.();
    this.scopePins.delete(owner);
    if (id) this.cache.forget(JSON.stringify(["missing", id]));
    if (id) {
      this.scopes.set(owner, id);
      const releaseView = this.cache.pin(viewKey(id)), releaseNavigation = this.cache.pin(navigationKey(id));
      this.scopePins.set(owner, () => { releaseView(); releaseNavigation(); });
      // Details omitted by the previous scope must be validated, even when a
      // global cursor is already at the current event-stream frontier.
      this.invalidateSession(id, false);
    } else this.scopes.delete(owner);
    if (this.running) {
      if (this.recovering) this.recoveryAgain = true;
      else if (this.needsBaseline) void this.recover().catch((error) => this.fail(error));
      else this.openStream();
    }
    if (id) this.schedule();
  }

  private selectedIDs(): string[] { return [...new Set(this.scopes.values())].sort(); }

  private openStream(after = this.cursor): void {
    if (!this.running) return;
    const serial = ++this.streamSerial;
    this.subscription?.close();
    this.subscription = this.client.subscribe(after, {
      onEvent: (envelope) => { if (serial === this.streamSerial) this.receive({ envelope }); },
      onConnected: (cursor) => { if (serial === this.streamSerial) this.receive({ connected: cursor }); },
      onReset: (_revision, reason) => {
        if (serial !== this.streamSerial) return;
        // Keep the old checkpoint until a new resource baseline is installed.
        void this.recover(reason).catch((error) => this.fail(error));
      },
    }, this.selectedIDs());
    void this.subscription.ready.catch((error) => {
      if (serial !== this.streamSerial) return;
      this.fail(error);
      void this.recover().catch((failure) => this.fail(failure));
    });
  }

  private receive(value: BufferedEvent): void {
    if (this.holds > 0) {
      this.bufferBytes += retainedBytes(value);
      if (this.bufferBytes > maxBufferedBytes || this.buffer.length >= 2048) {
        this.overflowed = true;
        this.buffer = [];
      } else if (!this.overflowed) this.buffer.push(value);
      return;
    }
    if (this.needsBaseline) return;
    if ("connected" in value) {
      const [epoch] = cursorParts(value.connected), [previousEpoch] = cursorParts(this.cursor);
      if (this.cursor && epoch !== previousEpoch) { void this.recover().catch((error) => this.fail(error)); return; }
      if (isAfter(value.connected, this.cursor)) this.cursor = value.connected;
      this.error = "";
      this.emit();
      return;
    }
    const envelope = value.envelope;
    if (!envelope.cursor) { void this.recover("事件缺少同步游标").catch((error) => this.fail(error)); return; }
    const [epoch] = cursorParts(envelope.cursor), [previousEpoch] = cursorParts(this.cursor);
    if (this.cursor && epoch !== previousEpoch) { void this.recover().catch((error) => this.fail(error)); return; }
    if (!isAfter(envelope.cursor, this.cursor)) return;
    this.applyEvent(envelope);
    this.cursor = envelope.cursor;
    for (const listener of this.eventListeners) listener(envelope);
    this.emit();
    this.schedule();
  }

  private hold(): () => void {
    this.holds++;
    return () => {
      this.holds--;
      if (this.holds !== 0) return;
      const buffered = this.buffer;
      this.buffer = [];
      this.bufferBytes = 0;
      if (this.overflowed) {
        this.overflowed = false;
        void this.recover("读取期间的事件过多，正在校验当前资源").catch((error) => this.fail(error));
      } else for (const value of buffered) this.receive(value);
    };
  }

  private recover(reason?: string): Promise<void> {
    this.needsBaseline = true;
    if (this.recovering) { this.recoveryAgain = true; return this.recovering; }
    if (!this.running) return Promise.resolve();
    if (this.recoveryTimer) clearTimeout(this.recoveryTimer);
    this.recoveryTimer = null;
    this.subscription?.close();
    this.subscription = null;
    this.streamSerial++;
    this.error = reason ?? "正在恢复同步";
    this.buffer = [];
    this.bufferBytes = 0;
    this.overflowed = this.recoveryAgain = false;
    const lifecycle = this.lifecycle;
    const release = this.hold();
    const work = (async () => {
      this.cache.invalidate((key) => key.startsWith('["application"') || key.startsWith('["session"'));
      const selected = this.selectedIDs();
      for (const id of selected) this.dirtySessions.add(id);
      const baseline = await this.readApplication("overview", {}, true);
      if (lifecycle !== this.lifecycle || !this.running) return;
      if (!baseline.cursor) throw new Error("服务端未返回恢复起点");
      // Subscribe at the candidate baseline, while keeping the applied cursor
      // unchanged until all selected resources have successfully validated.
      this.openStream(baseline.cursor);
      this.acceptOverview(baseline.data);
      const results = await Promise.allSettled([
        ...selected.map((id) => this.loadSession(id, true)), this.refreshLoadedDirectories(),
      ]);
      if (lifecycle !== this.lifecycle || !this.running) return;
      const failure = results.find((result) => result.status === "rejected" && !isMissing(result.reason));
      if (failure?.status === "rejected") throw failure.reason;
      if (this.recoveryAgain || this.overflowed) return;
      this.cursor = baseline.cursor;
      this.needsBaseline = false;
      this.recoveryAttempts = 0;
      this.rebuildSnapshot();
      this.error = "";
    })();
    const done = work.finally(() => {
      if (this.recovering === done) this.recovering = null;
      if (this.needsBaseline && lifecycle === this.lifecycle) {
        this.subscription?.close();
        this.subscription = null;
        this.streamSerial++;
        this.buffer = [];
        this.bufferBytes = 0;
        this.overflowed = false;
        if (this.running) {
          const delay = this.recoveryAgain ? 0 : Math.min(5000, 500 * 2 ** Math.min(this.recoveryAttempts++, 4));
          this.recoveryTimer = setTimeout(() => {
            this.recoveryTimer = null;
            void this.recover().catch((error) => this.fail(error));
          }, delay);
        }
      }
      release();
      this.emit();
    });
    this.recovering = done;
    return done;
  }

  private readApplication<K extends keyof ApplicationResources>(kind: K, query: ApplicationReadRequest = {}, force = false): Promise<ResourceResponse<ApplicationResources[K]> & { data: ApplicationResources[K] }> {
    return this.cache.read(applicationKey(kind, query), (known) => this.client.readApplicationResource(kind, query, known), { force });
  }

  read<K extends keyof SessionResources>(id: string, kind: K, query: ReadResourceRequest = {}, force = false): Promise<ResourceResponse<SessionResources[K]> & { data: SessionResources[K] }> {
    return this.cache.read(resourceKey(id, kind, query), (known) => this.client.readResource(id, kind, query, known), { force });
  }

  async cached<T>(scope: string, id: string, load: () => Promise<T>, force = false): Promise<T> {
    const result = await this.cache.read(JSON.stringify(["configuration", scope, id]), async () => ({ token: "", data: await load() }), { force, maxAge: revalidateAfter });
    return result.data;
  }

  invalidateConfiguration(id?: string): void {
    this.cache.invalidate((key) => key.startsWith('["configuration"') && (!id || key.includes(JSON.stringify(id))));
    this.configurationRevision++;
  }

  async refreshOverview(force = false): Promise<ApplicationSnapshot> {
    const result = await this.readApplication("overview", {}, force);
    this.acceptOverview(result.data);
    this.rebuildSnapshot();
    this.emit();
    return this.snapshotValue!;
  }

  private acceptOverview(value: ApplicationOverview): void {
    this.overview = value;
    if (value.directory) this.installProjectPage(value.directory, false);
  }

  private installProjectPage(page: ProjectDirectoryPage, more: boolean): void {
    const previous = this.cache.peek<ProjectDirectoryWindow>(projectDirectoryKey);
    let pages = more && previous ? [...previous.pages, page] : [page];
    if (!more && previous) pages = previous.pages[0]?.version === page.version ? [page, ...previous.pages.slice(1)] : previous.pages;
    const projects = new Map(pages.flatMap((item) => item.projects).map((item) => [item.project.path, item.project]));
    for (const item of page.projects) {
      projects.set(item.project.path, item.project);
      const key = directoryKey(item.project.path, "");
      const old = this.directory(item.project.path);
      const head = item.sessions;
      const window = !old ? this.directoryWindow([head], {})
        : old.pages[0]?.version === head.version ? this.directoryWindow([head, ...old.pages.slice(1)], old.overrides)
          : this.directoryWindow(old.pages, { ...old.overrides, ...Object.fromEntries(head.sessions.map((info) => [info.id, info])) });
      this.directoryQueries.set(key, { project: item.project.path, search: "" });
      this.cache.put(key, window);
      this.cache.put(applicationKey("directory", { project: item.project.path, search: "", limit: directoryLimit }), head);
    }
    this.cache.put(projectDirectoryKey, { pages, projects: [...projects.values()].sort((a, b) => b.modified.localeCompare(a.modified) || a.path.localeCompare(b.path)),
      next: pages.at(-1)?.next ?? (page.total > projects.size ? page.next : undefined) } satisfies ProjectDirectoryWindow);
  }

  async loadProjects(): Promise<void> {
    const previous = this.cache.peek<ProjectDirectoryWindow>(projectDirectoryKey);
    if (previous && !previous.next) return;
    try {
      const page = (await this.readApplication("projects", { limit: projectLimit, sessionLimit: directoryLimit, ...(previous?.next ? { cursor: previous.next } : {}) })).data;
      this.installProjectPage(page, Boolean(previous));
    } catch (error) {
      if (!isConflict(error)) throw error;
      this.cache.forget(projectDirectoryKey);
      let next: string | undefined;
      do {
        const page = (await this.readApplication("projects", { limit: projectLimit, sessionLimit: directoryLimit, ...(next ? { cursor: next } : {}) }, true)).data;
        this.installProjectPage(page, Boolean(next)); next = page.next;
      } while (next && (this.cache.peek<ProjectDirectoryWindow>(projectDirectoryKey)?.projects.length ?? 0) < (previous?.projects.length ?? projectLimit) + projectLimit);
    }
    this.rebuildSnapshot(); this.emit();
  }

  async loadDirectory(project: string, search = "", more = false, force = false): Promise<DirectoryWindow> {
    const key = directoryKey(project, search), previous = this.directory(project, search);
    for (const candidate of this.directoryQueries.keys()) if (!this.cache.peek(candidate) && !this.visibleDirectories.has(candidate)) this.directoryQueries.delete(candidate);
    this.directoryQueries.set(key, { project, search });
    if (!more && !force && previous && this.cache.isFresh(applicationKey("directory", { project, search, limit: directoryLimit }), revalidateAfter)) return previous;
    const cursor = more ? previous?.next : undefined;
    if (more && !cursor && previous) return previous;
    let page: SessionDirectoryPage;
    try { page = (await this.readApplication("directory", { project, search, limit: cursor ? directoryMoreLimit : directoryLimit, ...(cursor ? { cursor } : {}) }, force)).data; }
    catch (error) { if (more && isConflict(error)) {
      const pages: SessionDirectoryPage[] = [];
      let next: string | undefined;
      do {
        const result = (await this.readApplication("directory", { project, search, limit: next ? directoryMoreLimit : directoryLimit, ...(next ? { cursor: next } : {}) }, true)).data;
        pages.push(result); next = result.next;
      } while (next && pages.reduce((count, item) => count + item.sessions.length, 0) < (previous?.sessions.length ?? directoryLimit));
      const value = this.directoryWindow(pages, {});
      this.cache.put(key, value); this.rebuildSnapshot(); this.emit(); return value;
    } throw error; }
    let pages = cursor && previous ? [...previous.pages, page] : [page];
    let overrides = cursor && previous ? previous.overrides : {};
    if (!cursor && previous) {
      if (previous.pages[0]?.version === page.version) pages = previous.pages;
      else {
        pages = previous.pages;
        overrides = { ...previous.overrides };
        for (const info of page.sessions) overrides[info.id] = info;
      }
    }
    const value = this.directoryWindow(pages, overrides);
    this.cache.put(key, value);
    this.rebuildSnapshot();
    this.emit();
    return value;
  }

  private directoryWindow(pages: SessionDirectoryPage[], overrides: Record<string, SessionInfo | null>): DirectoryWindow {
    const byID = new Map(pages.flatMap((page) => page.sessions).map((session) => [session.id, session]));
    for (const [id, value] of Object.entries(overrides)) { if (value) byID.set(id, value); else byID.delete(id); }
    const sessions = [...byID.values()].sort((a, b) => b.modified.localeCompare(a.modified) || a.id.localeCompare(b.id));
    const first = pages[0]!, last = pages[pages.length - 1]!;
    return { pages, sessions, overrides, total: last.total, start: first.start, next: last.next };
  }

  private patchSummary(id: string, info: SessionInfo | null): void {
    for (const [key, query] of this.directoryQueries) {
      const value = this.cache.peek<DirectoryWindow>(key);
      if (!value) { this.directoryQueries.delete(key); continue; }
      const present = value.sessions.some((session) => session.id === id);
      // Retain updates for loaded rows. New rows are admitted through the
      // directory head, so unrelated session events cannot grow this map.
      if (!present) { this.cache.invalidate(applicationKey("directory", { ...query, limit: directoryLimit })); continue; }
      const match = info && projectMatches(query.project, info) && searchMatches(query.search, info);
      this.cache.put(key, this.directoryWindow(value.pages, { ...value.overrides, [id]: match ? info : null }));
    }
  }

  private rebuildSnapshot(): void {
    if (!this.overview) return;
    const sessions = new Map<string, SessionInfo>();
    for (const key of this.directoryQueries.keys()) {
      for (const value of this.cache.peek<DirectoryWindow>(key)?.sessions ?? []) sessions.set(value.id, value);
    }
    for (const id of this.selectedIDs()) {
      const view = this.session(id);
      if (view) sessions.set(id, view.metadata.info);
    }
    this.snapshotValue = { revision: cursorParts(this.cursor)[1] || 0, cursor: this.cursor, ...this.overview,
      projects: this.cache.peek<ProjectDirectoryWindow>(projectDirectoryKey)?.projects ?? this.overview.projects, sessions: [...sessions.values()] };
  }

  invalidateSession(id: string, history = true): void {
    this.cache.invalidate(resourceKey(id, "live"));
    if (history) this.cache.invalidate(resourceKey(id, "metadata"));
    if (this.session(id) || this.selectedIDs().includes(id)) this.dirtySessions.add(id);
  }

  loadSession(id: string, force = false): Promise<SessionReadView> {
    if (force) this.invalidateSession(id);
    const pending = this.sessionFlights.get(id);
    if (pending) return pending;
    const current = this.session(id);
    if (!force && current && !this.dirtySessions.has(id) && this.cache.isFresh(resourceKey(id, "metadata"), revalidateAfter) && this.cache.isFresh(resourceKey(id, "live"), revalidateAfter)) return Promise.resolve(current);
    const release = this.hold(), lifecycle = this.lifecycle;
    const work = this.readSessionBaseline(id).then((view) => {
      if (lifecycle !== this.lifecycle) return view;
      this.cache.forget(JSON.stringify(["missing", id]));
      for (const page of view.window.pages) for (const item of page.items) {
        const key = this.draftKey(id, view.window.version.generation, item.node.id), draft = this.drafts.get(key);
        if (draft && !draft.contentToken && item.node.contentToken) this.drafts.set(key, { ...draft, contentToken: item.node.contentToken });
      }
      this.cache.put(viewKey(id), view);
      if (this.selectedIDs().includes(id)) {
        if (view.live.active) void this.ensureNavigation(id).catch((error) => this.fail(error));
        void this.restoreEditingSource(id, view.window.version.generation).catch((error) => this.fail(error));
      }
      this.dirtySessions.delete(id);
      this.patchSummary(id, view.metadata.info);
      this.rebuildSnapshot();
      this.error = "";
      this.emit();
      return view;
    }).catch((error) => {
      if (lifecycle === this.lifecycle && isMissing(error)) this.markMissing(id);
      throw error;
    });
    const done = work.finally(() => { if (this.sessionFlights.get(id) === done) this.sessionFlights.delete(id); release(); });
    this.sessionFlights.set(id, done);
    return done;
  }

  private async readSessionBaseline(id: string): Promise<SessionReadView> {
    for (let attempt = 0; attempt < 3; attempt++) {
      const [metadataResult, liveResult] = await Promise.all([this.read(id, "metadata"), this.read(id, "live")]);
      const metadata = metadataResult.data, live = liveResult.data;
      const head = live.active && live.history ? live.history : { ...metadata.version, leafId: metadata.leafId };
      if (metadata.version.generation !== head.generation) { this.invalidateSession(id); continue; }
      const old = this.session(id);
      let window = old?.window;
      try {
        if (!window || window.version.generation !== head.generation) {
          this.cache.forget(navigationKey(id));
          window = makeWindow([await this.history(id, { at: head, leafId: head.leafId ?? "" })], true);
        } else if (window.preview) {
          window = { ...window, latestAvailable: true };
        } else if (!sameVersion(window.version, head) || window.anchorId !== (head.leafId ?? "")) {
          const tail = await this.history(id, { at: head, leafId: head.leafId ?? "", afterId: window.anchorId });
          if (!tail.connected) { this.cache.forget(navigationKey(id)); window = makeWindow([tail], true); }
          else if (!window.following) window = { ...window, latestAvailable: tail.total > window.total || tail.anchorId !== window.anchorId };
          else if (tail.start === window.end && window.end === window.total) {
            if (tail.items.length) window = makeWindow(appendHistoryPages(window.pages, tail), true);
            else window = { ...window, version: tail.version, anchorId: tail.anchorId, total: tail.total };
          } else window = makeWindow([tail], true);
        }
      } catch (error) {
        if (isConflict(error) && attempt < 2) { this.invalidateSession(id); continue; }
        throw error;
      }
      this.forgetOldGeneration(id, head.generation);
      return { metadata, live, window, pending: [...(live.pending ?? [])], controls: old?.controls };
    }
    throw new Error("会话正在切换，读取尚未稳定");
  }

  private forgetOldGeneration(id: string, generation: string): void {
    const old = this.session(id);
    if (!old || old.metadata.version.generation === generation) return;
    this.cache.forget((key) => {
      const parts: unknown[] = JSON.parse(key);
      if (parts[1] !== id) return false;
      if (parts[0] === "entry") return parts[2] !== generation;
      if (parts[0] !== "session" || parts[2] === "metadata" || parts[2] === "live") return false;
      return !key.includes(generation);
    });
    this.invalidateConfiguration(id);
  }

  private async history(id: string, query: ReadResourceRequest): Promise<HistoryPage> {
    const page = (await this.read(id, "history", { limit: pageLimit, maxBytes: pageBytes, ...query })).data;
    const items = page.items.map((item) => {
      const key = entityKey(id, page.version.generation, item.node.id);
      const cached = this.cache.peek<AgentMessage>(key);
      if (cached) return { ...item, message: cached };
      if (item.message) this.cache.put(key, item.message);
      return item;
    });
    return { ...page, items };
  }

  async pageHistory(id: string, direction: "older" | "newer" | "latest", aroundId?: string): Promise<void> {
    const value = await this.loadSession(id);
    const previous = value.window;
    let page: HistoryPage;
    let preview = false;
    if (direction === "latest" || aroundId) {
      const head = value.live.active && value.live.history ? value.live.history : { ...value.metadata.version, leafId: value.metadata.leafId };
      try { page = await this.history(id, { at: head, leafId: head.leafId ?? "", ...(aroundId ? { aroundId } : {}) }); }
      catch (error) {
        if (!aroundId || !/outside the selected path|entry not found/.test(errorText(error))) throw error;
        page = await this.history(id, { at: head, leafId: aroundId, aroundId });
        preview = true;
      }
    } else {
      let cursor = direction === "older" ? previous.older : previous.newer;
      if (!cursor && direction === "newer" && previous.end < previous.total) {
        const last = previous.pages.at(-1)?.items.at(-1)?.node.id;
        if (last) cursor = (await this.history(id, { at: previous.version, leafId: previous.anchorId, aroundId: last, limit: 1 })).newer;
      }
      if (cursor) page = await this.history(id, { cursor });
      else if (direction === "newer" && previous.latestAvailable && !previous.preview) {
        const head = value.live.active && value.live.history ? value.live.history : { ...value.metadata.version, leafId: value.metadata.leafId };
        const afterId = previous.pages.at(-1)?.items.at(-1)?.node.id;
        page = await this.history(id, { at: head, leafId: head.leafId ?? "", afterId, direction: "newer" });
        if (!page.connected) { this.invalidateSession(id); return; }
      } else return;
    }
    const current = this.session(id);
    if (!current || current.window !== previous) return;
    let pages: HistoryPage[];
    if (direction === "latest" || aroundId) pages = [page];
    else if (direction === "older" && page.end === previous.start) pages = [page, ...previous.pages].slice(0, maxWindowPages);
    else if (direction === "newer" && page.start === previous.end) pages = [...previous.pages, page].slice(-maxWindowPages);
    else return;
    const advanced = direction === "newer" && page.version.log > previous.version.log;
    const window = makeWindow(pages, direction !== "older" && !aroundId && page.end === page.total, previous.latestAvailable && direction !== "latest" && !advanced);
    window.preview = preview || direction !== "latest" && previous.preview;
    if (preview) window.latestAvailable = true;
    // An older cursor may refer to an ancestor snapshot from before an append.
    window.total = direction === "latest" || aroundId || advanced ? page.total : previous.total;
    window.version = direction === "latest" || aroundId || advanced ? page.version : previous.version;
    window.anchorId = direction === "latest" || aroundId || advanced ? page.anchorId : previous.anchorId;
    this.cache.put(viewKey(id), { ...current, window });
    this.emit();
  }

  async loadEntry(id: string, generation: string, entryId: string): Promise<AgentMessage | null> {
    const result = await this.read(id, "entry", { generation, entryId });
    if (result.data.message) this.cache.put(entityKey(id, generation, entryId), result.data.message);
    this.emit();
    return result.data.message;
  }

  messageFor(id: string, generation: string, item: HistoryItem): AgentMessage | undefined {
    return this.cache.peek<AgentMessage>(entityKey(id, generation, item.node.id)) ?? item.message;
  }

  async nodes(id: string, query: ReadResourceRequest = {}): Promise<NodesPage> {
    // Fixed cursors stay reusable; the initial node index is invalidated only
    // by log/source changes, independently of the currently rendered history.
    return (await this.read(id, "nodes", { limit: 80, ...query })).data;
  }

  ensureNavigation(id: string): Promise<void> {
    const view = this.session(id);
    if (!view) return Promise.resolve();
    const head = view.live.active && view.live.history ? view.live.history : { ...view.metadata.version, leafId: view.metadata.leafId };
    const previous = this.navigation(id), leafId = head.leafId ?? "";
    if (previous?.version.generation === head.generation && previous.leafId === leafId) return Promise.resolve();
    const key = JSON.stringify([id, head.generation, head.log, leafId]);
    const running = this.navigationFlights.get(key);
    if (running) return running;
    const lifecycle = this.lifecycle;
    const afterId = previous?.version.generation === head.generation ? previous.leafId : "";
    const collect = async (filter: ReadResourceRequest): Promise<{ items: EntryNode[]; connected: boolean }> => {
      const items: EntryNode[] = [];
      let cursor: string | undefined, connected = true;
      do {
        const page = await this.nodes(id, { ...filter, leafId, ...(afterId ? { afterId } : {}), limit: 256,
          ...(cursor ? { cursor } : { at: head }) });
        items.push(...page.items); connected = page.connected;
        cursor = page.next;
        if (lifecycle !== this.lifecycle) return { items: [], connected: false };
      } while (cursor);
      return { items, connected };
    };
    const work = (async () => {
      const [users, compactions] = await Promise.all([collect({ role: "user" }), collect({ entryType: "compaction" })]);
      const current = this.session(id), currentHead = current?.live.history ?? current?.metadata.version;
      const currentLeaf = current?.live.history ? current.live.history.leafId ?? "" : current?.metadata.leafId ?? "";
      if (lifecycle !== this.lifecycle || currentHead?.generation !== head.generation || currentLeaf !== leafId) return;
      const base = previous && afterId && users.connected && compactions.connected ? previous.items : [];
      const added = [...users.items, ...compactions.items].map((node) => ({ id: node.id, label: node.preview, position: node.position ?? 0 }));
      const byID = new Map([...base, ...added].map((item) => [item.id, item]));
      this.cache.put(navigationKey(id), { version: head, leafId, items: [...byID.values()].sort((a, b) => a.position - b.position) } satisfies NavigationIndex);
      this.emit();
    })();
    const done = work.finally(() => { if (this.navigationFlights.get(key) === done) this.navigationFlights.delete(key); });
    this.navigationFlights.set(key, done);
    return done;
  }

  private async restoreEditingSource(id: string, generation: string): Promise<void> {
    const scope = JSON.stringify([id, generation]);
    if (this.editing.has(scope)) return;
    for (const [oldScope, entryId] of [...this.editing].reverse()) {
      const [owner, oldGeneration] = JSON.parse(oldScope) as [string, string];
      if (owner !== id || oldGeneration === generation) continue;
      const oldKey = this.draftKey(id, oldGeneration, entryId), draft = this.drafts.get(oldKey);
      if (!draft?.contentToken) continue;
      const current = (await this.read(id, "entry", { generation, entryId })).data;
      if (this.session(id)?.window.version.generation !== generation || this.editing.has(scope)) return;
      // A Runtime admission can change the read generation while retaining
      // the exact entry. Match its full raw identity, never just displayed text.
      if (current.contentToken !== draft.contentToken) return;
      this.drafts.set(this.draftKey(id, generation, entryId), this.drafts.get(oldKey) ?? draft);
      this.editing.set(scope, entryId);
      this.drafts.delete(oldKey); this.editing.delete(oldScope);
      this.emit();
      return;
    }
  }

  private markMissing(id: string): void {
    this.patchSummary(id, null);
    this.cache.forget((key) => JSON.parse(key)[1] === id);
    this.cache.put(JSON.stringify(["missing", id]), true);
    this.dirtySessions.delete(id);
    this.summaryReads.delete(id);
    this.cache.invalidate(applicationKey("overview"));
    this.invalidateConfiguration(id);
    for (const [owner, selected] of this.scopes) if (selected === id) {
      this.scopes.delete(owner);
      this.scopePins.get(owner)?.();
      this.scopePins.delete(owner);
    }
    this.rebuildSnapshot();
    this.emit();
  }

  private applyEvent(envelope: ApplicationEventEnvelope): void {
    const { sessionId: id, event } = envelope;
    let view = this.session(id);
    if (event.type === "project_catalog") { this.cache.forget(projectDirectoryKey); this.cache.invalidate(applicationKey("overview")); return; }
    if (event.type === "session_catalog") {
      this.cache.invalidate(applicationKey("overview"));
      if (event.change === "deleted") {
        this.markMissing(id);
      } else {
        this.summaryReads.add(id);
        this.invalidateSession(id);
      }
      this.rebuildSnapshot();
      return;
    }
    if (event.type === "session_resource") {
      const version = { generation: String(event.generation), log: Number(event.logVersion), view: Number(event.viewVersion) };
      const head = view?.live.history ?? view?.metadata.version;
      if (!head || version.generation !== head.generation || version.log > head.log || version.view > head.view || event.uncertain) {
        this.invalidateSession(id);
        this.summaryReads.add(id);
        if (!head || version.generation !== head.generation || version.log > head.log || event.uncertain) this.cache.invalidate((key) => {
          const parts = JSON.parse(key);
          return parts[0] === "session" && parts[1] === id && parts[2] === "nodes" && !String(parts[3]).includes('"cursor"');
        });
        this.cache.invalidate(JSON.stringify(["configuration", "stats", id]));
        this.cache.invalidate(applicationKey("overview"));
      }
      return;
    }
    if (event.type === "session_availability") {
      this.invalidateSession(id);
      this.invalidateConfiguration(id);
      return;
    }
    if (event.type === "configuration_resource") {
      const cwd = typeof event.cwd === "string" ? event.cwd : "";
      this.cache.invalidate((key) => {
        const [kind, scope, id] = JSON.parse(key);
        if (kind !== "configuration") return false;
        if (scope === "models") return Boolean(event.models) && (!cwd || projectMatches(cwd, { cwd: id, projectRoot: id } as SessionInfo));
        const owner = this.session(id)?.metadata.info;
        return Boolean(event.resources) && (!cwd || !owner || projectMatches(cwd, owner));
      });
      this.configurationRevision++;
      for (const selected of this.selectedIDs()) {
        const owner = this.session(selected)?.metadata.info;
        if (!cwd || !owner || projectMatches(cwd, owner)) this.invalidateSession(selected, false);
      }
      return;
    }
    if (!view) {
      const key = JSON.stringify(["activity", id]);
      const previous = this.cache.peek<{ state: SessionRuntimeState; activityVersion?: ResourceRevision; bashVersion?: ResourceRevision }>(key) ?? { state: {} };
      const next = { ...previous, state: { ...previous.state } };
      const activity = event.activityVersion as ResourceRevision | undefined, bash = event.bashVersion as ResourceRevision | undefined;
      if (newerRevision(activity, previous.activityVersion) && event.activity && typeof event.activity === "object") {
        next.activityVersion = activity;
        Object.assign(next.state, event.activity);
      }
      if (newerRevision(bash, previous.bashVersion) && typeof event.bashRunning === "boolean") { next.bashVersion = bash; next.state.isBashRunning = event.bashRunning; }
      if (next.activityVersion || next.bashVersion) { this.cache.put(key, next); this.updateRunning(id, next.state); }
      return;
    }
    let live = view.live;
    const activityVersion = event.activityVersion as ResourceRevision | undefined;
    if (newerRevision(activityVersion, live.activityVersion) && event.activity && typeof event.activity === "object") {
      const activity = event.activity as SessionRuntimeState;
      live = { ...live, activityVersion, state: { ...live.state, ...activity, isPromptRunning: activity.phase !== "idle" } };
      this.updateRunning(id, live.state!);
    }
    const bashVersion = event.bashVersion as ResourceRevision | undefined;
    if (newerRevision(bashVersion, live.bashVersion) && typeof event.bashRunning === "boolean") {
      live = { ...live, bashVersion, state: { ...live.state, isBashRunning: event.bashRunning } };
      this.updateRunning(id, live.state!);
    }
    if (event.type === "live_resource") {
      const agent = event.agent as AgentReadVersion | undefined;
      const controls = event.session as SessionReadView["controls"];
      if (agent && (!live.version || agent.generation !== live.version.generation || agent.configuration > live.version.configuration || agent.queue > live.version.queue)
        || controls && (!view.controls || controls.generation !== view.controls.generation || controls.controls > view.controls.controls)) {
        this.invalidateSession(id, false);
      }
      if (agent && (!live.version || agent.generation !== live.version.generation || agent.configuration > live.version.configuration)) {
        this.invalidateConfiguration(id);
      }
      view = { ...view, controls: controls ?? view.controls };
    }
    const readVersion = event.readVersion as AgentReadVersion | undefined;
    const sameAgent = readVersion && live.version?.generation === readVersion.generation;
    if (readVersion && !sameAgent) this.invalidateSession(id, false);
    if (event.type === "queue_update" && sameAgent && readVersion.queue > (live.version?.queue ?? -1)) {
      live = { ...live, version: { ...live.version!, queue: readVersion.queue }, state: { ...live.state, queuedMessages: {
        steering: Array.isArray(event.steering) ? event.steering as string[] : [], followUp: Array.isArray(event.followUp) ? event.followUp as string[] : [],
      } } };
    }
    if (sameAgent && readVersion.state > (live.version?.state ?? -1)) {
      const message = event.message as AgentMessage | undefined;
      const messageId = typeof event.messageId === "string" ? event.messageId : "";
      switch (event.type) {
        case "message_start": case "message_update":
          if (message && messageId) live = { ...live, streaming: { id: messageId, message } };
          break;
        case "message_end":
          if (message && messageId) {
            const pending = view.pending.filter((item) => item.id !== messageId);
            pending.push({ id: messageId, message });
            view = { ...view, pending };
            live = { ...live, streaming: null };
          }
          break;
        case "agent_end": live = { ...live, streaming: null, toolsRunning: [] }; break;
        case "tool_execution_start": case "tool_execution_update": {
          if (typeof event.toolCallId !== "string") break;
          const tools = [...(live.toolsRunning ?? [])];
          const index = tools.findIndex((tool) => tool.toolCallId === event.toolCallId);
          const value = { toolCallId: event.toolCallId, toolName: String(event.toolName ?? ""), args: event.args,
            ...(event.partialResult && typeof event.partialResult === "object" ? { partialResult: event.partialResult as Record<string, unknown> } : {}) };
          if (index >= 0) tools[index] = { ...tools[index]!, ...value }; else tools.push(value);
          live = { ...live, toolsRunning: tools };
          break;
        }
        case "tool_execution_end": live = { ...live, toolsRunning: live.toolsRunning?.filter((tool) => tool.toolCallId !== event.toolCallId) }; break;
      }
      if (["agent_start", "message_start", "message_update", "message_end", "agent_end", "tool_execution_start", "tool_execution_update", "tool_execution_end"].includes(event.type)) {
        live = { ...live, version: { ...live.version!, state: readVersion.state } };
      }
    }
    if (event.type === "message_receipt" && event.accepted && typeof event.messageId === "string" && typeof event.entryId === "string") {
      const entryId = event.entryId, generation = String(event.generation);
      view = { ...view, pending: view.pending.map((item) => item.id === event.messageId ? { ...item, entryId, generation } : item) };
      if (generation === view.window.version.generation && view.window.pages.some((page) => page.items.some((item) => item.node.id === entryId))) {
        view = { ...view, pending: view.pending.filter((item) => item.id !== event.messageId) };
      }
      this.invalidateSession(id);
    }
    if (event.type === "thinking_level_changed" && typeof event.level === "string") this.invalidateSession(id, false);
    if (event.type === "agent_settled" || event.type === "compaction_end" || event.type === "auto_retry_end") this.invalidateSession(id, false);
    if (event.type === "operation") {
      if (event.status === "failed") this.error = typeof event.errorMessage === "string" ? event.errorMessage : "指令执行失败";
      this.invalidateSession(id);
    }
    this.cache.put(viewKey(id), { ...view, live });
  }

  private updateRunning(id: string, state: SessionRuntimeState): void {
    if (!this.overview) return;
    const running = Boolean(state.isPromptRunning || state.isStreaming || state.isCompacting || state.isBashRunning || state.phase && state.phase !== "idle");
    const ids = new Set(this.overview.runningSessionIds);
    if (running === ids.has(id)) return;
    if (running) ids.add(id); else ids.delete(id);
    this.overview = { ...this.overview, runningSessionIds: [...ids].sort() };
    this.rebuildSnapshot();
  }

  private schedule(): void {
    if (!this.running || this.refreshTimer || this.holds > 0 || this.needsBaseline) return;
    this.refreshTimer = setTimeout(() => {
      this.refreshTimer = null;
      const selected = this.selectedIDs().filter((id) => this.dirtySessions.has(id));
      const summaries = [...this.summaryReads].filter((id) => !selected.includes(id));
      this.summaryReads.clear();
      const work: Promise<unknown>[] = selected.map((id) => this.loadSession(id));
      if (!this.cache.isFresh(applicationKey("overview"))) work.push(this.refreshOverview());
      for (const [key, query] of this.directoryQueries) {
        if (this.visibleDirectories.has(key) && !this.cache.isFresh(applicationKey("directory", { ...query, limit: directoryLimit })))
          work.push(this.loadDirectory(query.project, query.search));
      }
      for (const id of summaries) work.push(this.read(id, "metadata").then(({ data }) => { this.patchSummary(id, data.info); this.rebuildSnapshot(); this.emit(); }));
      void Promise.allSettled(work).then((results) => {
        const failure = results.find((result) => result.status === "rejected");
        if (failure?.status === "rejected") this.fail(failure.reason);
      });
    }, 20);
  }

  private async refreshLoadedDirectories(): Promise<void> {
    await Promise.all([...this.directoryQueries].filter(([key]) => this.visibleDirectories.has(key)).map(([, query]) => this.loadDirectory(query.project, query.search, false, true)));
  }

  async validateVisible(): Promise<void> {
    if (!this.running) return;
    if (this.needsBaseline) return this.recover();
    await Promise.all([this.refreshOverview(true), this.refreshLoadedDirectories(), ...this.selectedIDs().map((id) => this.loadSession(id, true))]);
    this.invalidateConfiguration();
    this.emit();
  }

  private emit(): void { this.revision++; for (const listener of this.listeners) listener(); }
  private fail(error: unknown): void { this.error = errorText(error); this.emit(); }
}

const stores = new WeakMap<ApplicationClient, ApplicationDataStore>();
export function applicationDataFor(client: ApplicationClient): ApplicationDataStore {
  let store = stores.get(client);
  if (!store) { store = new ApplicationDataStore(client); stores.set(client, store); }
  return store;
}
