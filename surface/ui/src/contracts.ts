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

export interface ApplicationSnapshot {
  revision: number;
  agentDir: string;
  defaultCwd: string;
  sessions: SessionInfo[];
  runningSessionIds: string[];
}

export interface ModelListItem {
  id: string;
  name: string;
  provider: string;
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

export interface QueuedMessages {
  steering: string[];
  followUp: string[];
}

export interface ContextUsage {
  percent: number | null;
  contextWindow: number;
  tokens: number | null;
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
  onReset(revision: number, reason?: string): void;
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
  sessionView(sessionId: string, leafId?: string): Promise<SessionView>;
  createSession(input: CreateSessionRequest): Promise<CreateSessionResult>;
  models(cwd: string): Promise<ModelsView>;
  browseDirectories(path?: string): Promise<DirectoryView>;
  listFiles(path: string): Promise<FileList>;
  renameSession(sessionId: string, name: string): Promise<void>;
  deleteSession(sessionId: string): Promise<void>;
  dispatch<T = unknown>(sessionId: string, command: Record<string, unknown>): Promise<T>;
  subscribe(after: number, observer: EventObserver): EventSubscription;
  close(): void;
}

export type MessageContentBlock = {
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
  [key: string]: unknown;
}
