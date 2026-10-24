export type ClientKind = "local" | "remote";

export interface SelectedModel {
  provider: string;
  modelId: string;
}

export interface SessionInfo {
  path: string;
  id: string;
  cwd: string;
  name?: string;
  created: string;
  modified: string;
  messageCount: number;
  firstMessage: string;
  parentSessionId?: string;
  projectRoot: string;
}

export interface ProjectInfo {
  path: string;
  modified: string;
  sessionCount: number;
}

export interface ApplicationSnapshot {
  revision: number;
	cursor: string;
  agentDir: string;
  defaultCwd: string;
  projects: ProjectInfo[];
  sessions: SessionInfo[];
  runningSessionIds: string[];
}

export interface ModelListItem {
  id: string;
  name: string;
  provider: string;
  providerName?: string;
}

export interface ModelsView {
  models: Record<string, string>;
  modelList: ModelListItem[];
  defaultModel: SelectedModel | null;
  thinkingLevels: Record<string, string[]>;
  thinkingLevelMaps: Record<string, Record<string, string | null>>;
  thinkingLevelPins: Record<string, string>;
  modelError?: string;
  modelScopeWarnings?: string[];
}

export interface DirectoryEntry {
  name: string;
  path: string;
}

export interface DirectoryView {
  path: string;
  parentPath: string | null;
  directories: DirectoryEntry[];
  drives?: DirectoryEntry[];
}

export interface FileEntry {
  name: string;
  isDir: boolean;
  size: number;
  modified: string;
}

export interface FileList {
  path: string;
  entries: FileEntry[];
}

export type FilePreviewKind = "text" | "image" | "audio" | "pdf" | "docx";

export interface FilePreview {
  path: string;
  name: string;
  kind: FilePreviewKind;
  mimeType: string;
  language: string;
  size: number;
  content?: string;
  sourceUrl?: string;
}

export type UploadConflictStrategy = "error" | "overwrite" | "skip";

export interface UploadTargetInspection {
  conflicts: string[];
  nonReplaceable: string[];
}

export interface UploadFileError {
  name: string;
  error: string;
}

export interface UploadResult {
  uploaded: string[];
  skipped: string[];
  errors: UploadFileError[];
  conflicts?: string[];
  nonReplaceable?: string[];
}

export interface QueuedMessages {
  steering: string[];
  followUp: string[];
}

export interface ContextUsage {
  percent: number | null;
  contextWindow: number;
  tokens: number | null;
}

export interface TokenCostInfo {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  total: number;
}

export interface TokenUsageInfo {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  totalTokens?: number;
  cost?: TokenCostInfo;
}

export interface SessionRuntimeState {
  sessionId?: string;
  cwd?: string;
  thinkingLevel?: string;
  phase?: string;
  isStreaming?: boolean;
  isPromptRunning?: boolean;
  isBashRunning?: boolean;
  isCompacting?: boolean;
  retryAttempt?: number;
  retryWaiting?: boolean;
  queuedMessages?: QueuedMessages;
  contextUsage?: ContextUsage | null;
  model?: {
    id: string;
    name?: string;
    provider: string;
  };
  [key: string]: unknown;
}

export interface SessionContext {
  messages: AgentMessage[];
  entryIds: string[];
  thinkingLevel: string;
  model: SelectedModel | null;
}

export interface SessionTreeEntry {
  id: string;
  type: string;
  message?: AgentMessage;
  [key: string]: unknown;
}

export interface SessionTreeNode {
  entry: SessionTreeEntry;
  children: SessionTreeNode[];
  label?: string;
  compressedEntryIds?: string[];
}

export interface SessionView {
  revision: number;
  sessionId: string;
  filePath: string;
  info: SessionInfo;
  leafId: string | null;
  tree: SessionTreeNode[];
  context: SessionContext;
  running: boolean;
  state?: SessionRuntimeState;
}

export interface SessionVersion {
  generation: string;
  log: number;
  view: number;
}

export interface AgentReadVersion {
  generation: string;
  state: number;
  configuration: number;
  queue: number;
}

export interface ReadResourceRequest {
  leafId?: string;
  at?: SessionVersion;
  cursor?: string;
  aroundId?: string;
  afterId?: string;
  direction?: "older" | "newer";
  limit?: number;
  maxBytes?: number;
  parentId?: string;
  tree?: boolean;
  entryType?: string;
  role?: string;
  search?: string;
  entryId?: string;
  generation?: string;
  blockIndex?: number;
}

export interface ResourceResponse<T> {
  token: string;
	cursor?: string;
  unchanged?: boolean;
  data?: T;
}

export interface ApplicationReadRequest {
  project?: string;
  search?: string;
  cursor?: string;
  limit?: number;
  sessionLimit?: number;
}

export interface ApplicationOverview {
  agentDir: string;
  defaultCwd: string;
  projects: ProjectInfo[];
  sessionCount: number;
  runningSessionIds: string[];
  directory?: ProjectDirectoryPage;
}

export interface ProjectDirectoryPage {
  version: string;
  projects: { project: ProjectInfo; sessions: SessionDirectoryPage }[];
  start: number;
  total: number;
  next?: string;
}

export interface SessionDirectoryPage {
  version: string;
  sessions: SessionInfo[];
  total: number;
  start: number;
  next?: string;
}

export interface ApplicationResources {
  overview: ApplicationOverview;
  directory: SessionDirectoryPage;
  projects: ProjectDirectoryPage;
}

export interface EntryNode {
  id: string;
  parentId: string | null;
  type: string;
  role?: string;
  timestamp: string;
  preview: string;
  label?: string;
  hasChildren: boolean;
  rawBytes: number;
  contentToken?: string;
  branchDepth?: number;
  position?: number;
}

export interface HistoryItem {
  node: EntryNode;
  message?: AgentMessage;
}

export interface HistoryPage {
  sessionId: string;
  version: SessionVersion;
  anchorId: string;
  items: HistoryItem[];
  start: number;
  end: number;
  total: number;
  connected: boolean;
  older?: string;
  newer?: string;
}

export interface NodesPage {
  connected: boolean;
  sessionId: string;
  version: SessionVersion;
  items: EntryNode[];
  next?: string;
}

export interface SessionMetadata {
  sessionId: string;
  version: SessionVersion;
  info: SessionInfo;
  filePath: string;
  leafId: string | null;
}

export interface ReadMessage {
  id: string;
  message: AgentMessage;
}

export interface LiveSnapshot {
  sessionId: string;
  active: boolean;
  version?: AgentReadVersion;
  activityVersion?: ResourceRevision;
  bashVersion?: ResourceRevision;
  history?: SessionVersion & { leafId: string | null };
  state?: SessionRuntimeState;
  streaming?: ReadMessage | null;
  pending?: ReadMessage[];
  toolsRunning?: { toolCallId: string; toolName: string; args: unknown; partialResult?: Record<string, unknown> }[];
  runId?: number;
  turn?: number;
  errorMessage?: string;
}

export interface ResourceRevision {
  generation: string;
  revision: number;
}

export interface SessionResources {
  history: HistoryPage;
  nodes: NodesPage;
  metadata: SessionMetadata;
  live: LiveSnapshot;
  entry: { sessionId: string; generation: string; entryId: string; contentToken: string; message: AgentMessage | null };
  image: { sessionId: string; generation: string; entryId: string; mimeType: string; data: string };
}

export interface CreateSessionRequest {
  cwd: string;
  provider?: string;
  modelId?: string;
  toolNames?: string[];
  thinkingLevel?: string;
}

export interface CreateSessionResult {
  sessionId: string;
  revision: number;
  model: SelectedModel | null;
  thinkingLevel: string;
}

export interface ImageAttachment {
  data: string;
  mimeType: string;
  previewUrl: string;
}

export interface ApplicationEvent {
  type: string;
  [key: string]: unknown;
}

export interface ApplicationEventEnvelope {
  sequence: number;
  cursor?: string;
  sessionId: string;
  event: ApplicationEvent;
}

export interface AuthState {
  authRequired: boolean;
  authenticated: boolean;
  expiresAtMs?: number | null;
}

export type SlashCommandSource = "extension" | "prompt" | "skill";

export interface SlashCommandInfo {
  name: string;
  description?: string;
  argumentHint?: string;
  source: SlashCommandSource;
  sourceInfo?: {
    path: string;
    source: string;
    scope: "user" | "project" | "temporary";
    origin: "package" | "top-level";
    baseDir?: string;
  };
}

export interface SessionStatsInfo {
  sessionFile?: string;
  sessionId: string;
  sessionName?: string;
  userMessages: number;
  assistantMessages: number;
  toolCalls: number;
  toolResults: number;
  totalMessages: number;
  tokens: {
    input: number;
    output: number;
    cacheRead: number;
    cacheWrite: number;
    total: number;
  };
  cost: number;
  contextUsage?: ContextUsage;
}

export interface EventObserver {
  onEvent(envelope: ApplicationEventEnvelope): void;
  onReset(revision: number, reason?: string, cursor?: string): void;
  onConnected?(cursor: string): void;
}

export interface EventSubscription {
  ready: Promise<void>;
  close(): void;
}

export interface ApplicationClient {
  readonly kind: ClientKind;
  readonly endpoint: string;
  authStatus(): Promise<AuthState>;
  login(password: string): Promise<void>;
  snapshot(): Promise<ApplicationSnapshot>;
  readApplicationResource<K extends keyof ApplicationResources>(kind: K, query?: ApplicationReadRequest, known?: string): Promise<ResourceResponse<ApplicationResources[K]>>;
  sessionView(sessionId: string, leafId?: string): Promise<SessionView>;
  readResource<K extends keyof SessionResources>(sessionId: string, kind: K, query?: ReadResourceRequest, known?: string): Promise<ResourceResponse<SessionResources[K]>>;
  createSession(input: CreateSessionRequest): Promise<CreateSessionResult>;
  models(cwd: string): Promise<ModelsView>;
  browseDirectories(path?: string): Promise<DirectoryView>;
  listFiles(path: string): Promise<FileList>;
  previewFile(path: string): Promise<FilePreview>;
  deleteFile(path: string): Promise<void>;
  inspectUploadTargets(directory: string, fileNames: string[]): Promise<UploadTargetInspection>;
  uploadFiles(directory: string, files: File[], strategy: UploadConflictStrategy): Promise<UploadResult>;
  addProject(path: string): Promise<ProjectInfo>;
  removeProject(path: string): Promise<void>;
  renameSession(sessionId: string, name: string): Promise<void>;
  deleteSession(sessionId: string): Promise<void>;
  dispatch<T = unknown>(sessionId: string, command: Record<string, unknown>): Promise<T>;
  subscribe(after: number | string, observer: EventObserver, sessionIds?: string[]): EventSubscription;
  close(): void;
}

export type MessageContentBlock = {
  imageRef?: { entryId: string; blockIndex: number; generation?: string };
  byteSize?: number;
  type?: string;
  text?: string;
  thinking?: string;
  name?: string;
  toolName?: string;
  [key: string]: unknown;
};

export interface AgentMessage {
  role?: string;
  content?: string | MessageContentBlock[];
  customType?: string;
  display?: boolean;
  timestamp?: number;
  api?: string;
  provider?: string;
  model?: string;
  usage?: TokenUsageInfo;
  stopReason?: string;
  [key: string]: unknown;
}
