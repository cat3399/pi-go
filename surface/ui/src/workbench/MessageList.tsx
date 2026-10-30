import {
  createContext,
  useContext,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type PointerEvent as ReactPointerEvent,
  type ReactNode,
} from "react";
import { ArrowDown, ArrowUpRight, BookOpen, Check, ChevronRight, Copy, FilePenLine, GitFork, LoaderCircle, Pencil, Search, SquareTerminal } from "lucide-react";
import type { AgentMessage, HistoryRun, LiveSnapshot, MessageContentBlock } from "../contracts";
import { MarkdownBody } from "../content/MarkdownBody";
import { ImagePreview } from "../primitives/ImagePreview";
import { OverlayScrollbar } from "../primitives/OverlayScrollbar";
import { messageText, visibleMessage } from "./message";
import { ElapsedClock, elapsedBetween } from "./elapsed-time";
import { ComposerCancelButton, ComposerInput, ComposerSendButton } from "./ComposerInput";
import { MessageAnchors, type MessageAnchorsHandle } from "./MessageAnchors";
import { TranscriptViewport } from "./TranscriptViewport";
import { blocksEdgeGestureStart, isTextSelectionInteraction } from "./edge-gesture-target";

import type { SessionImageCache } from "./session-images";
import type { ApplicationDataStore, HistoryWindow, ScrollPosition } from "./application-data";

const ImageCacheContext = createContext<SessionImageCache | null>(null);
const HistoryContext = createContext<{ data: ApplicationDataStore; sessionId: string; generation: string } | null>(null);
const ToolCallsContext = createContext<Set<string>>(new Set());
const ToolResultEntriesContext = createContext<Map<string, string>>(new Map());

function useDisclosure(kind: string, identity: string, initial: boolean): [boolean, (value: boolean | ((previous: boolean) => boolean)) => void] {
  const history = useContext(HistoryContext);
  const key = history && identity ? JSON.stringify([history.sessionId, history.generation, kind, identity]) : "";
  const [value, setValue] = useState(initial);
  const current = key ? history?.data.disclosures.get(key) ?? value : value;
  return [current, (next) => {
    const result = typeof next === "function" ? next(current) : next;
    if (key) history?.data.disclosures.set(key, result);
    setValue(result);
  }];
}

interface MessageListProps {
  imageCache: SessionImageCache;
  data: ApplicationDataStore;
  historyWindow: HistoryWindow | null;
  toolProgress: LiveSnapshot["toolsRunning"];
  onLoadHistory(direction: "older" | "newer" | "latest", aroundId?: string): Promise<void>;
  sessionId: string;
  messages: AgentMessage[];
  pendingMessages: AgentMessage[];
  entryIds: string[];
  streamingMessage: AgentMessage | null;
  busy: boolean;
  compacting: boolean;
  mobile: boolean;
  anchorsEnabled: boolean;
  onOpenTerminal?(id: string): void;
  onFork(entryId: string): Promise<void>;
  onEdit(entryId: string, text: string): Promise<void>;
}

interface AnchorTrackMetrics {
  innerTop: number;
  innerHeight: number;
  slotHeight: number;
}

interface AnchorGesture {
  pointerId: number;
  startX: number;
  startY: number;
  lastY: number;
  engaged: boolean;
  hasSelection: boolean;
  insideHitRegion: boolean;
  index: number;
  trackMetrics: AnchorTrackMetrics | null;
  stageTop: number;
  stageBottom: number;
}

const MOBILE_EDGE_SIZE = 44;
const TOUCH_SLOP = 8;
const ANCHOR_PREVIEW_DWELL_MS = 160;

async function writeClipboardText(value: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(value);
      return;
    } catch {
      // Older Android WebViews may expose the API but reject the write.
    }
  }
  const input = document.createElement("textarea");
  input.value = value;
  input.setAttribute("readonly", "");
  input.style.position = "fixed";
  input.style.opacity = "0";
  document.body.append(input);
  input.select();
  const copied = document.execCommand("copy");
  input.remove();
  if (!copied) throw new Error("当前环境不支持剪贴板写入");
}

function contentBlocks(message: AgentMessage): MessageContentBlock[] {
  return Array.isArray(message.content) ? message.content : [];
}

function MessageImage({ block, alt }: { block: MessageContentBlock; alt: string }) {
  const cache = useContext(ImageCacheContext)!;
  const [src, setSource] = useState(() => cache.source(block));
  const [status, setStatus] = useState<"idle" | "loading" | "loaded" | "error">(
    block.imageRef && !cache.availableSource(block) ? "idle" : "loaded",
  );
  const [preview, setPreview] = useState(false);
  const identity = JSON.stringify(block.imageRef ?? block.url ?? block.source ?? block.data);
  useEffect(() => {
    setSource(cache.source(block));
    setStatus(block.imageRef && !cache.availableSource(block) ? "idle" : "loaded");
    setPreview(false);
  }, [cache, identity]);
  if (!src && !block.imageRef) return null;
  const size = typeof block.byteSize === "number"
    ? block.byteSize >= 1024 * 1024
      ? `${(block.byteSize / (1024 * 1024)).toFixed(1)} MB`
      : `${Math.max(1, Math.round(block.byteSize / 1024))} KB`
    : "";
  return (
    <>
      <button
        className="pi-message-image-button"
        type="button"
        aria-label={status === "loaded" ? `预览${alt}` : `加载${alt}`}
        aria-busy={status === "loading"}
        onClick={() => {
          if (status === "loaded") { setPreview(true); return; }
          setStatus("loading");
          void cache.load(block).then(setSource).catch(() => setStatus("error"));
        }}
      >
        {src && (status === "loading" || status === "loaded") && (
          <img className="pi-message-image" src={src} alt={alt}
            onLoad={() => { cache.rememberLoaded(block, src); setStatus("loaded"); }} onError={() => setStatus("error")} />
        )}
        {status !== "loaded" && (
          <span className="pi-message-image-placeholder" role={status === "error" ? "alert" : "status"}>
            {status === "loading" ? "图片加载中…" : status === "error" ? "图片加载失败，点击重试" : "点击查看图片"}
            {size && ` · ${size}`}
          </span>
        )}
      </button>
      {preview && <ImagePreview src={src} alt={alt} onClose={() => setPreview(false)} />}
    </>
  );
}

function imageBlockKey(block: MessageContentBlock, index: number): string {
  return block.imageRef ? `${block.imageRef.generation}:${block.imageRef.entryId}:${block.imageRef.blockIndex}` : `inline-${index}`;
}

function toolCallID(block: MessageContentBlock): string {
  if (typeof block.toolCallId === "string") return block.toolCallId;
  return typeof block.id === "string" ? block.id : "";
}

function toolCallName(block: MessageContentBlock): string {
  if (typeof block.toolName === "string") return block.toolName;
  return typeof block.name === "string" ? block.name : "tool";
}

function toolCallInput(block: MessageContentBlock): unknown {
  if (block.input && typeof block.input === "object") return block.input;
  if (block.arguments && typeof block.arguments === "object") return block.arguments;
  return {};
}

function resultText(message: AgentMessage | undefined): string {
  if (!message) return "";
  return messageText(message);
}

function toolInputRecord(block: MessageContentBlock): Record<string, unknown> {
  const value = toolCallInput(block);
  return value && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : {};
}

function inputString(input: Record<string, unknown>, key: string): string {
  return typeof input[key] === "string" ? input[key] as string : "";
}

type FileChangeKind = "context" | "added" | "removed" | "separator";

interface FileChangeLine {
  kind: FileChangeKind;
  oldLine?: number;
  newLine?: number;
  text: string;
}

function recordValue(value: unknown): Record<string, unknown> | null {
  return value && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : null;
}

function fileName(path: string): string {
  const normalized = path.replace(/\\/g, "/").replace(/\/$/, "");
  return normalized.split("/").pop() || path || "文件";
}

function contentLines(value: string): string[] {
  if (!value) return [];
  const lines = value.replace(/\r\n/g, "\n").split("\n");
  if (lines.at(-1) === "") lines.pop();
  return lines;
}

function parseUnifiedPatch(patch: string): FileChangeLine[] {
  const rows: FileChangeLine[] = [];
  let oldLine = 0;
  let newLine = 0;
  let inHunk = false;
  let hunkCount = 0;

  for (const line of patch.replace(/\r\n/g, "\n").split("\n")) {
    const hunk = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(line);
    if (hunk) {
      oldLine = Number(hunk[1]);
      newLine = Number(hunk[2]);
      if (hunkCount > 0) rows.push({ kind: "separator", text: "…" });
      hunkCount += 1;
      inHunk = true;
      continue;
    }
    if (!inHunk || line === "\\ No newline at end of file" || line.length === 0) continue;
    const marker = line[0];
    const text = line.slice(1);
    if (marker === "+") {
      rows.push({ kind: "added", newLine, text });
      newLine += 1;
    } else if (marker === "-") {
      rows.push({ kind: "removed", oldLine, text });
      oldLine += 1;
    } else if (marker === " ") {
      rows.push({ kind: "context", oldLine, newLine, text });
      oldLine += 1;
      newLine += 1;
    }
  }
  return rows;
}

function parseDisplayDiff(diff: string): FileChangeLine[] {
  const rows: FileChangeLine[] = [];
  for (const line of diff.replace(/\r\n/g, "\n").split("\n")) {
    if (/^\s*\.\.\.\s*$/.test(line)) {
      rows.push({ kind: "separator", text: "…" });
      continue;
    }
    const match = /^([ +\-])(\d+)\s?(.*)$/.exec(line);
    if (!match) continue;
    const lineNumber = Number(match[2]);
    const text = match[3] ?? "";
    if (match[1] === "+") rows.push({ kind: "added", newLine: lineNumber, text });
    else if (match[1] === "-") rows.push({ kind: "removed", oldLine: lineNumber, text });
    else rows.push({ kind: "context", oldLine: lineNumber, newLine: lineNumber, text });
  }
  return rows;
}

function editFallbackLines(input: Record<string, unknown>): FileChangeLine[] {
  const edits = Array.isArray(input.edits) ? input.edits : [];
  const rows: FileChangeLine[] = [];
  edits.forEach((value, index) => {
    const edit = recordValue(value);
    if (!edit) return;
    if (rows.length > 0) rows.push({ kind: "separator", text: "…" });
    contentLines(inputString(edit, "oldText")).forEach((text) => rows.push({ kind: "removed", text }));
    contentLines(inputString(edit, "newText")).forEach((text) => rows.push({ kind: "added", text }));
  });
  return rows;
}

function fileChangeLines(name: string, input: Record<string, unknown>, result?: AgentMessage): FileChangeLine[] {
  if (name === "write") {
    return contentLines(inputString(input, "content")).map((text, index) => ({
      kind: "added",
      newLine: index + 1,
      text,
    }));
  }
  const details = recordValue(result?.details);
  const patch = details && typeof details.patch === "string" ? details.patch : "";
  const parsed = patch ? parseUnifiedPatch(patch) : [];
  if (parsed.length > 0) return parsed;
  const diff = details && typeof details.diff === "string" ? details.diff : "";
  const displayRows = diff ? parseDisplayDiff(diff) : [];
  return displayRows.length > 0 ? displayRows : editFallbackLines(input);
}

function fileChangeCopyValue(name: string, input: Record<string, unknown>, result?: AgentMessage): string {
  if (name === "write") return inputString(input, "content");
  const details = recordValue(result?.details);
  if (details && typeof details.patch === "string" && details.patch) return details.patch;
  if (details && typeof details.diff === "string" && details.diff) return details.diff;
  return (Array.isArray(input.edits) ? input.edits : []).flatMap((value) => {
    const edit = recordValue(value);
    if (!edit) return [];
    return [inputString(edit, "oldText"), inputString(edit, "newText")].filter(Boolean);
  }).join("\n");
}

function FileChangeCard({
  name,
  input,
  result,
  streaming,
}: {
  name: string;
  input: Record<string, unknown>;
  result?: AgentMessage;
  streaming: boolean;
}) {
  const [copied, setCopied] = useState(false);
  const viewportRef = useRef<HTMLDivElement>(null);
  const path = inputString(input, "path");
  const rows = fileChangeLines(name, input, result);
  const additions = rows.filter((line) => line.kind === "added").length;
  const removals = rows.filter((line) => line.kind === "removed").length;
  const copyValue = fileChangeCopyValue(name, input, result);
  const failed = result?.isError === true;
  const complete = Boolean(result && !result.partial);
  const running = streaming || result?.partial === true;

  const copy = async () => {
    await writeClipboardText(copyValue);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1500);
  };

  return (
    <section className={`pi-file-change ${failed ? "is-error" : ""}`}>
      <header className="pi-file-change-header">
        <span className="pi-file-change-name" title={path}>{fileName(path)}</span>
        <span className="pi-file-change-stats" aria-label={`${additions} 行新增，${removals} 行删除`}>
          <span className="is-added">+{additions}</span>
          <span className="is-removed">-{removals}</span>
        </span>
        <span className="pi-file-change-status">
          {complete && !failed && <Check size={12} />}
          {running && !complete ? (name === "write" ? "写入中" : "修改中") : failed ? "失败" : complete ? "成功" : "等待"}
        </span>
        <button
          type="button"
          className="pi-file-change-copy"
          aria-label={copied ? "变更已复制" : "复制变更"}
          title={copied ? "已复制" : "复制变更"}
          disabled={!copyValue}
          onClick={() => void copy()}
        >
          {copied ? <Check size={14} /> : <Copy size={14} />}
        </button>
      </header>
      <div className="pi-file-change-scroll pi-overlay-scroll-host">
        <div ref={viewportRef} className="pi-file-change-lines pi-overlay-scroll-viewport">
          {rows.length > 0 ? rows.map((line, index) => line.kind === "separator" ? (
            <div className="pi-file-change-separator" key={`separator-${index}`}>{line.text}</div>
          ) : (
            <div className={`pi-file-change-line is-${line.kind}`} key={`${line.kind}-${index}`}>
              <span className="pi-file-change-number">{line.kind === "removed" ? line.oldLine : line.newLine}</span>
              <span className="pi-file-change-marker">{line.kind === "added" ? "+" : line.kind === "removed" ? "−" : ""}</span>
              <code>{line.text || " "}</code>
            </div>
          )) : (
            <div className="pi-file-change-empty">{failed ? "文件变更失败" : "没有文本行变更"}</div>
          )}
        </div>
        <OverlayScrollbar viewportRef={viewportRef} />
      </div>
    </section>
  );
}

function toolPresentation(name: string, input: Record<string, unknown>, complete: boolean) {
  switch (name) {
    case "read":
      return { icon: BookOpen, verb: complete ? "已读取" : "正在读取", target: inputString(input, "path"), card: "文件" };
    case "write":
      return { icon: FilePenLine, verb: complete ? "已写入" : "正在写入", target: inputString(input, "path"), card: "写入" };
    case "edit":
      return { icon: FilePenLine, verb: complete ? "已编辑" : "正在编辑", target: inputString(input, "path"), card: "编辑" };
    case "grep":
      return {
        icon: Search,
        verb: complete ? "已搜索" : "正在搜索",
        target: [inputString(input, "pattern"), inputString(input, "path")].filter(Boolean).join(" · "),
        card: "搜索",
      };
    case "find":
      return { icon: Search, verb: complete ? "已查找" : "正在查找", target: inputString(input, "pattern"), card: "查找" };
    case "ls":
      return { icon: BookOpen, verb: complete ? "已读取目录" : "正在读取目录", target: inputString(input, "path") || ".", card: "目录" };
    case "terminal":
      return { icon: SquareTerminal, verb: "终端", target: inputString(input, "command") || inputString(input, "id"), card: "终端" };
    case "bash":
      return { icon: SquareTerminal, verb: complete ? "已运行" : "正在运行", target: inputString(input, "command"), card: "Shell" };
    default:
      return { icon: SquareTerminal, verb: complete ? "已调用" : "正在调用", target: name, card: name };
  }
}

function ToolDataSection(props: {
  entryId?: string;
  kind: "input" | "output";
  value: string;
  children: ReactNode;
}) {
  const [copied, setCopied] = useState(false);
  const viewportRef = useRef<HTMLDivElement>(null);
  const label = props.kind === "input" ? "输入" : "输出";

  const copy = async () => {
    await writeClipboardText(props.value);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1500);
  };

  return (
    <div className={`pi-tool-section pi-overlay-scroll-host is-${props.kind}`} data-entry-id={props.entryId}>
      <div ref={viewportRef} className="pi-tool-section-content pi-overlay-scroll-viewport">{props.children}</div>
      <button
        className="pi-tool-section-copy"
        type="button"
        aria-label={copied ? `${label}已复制` : `复制${label}`}
        onClick={() => void copy()}
      >
        {copied ? <Check size={13} /> : <Copy size={13} />}
      </button>
      <OverlayScrollbar viewportRef={viewportRef} />
    </div>
  );
}

function ToolCall({
  block,
  result,
  streaming,
  onOpenTerminal,
}: {
  onOpenTerminal?(id: string): void;
  block: MessageContentBlock;
  result?: AgentMessage;
  streaming: boolean;
}) {
  const name = toolCallName(block);
  const resultEntryId = useContext(ToolResultEntriesContext).get(toolCallID(block));
  const [expanded, setExpanded] = useDisclosure("tool", toolCallID(block), name === "edit" || name === "write");
  const input = toolInputRecord(block);
  const output = resultText(result);
  const failed = result?.isError === true;
  const complete = Boolean(result && !result.partial);
  const running = streaming || result?.partial === true;
  const presentation = toolPresentation(name, input, complete);
  const Icon = presentation.icon;
  const serializedInput = JSON.stringify(input, null, 2);
  const command = inputString(input, "command");
  const details = recordValue(result?.details);
  const terminalId = name === "terminal"
    ? typeof details?.terminalId === "string" ? details.terminalId : inputString(input, "id")
    : "";
  const canOpenTerminal = Boolean(terminalId && onOpenTerminal);
  const inputCopyValue = name === "bash" ? command : serializedInput;
  const hasFileChange = name === "edit" || name === "write";

  return (
    <div className={`pi-tool ${failed ? "is-error" : ""} ${expanded ? "is-open" : ""}`} data-entry-id={!output || hasFileChange && !failed ? resultEntryId : undefined}>
      <div className="pi-tool-heading">
        <button className="pi-tool-summary" type="button" aria-expanded={canOpenTerminal ? undefined : expanded} title={canOpenTerminal ? "打开终端" : undefined} onClick={() => canOpenTerminal ? onOpenTerminal?.(terminalId) : setExpanded((value) => !value)}>
          <Icon size={15} />
          <span>{failed ? "调用失败" : presentation.verb}</span>
          {presentation.target && <code>{presentation.target}</code>}
          {canOpenTerminal ? <ArrowUpRight size={14} /> : <ChevronRight className="pi-disclosure" size={14} />}
        </button>
        {canOpenTerminal && <button className="pi-icon-button pi-terminal-details" type="button" aria-label="终端调用详情" aria-expanded={expanded} onClick={() => setExpanded((value) => !value)}><ChevronRight className="pi-disclosure" size={14} /></button>}
      </div>
      {expanded && (
        <div className={`pi-tool-body ${hasFileChange ? "is-file-change" : ""}`}>
          {hasFileChange ? (
            <FileChangeCard name={name} input={input} result={result} streaming={streaming} />
          ) : (
            <>
              <div className="pi-tool-card-header">
                <span>{presentation.card}</span>
                <span className="pi-tool-card-status">
                  {complete && !failed && <Check size={12} />}
                  {running && !complete ? "运行中" : failed ? "失败" : complete ? "成功" : "等待"}
                </span>
              </div>
              <ToolDataSection kind="input" value={inputCopyValue}>
                <pre className="pi-tool-input">{name === "bash" ? `$ ${command}` : serializedInput}</pre>
              </ToolDataSection>
            </>
          )}
          {output && (!hasFileChange || failed) && (
            <ToolDataSection kind="output" value={output} entryId={resultEntryId}>
              <pre className="pi-tool-output">{output}</pre>
            </ToolDataSection>
          )}
          {result && contentBlocks(result).map((image, index) => image.type === "image" ? (
            <MessageImage key={imageBlockKey(image, index)} block={image} alt={`工具结果图片 ${index + 1}`} />
          ) : null)}
        </div>
      )}
    </div>
  );
}

function processKind(block: MessageContentBlock): "bash" | "edit" | "read" | "search" | null {
  const name = toolCallName(block);
  if (name === "bash") return "bash";
  if (name === "edit" || name === "write") return "edit";
  if (name === "read" || name === "ls") return "read";
  if (name === "grep" || name === "find") return "search";
  return null;
}

function ProcessGroup(props: {
  kind: NonNullable<ReturnType<typeof processKind>>;
  blocks: MessageContentBlock[];
  toolResults: Map<string, AgentMessage>;
  streaming: boolean;
}) {
  const history = useContext(HistoryContext);
  const resultEntries = useContext(ToolResultEntriesContext);
  const focus = history?.data.focusedEntry(history.sessionId);
  const retainedEntry = history?.data.scroll.get(JSON.stringify([history.sessionId, history.generation]))?.entryId;
  const [expanded, setExpanded] = useDisclosure("process", props.blocks.map(toolCallID).join(":"), true);
  const containsFocus = props.blocks.some((block) => {
    const entry = resultEntries.get(toolCallID(block));
    return Boolean(entry && (entry === focus?.entryId || entry === retainedEntry));
  });
  useLayoutEffect(() => {
    if (containsFocus) setExpanded(true);
  }, [focus?.serial, containsFocus]);
  const complete = props.blocks.every((block) => { const result = props.toolResults.get(toolCallID(block)); return Boolean(result && !result.partial); });
  const failed = props.blocks.some((block) => props.toolResults.get(toolCallID(block))?.isError === true);
  const labels = {
    bash: "运行了命令",
    edit: complete ? "已编辑文件" : "正在编辑文件",
    read: complete ? "已读取文件" : "正在读取文件",
    search: complete ? "已搜索文件" : "正在搜索文件",
  };
  const icons = { bash: SquareTerminal, edit: FilePenLine, read: BookOpen, search: Search };
  const Icon = icons[props.kind];

  return (
    <div className={`pi-process-group ${failed ? "is-error" : ""} ${expanded ? "is-open" : ""}`}>
      <button className="pi-process-summary" type="button" aria-expanded={expanded} onClick={() => setExpanded((value) => !value)}>
        <Icon size={15} />
        <span>{labels[props.kind]}</span>
        <ChevronRight className="pi-disclosure" size={14} />
      </button>
      {expanded && (
        <div className="pi-process-steps">
          {props.blocks.map((block, index) => {
            const id = toolCallID(block);
            return (
              <ToolCall
                key={id || index}
                block={block}
                result={props.toolResults.get(id)}
                streaming={props.streaming}
              />
            );
          })}
        </div>
      )}
    </div>
  );
}

function ThinkingBlock({ block, streaming, identity }: { block: MessageContentBlock; streaming: boolean; identity: string }) {
  const [expanded, setExpanded] = useDisclosure("thinking", identity, false);
  const value = typeof block.thinking === "string" ? block.thinking.trim() : "";
  if (!value) return null;
  const [summary, ...remainder] = value.split("\n");
  const body = remainder.join("\n").trim();
  if (streaming || !body) return <div className="pi-thinking-line">{summary}</div>;
  return (
    <div className={`pi-thinking ${expanded ? "is-open" : ""}`}>
      <button
        className="pi-thinking-summary"
        type="button"
        aria-expanded={expanded}
        onClick={() => setExpanded((current) => !current)}
      >
        <span>{summary}</span>
        <ChevronRight className="pi-disclosure" size={14} />
      </button>
      {expanded && <MarkdownBody>{body}</MarkdownBody>}
    </div>
  );
}

function Message({
  message,
  streaming = false,
  toolResults,
  entryId,
  onFork,
  onOpenTerminal,
  editing = false,
  editAvailable = false,
  editEnabled = false,
  onEditStart,
  onEditCancel,
  onEdit,
  process = false,
}: {
  message: AgentMessage;
  streaming?: boolean;
  toolResults: Map<string, AgentMessage>;
  entryId?: string;
  onOpenTerminal?(id: string): void;
  onFork(entryId: string): Promise<void>;
  editing?: boolean;
  editAvailable?: boolean;
  editEnabled?: boolean;
  onEditStart?(entryId: string): void;
  onEditCancel?(): void;
  onEdit?(entryId: string, text: string): Promise<void>;
  process?: boolean;
}) {
  const history = useContext(HistoryContext);
  const linkedToolCalls = useContext(ToolCallsContext);
  const [copied, setCopied] = useState(false);
  const [userActionsVisible, setUserActionsVisible] = useState(false);
  const [draft, setDraft] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const actionsHideTimerRef = useRef<number | null>(null);
  const copiedTimerRef = useRef<number | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const blocks = contentBlocks(message);
  const text = messageText(message);
  const draftKey = history && entryId ? history.data.draftKey(history.sessionId, history.generation, entryId) : "";

  useEffect(() => () => {
    if (actionsHideTimerRef.current !== null) window.clearTimeout(actionsHideTimerRef.current);
    if (copiedTimerRef.current !== null) window.clearTimeout(copiedTimerRef.current);
  }, []);

  useEffect(() => {
    if (history && message.role === "toolResult" && typeof message.toolCallId === "string" && !linkedToolCalls.has(message.toolCallId)) {
      // An orphan result is visible at a page boundary. When its call arrives
      // on the preceding page, retain that output inside the associated card.
      history.data.disclosures.set(JSON.stringify([history.sessionId, history.generation, "tool", message.toolCallId]), true);
    }
  }, [history, message.role, message.toolCallId, linkedToolCalls]);

  useEffect(() => {
    if (!editing) return;
    const value = draftKey ? history?.data.drafts.get(draftKey)?.text ?? text : text;
    setDraft(value);
    setSubmitting(false);
    requestAnimationFrame(() => {
      const textarea = textareaRef.current;
      if (!textarea) return;
      textarea.focus({ preventScroll: true });
      textarea.setSelectionRange(value.length, value.length);
      textarea.style.height = "auto";
      textarea.style.height = `${Math.min(Math.max(textarea.scrollHeight, 76), 180)}px`;
    });
  }, [editing, text, draftKey, history?.data]);

  useEffect(() => {
    if (!editing) return;
    const textarea = textareaRef.current;
    if (!textarea) return;
    textarea.style.height = "auto";
    textarea.style.height = `${Math.min(Math.max(textarea.scrollHeight, 76), 180)}px`;
  }, [draft, editing]);

  if (message.role === "toolResult") {
    if (linkedToolCalls.has(String(message.toolCallId ?? ""))) return null;
    const output = resultText(message);
    return <article className="pi-message" data-entry-id={entryId}>
      {output && <ToolDataSection kind="output" value={output}><pre className="pi-tool-output">{output}</pre></ToolDataSection>}
      {blocks.map((image, index) => image.type === "image" ? <MessageImage key={imageBlockKey(image, index)} block={image} alt={`工具结果图片 ${index + 1}`} /> : null)}
    </article>;
  }
  if (!visibleMessage(message)) return null;
  if (message.role === "custom" && message.customType === "compaction") {
    return <CompactionResult message={message} entryId={entryId} />;
  }
  const role = message.role === "user"
    ? "user"
    : message.role === "custom"
      ? "custom"
      : message.role === "bashExecution"
        ? "bash"
        : "assistant";
  const renderedBlocks: ReactNode[] = [];
  for (let index = 0; index < blocks.length;) {
    const block = blocks[index];
    if (!block) {
      index += 1;
      continue;
    }
    const kind = block.type === "toolCall" ? processKind(block) : null;
    if (kind) {
      const grouped: MessageContentBlock[] = [];
      while (index < blocks.length) {
        const candidate = blocks[index];
        if (!candidate || candidate.type !== "toolCall" || processKind(candidate) !== kind) break;
        grouped.push(candidate);
        index += 1;
      }
      renderedBlocks.push(
        <ProcessGroup
          key={`process-${index}-${kind}`}
          kind={kind}
          blocks={grouped}
          toolResults={toolResults}
          streaming={streaming}
        />,
      );
      continue;
    }
    if (block.type === "text" && typeof block.text === "string" && block.text && !(message.role === "user" && editing)) {
      renderedBlocks.push(<MarkdownBody key={index} isStreaming={streaming}>{block.text}</MarkdownBody>);
    } else if (block.type === "thinking" && block.thinking) {
      renderedBlocks.push(<ThinkingBlock key={index} block={block} streaming={streaming} identity={entryId ? `${entryId}:${index}` : ""} />);
    } else if (block.type === "toolCall") {
      const id = toolCallID(block);
      renderedBlocks.push(
        <ToolCall key={id || index} block={block} result={toolResults.get(id)} streaming={streaming} onOpenTerminal={onOpenTerminal} />,
      );
    } else if (block.type === "image") {
      renderedBlocks.push(<MessageImage key={imageBlockKey(block, index)} block={block} alt={`消息图片 ${index + 1}`} />);
    }
    index += 1;
  }

  const copy = async () => {
    if (!text) return;
    await writeClipboardText(text);
    setCopied(true);
    if (copiedTimerRef.current !== null) window.clearTimeout(copiedTimerRef.current);
    copiedTimerRef.current = window.setTimeout(() => {
      copiedTimerRef.current = null;
      setCopied(false);
    }, 1500);
  };

  const clearActionsHideTimer = () => {
    if (actionsHideTimerRef.current === null) return;
    window.clearTimeout(actionsHideTimerRef.current);
    actionsHideTimerRef.current = null;
  };

  const showUserActions = () => {
    clearActionsHideTimer();
    setUserActionsVisible(true);
  };

  const hideUserActionsAfter = (delay: number) => {
    clearActionsHideTimer();
    actionsHideTimerRef.current = window.setTimeout(() => {
      actionsHideTimerRef.current = null;
      setUserActionsVisible(false);
    }, delay);
  };

  const userCopyAvailable = message.role === "user" && !streaming && Boolean(text);
  const userEditAvailable = message.role === "user" && !streaming && editAvailable && Boolean(entryId && onEditStart);
  const userActionsAvailable = !editing && (userCopyAvailable || userEditAvailable);
  const hasImages = blocks.some((block) => block.type === "image");
  const canSubmitEdit = editEnabled && !submitting && (draft.trim().length > 0 || hasImages);

  const cancelEdit = () => {
    if (submitting) return;
    setDraft(text);
    onEditCancel?.();
  };

  const submitEdit = async () => {
    if (!entryId || !onEdit || !canSubmitEdit) return;
    setSubmitting(true);
    try {
      await onEdit(entryId, draft);
    } catch {
      // The controller owns error presentation. Keep the draft in place.
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <>
      <article
        data-entry-id={entryId}
        className={`pi-message pi-message-${role} ${streaming ? "is-streaming" : ""} ${process ? "is-process" : ""} ${userActionsVisible ? "is-actions-visible" : ""} ${editing ? "is-editing" : ""}`}
        onPointerEnter={(event) => {
          if (userActionsAvailable && event.pointerType === "mouse") showUserActions();
        }}
        onPointerLeave={(event) => {
          if (userActionsAvailable && event.pointerType === "mouse") hideUserActionsAfter(420);
        }}
        onClick={(event) => {
          if (!userActionsAvailable) return;
          const target = event.target;
          if (target instanceof Element && target.closest("button, a")) return;
          showUserActions();
          hideUserActionsAfter(3200);
        }}
      >
      {message.role === "bashExecution" && typeof message.command === "string" && (
        <div className="pi-bash-command">$ {message.command}</div>
      )}
      {message.role === "user" && editing ? (
        <ComposerInput
          ref={textareaRef}
          className="pi-inline-message-composer"
          leading={renderedBlocks.length > 0 ? (
            <div className="pi-inline-message-composer-media">{renderedBlocks}</div>
          ) : undefined}
          rows={1}
          value={draft}
          disabled={submitting}
          aria-label="编辑消息"
          onChange={(event) => {
            setDraft(event.target.value);
            if (draftKey && history) history.data.drafts.set(draftKey, { ...history.data.drafts.get(draftKey), text: event.target.value, original: text });
          }}
          onKeyDown={(event) => {
            if (event.key === "Escape" && !submitting) {
              event.preventDefault();
              event.stopPropagation();
              cancelEdit();
              return;
            }
            if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
              event.preventDefault();
              void submitEdit();
            }
          }}
          toolbarLeft={(
            <ComposerCancelButton
              disabled={submitting}
              aria-label="取消编辑"
              title="取消编辑"
              onClick={cancelEdit}
            />
          )}
          toolbarRight={(
            <ComposerSendButton
              disabled={!canSubmitEdit}
              submitting={submitting}
              aria-label="发送"
              title="发送"
              onClick={() => void submitEdit()}
            />
          )}
        />
      ) : (
        <>
          {typeof message.content === "string" && text && (
            role === "bash"
              ? <pre className="pi-bash-output">{text}</pre>
              : <MarkdownBody isStreaming={streaming}>{text}</MarkdownBody>
          )}
          {renderedBlocks}
        </>
      )}
      {message.role === "assistant" && message.stopReason === "error" && (
        <div className="pi-message-error">
          {typeof message.errorMessage === "string" ? message.errorMessage : "模型返回错误"}
        </div>
      )}
      {userActionsAvailable && (
        <div
          className="pi-user-message-actions"
          onPointerEnter={showUserActions}
          onPointerLeave={() => hideUserActionsAfter(420)}
        >
          {userCopyAvailable && (
            <button
              type="button"
              aria-label={copied ? "消息已复制" : "复制消息"}
              title={copied ? "已复制" : "复制消息"}
              onClick={(event) => {
                event.stopPropagation();
                event.currentTarget.blur();
                void copy().catch(() => undefined).finally(() => hideUserActionsAfter(1200));
              }}
            >
              {copied ? <Check size={14} /> : <Copy size={14} />}
            </button>
          )}
          {entryId && onEditStart && (
            <button
              type="button"
              aria-label="编辑消息"
              title="编辑消息"
              onClick={(event) => {
                event.stopPropagation();
                event.currentTarget.blur();
                onEditStart(entryId);
              }}
            >
              <Pencil size={14} />
            </button>
          )}
        </div>
      )}
      {message.role === "assistant" && !process && !streaming && (text || entryId || messageTimestamp(message) !== null) && (
        <div className="pi-message-actions">
          {text && (
            <button type="button" aria-label={copied ? "已复制" : "复制"} title={copied ? "已复制" : "复制"} onClick={() => void copy()}>
              {copied ? <Check size={14} /> : <Copy size={14} />}
            </button>
          )}
          {entryId && (
            <button type="button" aria-label="从此处分支" title="从此处分支" onClick={() => void onFork(entryId)}>
              <GitFork size={14} />
            </button>
          )}
          {messageTimestamp(message) !== null && <time>{formatClock(messageTimestamp(message)!)}</time>}
        </div>
      )}
      </article>
    </>
  );
}

interface IndexedMessage {
  message: AgentMessage;
  entryId?: string;
  pending?: boolean;
}

interface ConversationTurn {
  id: string;
  run?: HistoryRun;
  anchor: IndexedMessage;
  result?: IndexedMessage;
  tail: IndexedMessage[];
}

function messageTimestamp(message: { timestamp?: unknown } | undefined): number | null {
  if (!message) return null;
  const value = message.timestamp;
  if (typeof value === "number" && Number.isFinite(value)) {
    return value < 10_000_000_000 ? value * 1000 : value;
  }
  if (typeof value === "string") {
    const parsed = Date.parse(value);
    return Number.isNaN(parsed) ? null : parsed;
  }
  return null;
}

function formatClock(timestamp: number): string {
  const value = new Date(timestamp);
  return `${String(value.getHours()).padStart(2, "0")}:${String(value.getMinutes()).padStart(2, "0")}`;
}

function elapsedLabel(elapsed: number | null): string | null {
  if (elapsed === null) return null;
  const seconds = Math.max(1, Math.floor(elapsed / 1000));
  const minutes = Math.floor(seconds / 60);
  const remainder = seconds % 60;
  return minutes > 0 ? `耗时 ${minutes}分钟 ${remainder}秒` : `耗时 ${seconds}秒`;
}

function CompactionResult({ message, entryId }: IndexedMessage) {
  const [expanded, setExpanded] = useDisclosure("compaction", entryId ?? String(message.id ?? message.timestamp ?? ""), false);
  const tokens = recordValue(message.details)?.tokensBefore;
  return <article className={`pi-compaction-result ${expanded ? "is-open" : ""}`} data-entry-id={entryId}>
    <button className="pi-compaction-summary" type="button" aria-expanded={expanded} onClick={() => setExpanded((value) => !value)}>
      <Check size={14} aria-hidden="true" />
      <span>上下文已压缩</span>
      {typeof tokens === "number" && <span className="pi-compaction-tokens">压缩前 {tokens.toLocaleString("zh-CN")} tokens</span>}
      <ChevronRight className="pi-disclosure" size={14} aria-hidden="true" />
    </button>
    {expanded && <div className="pi-compaction-body"><MarkdownBody>{messageText(message)}</MarkdownBody></div>}
  </article>;
}

function compactingElapsedLabel(seconds: number): string {
  const minutes = Math.floor(seconds / 60);
  const remainder = seconds % 60;
  return minutes > 0 ? `耗时 ${minutes} 分 ${remainder} 秒` : `耗时 ${seconds} 秒`;
}

function CompactionStatus() {
  const [startedAt] = useState(() => Date.now());
  const [elapsed, setElapsed] = useState(0);

  useEffect(() => {
    const update = () => setElapsed(Math.max(0, Math.floor((Date.now() - startedAt) / 1000)));
    update();
    const timer = window.setInterval(update, 1000);
    return () => window.clearInterval(timer);
  }, [startedAt]);

  return (
    <div className="pi-compaction-status" role="status" aria-live="polite">
      <LoaderCircle size={15} aria-hidden="true" />
      <span>正在压缩上下文</span>
      <time aria-hidden="true" dateTime={`PT${elapsed}S`}>· {compactingElapsedLabel(elapsed)}</time>
    </div>
  );
}

function displayableAssistantBlocks(message: AgentMessage): MessageContentBlock[] {
  return contentBlocks(message).filter((block) => !(
    block.type === "thinking"
    && typeof block.thinking === "string"
    && block.thinking.trim() === ""
  ));
}

function isFinalAnswerBlock(block: MessageContentBlock): boolean {
  return block.type === "text" || block.type === "image";
}

function splitFinalAssistant(message: AgentMessage): { answer: AgentMessage | null; process: AgentMessage | null } {
  if (!Array.isArray(message.content)) {
    return {
      answer: (messageText(message).trim() || message.stopReason === "error") ? message : null,
      process: null,
    };
  }
  const blocks = displayableAssistantBlocks(message);
  let lastProcessIndex = -1;
  for (let index = blocks.length - 1; index >= 0; index -= 1) {
    const block = blocks[index];
    if (block && !isFinalAnswerBlock(block)) {
      lastProcessIndex = index;
      break;
    }
  }
  const processBlocks = lastProcessIndex < 0 ? [] : blocks.slice(0, lastProcessIndex + 1);
  const answerBlocks = lastProcessIndex < 0 ? blocks : blocks.slice(lastProcessIndex + 1);
  return {
    answer: answerBlocks.length > 0 || message.stopReason === "error"
      ? { ...message, content: answerBlocks }
      : null,
    process: processBlocks.length > 0
      ? { ...message, content: processBlocks }
      : null,
  };
}

function hasDisplayableProcessMessage(message: AgentMessage): boolean {
  if (message.role === "assistant") {
    return typeof message.content === "string"
      ? message.content.trim().length > 0
      : displayableAssistantBlocks(message).length > 0;
  }
  return message.role === "toolResult" || visibleMessage(message);
}

function Turn(props: {
  turn: ConversationTurn;
  toolResults: Map<string, AgentMessage>;
  streamingMessage: AgentMessage | null;
  busy: boolean;
  compacting: boolean;
  active: boolean;
  onLoadProcess(runId: string, direction: "initial" | "older" | "newer"): Promise<boolean>;
  onExpand(runId: string): void;
  onOpenTerminal?(id: string): void;
  onFork(entryId: string): Promise<void>;
  editingEntryId: string | null;
  editAvailable: boolean;
  editEnabled: boolean;
  onEditStart(entryId: string): void;
  onEditCancel(): void;
  onEdit(entryId: string, text: string): Promise<void>;
}) {
  const history = useContext(HistoryContext)!;
  const { turn } = props;
  const running = props.active && (props.busy || props.compacting);
  const [expanded, setExpanded] = useDisclosure("turn", turn.id, running);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [, refreshClock] = useState(0);
  const [clock] = useState(() => new ElapsedClock());
  const wasBusy = useRef(running);
  const loadingRef = useRef(false);
  const lastDirectionRef = useRef<"initial" | "older" | "newer">("initial");
  const processRef = useRef<HTMLDivElement>(null);
  const detail = turn.run ? history.data.runDetails(history.sessionId, turn.run.id) : undefined;
  const processMessages = turn.tail.filter((item) => item.entryId !== turn.result?.entryId || !item.entryId);
  const fullFinal = turn.result && turn.tail.find((item) => item.entryId === turn.result?.entryId);
  const finalProcess = fullFinal && splitFinalAssistant(fullFinal.message).process;
  if (finalProcess) processMessages.push({ ...fullFinal, message: finalProcess });
  const followsLive = !turn.run || history.data.runFollowsLive(history.sessionId, turn.run.id);
  if (props.active && props.streamingMessage && followsLive) processMessages.push({ message: props.streamingMessage });
  const processVisible = Boolean(turn.run?.processCount) || processMessages.some(({ message }) => hasDisplayableProcessMessage(message));
  const startTime = messageTimestamp(turn.run ? { timestamp: turn.run.header.timestamp } : turn.anchor.message);
  const endTime = turn.run ? messageTimestamp({ timestamp: turn.run.lastActivity })
    : messageTimestamp(turn.tail.at(-1)?.message ?? turn.result?.message);
  const historicalElapsed = elapsedBetween(startTime, endTime);
  let elapsed = historicalElapsed;
  if (running && startTime !== null) elapsed = clock.read(startTime, performance.now(), Date.now());
  // Keep the last displayed duration while the final receipt is still handing
  // off to history; the previous tool entry is not the end of this run.
  else if (props.active && turn.tail.some((item) => item.pending)) elapsed = clock.lastElapsed ?? historicalElapsed;
  const processLabel = elapsedLabel(elapsed) ?? "耗时未知";
  const load = async (direction: "initial" | "older" | "newer") => {
    if (!turn.run || loadingRef.current) return;
    lastDirectionRef.current = direction;
    loadingRef.current = true;
    setLoading(true); setError("");
    try { if (!await props.onLoadProcess(turn.run.id, direction)) lastDirectionRef.current = "initial"; }
    catch (error) { setError(error instanceof Error ? error.message : String(error)); }
    finally { loadingRef.current = false; setLoading(false); }
  };
  useEffect(() => {
    if (running && !wasBusy.current) setExpanded(true);
    wasBusy.current = running;
    if (!running) return;
    const timer = window.setInterval(() => refreshClock((value) => value + 1), 1000);
    return () => window.clearInterval(timer);
  }, [running]);
  useEffect(() => {
    if (!expanded || !turn.run) return;
    const release = history.data.watchRun(history.sessionId, turn.run.id);
    void load("initial");
    return release;
  }, [expanded, turn.run?.id, turn.run?.lastEntryId, history.generation, Boolean(detail)]);
  useEffect(() => {
    if (!expanded || loading || error || lastDirectionRef.current !== "newer") return;
    const frame = requestAnimationFrame(() => {
      const button = processRef.current?.querySelector<HTMLButtonElement>(`[data-run-page="${lastDirectionRef.current}"]`);
      const transcript = processRef.current?.closest(".pi-transcript");
      if (!button || !transcript) return;
      const bounds = transcript.getBoundingClientRect(), box = button.getBoundingClientRect();
      // A result may only update an already visible tool card. If this adds no
      // scrollable height, continue until the requested edge leaves the view.
      if (box.bottom > bounds.top && box.top < bounds.bottom - 80) void load(lastDirectionRef.current);
    });
    return () => cancelAnimationFrame(frame);
  }, [expanded, loading, error, detail?.start, detail?.end]);
  const messageProps = { toolResults: props.toolResults, onFork: props.onFork, onOpenTerminal: props.onOpenTerminal,
    editAvailable: props.editAvailable, editEnabled: props.editEnabled, onEditStart: props.onEditStart,
    onEditCancel: props.onEditCancel, onEdit: props.onEdit };
  return (
    <section className="pi-turn" data-run-id={turn.id}>
      <Message {...messageProps} message={turn.anchor.message} entryId={turn.anchor.entryId} editing={props.editingEntryId === turn.anchor.entryId} />
      {(processVisible || running) && (
        <div className={`pi-turn-process ${expanded ? "is-open" : ""}`}>
          <button className="pi-turn-process-summary" data-run-summary={turn.id} type="button" aria-expanded={expanded}
            onClick={() => { props.onExpand(turn.id); setExpanded((value) => !value); }}>
            <span>{processLabel}</span>
            <ChevronRight className="pi-disclosure" size={14} />
          </button>
          {expanded && <div ref={processRef} className="pi-turn-process-body" data-run-start={detail?.start} data-run-end={detail?.end}>
            {detail?.older && <button className="pi-history-more" data-run-page="older" type="button" disabled={loading} aria-busy={loading} onClick={() => void load("older")}>{loading && <LoaderCircle className="pi-loading-spinner" size={13} />}{loading ? "正在加载过程…" : "查看更早的过程"}</button>}
            {processMessages.map(({ message, entryId }, index) => <Message {...messageProps}
              key={entryId || String(message.id ?? `process-${index}`)} message={message} entryId={entryId}
              editing={props.editingEntryId === entryId} streaming={message === props.streamingMessage} process />)}
            {loading && !detail && <div className="pi-process-loading" role="status"><LoaderCircle className="pi-loading-spinner" size={14} />正在加载过程…</div>}
            {error && <button className="pi-history-more is-error" type="button" onClick={() => void load(lastDirectionRef.current)}>加载失败，点击重试</button>}
            {(detail?.newer || detail?.latestAvailable || running && detail && !detail.following) && <button className="pi-history-more" data-run-page="newer" type="button" disabled={loading} aria-busy={loading} onClick={() => void load("newer")}>{loading && <LoaderCircle className="pi-loading-spinner" size={13} />}{loading ? "正在加载过程…" : "查看后续过程"}</button>}
            {props.compacting && <CompactionStatus />}
          </div>}
        </div>
      )}
      {turn.result && <Message {...messageProps} message={turn.result.message} entryId={turn.result.entryId} editing={props.editingEntryId === turn.result.entryId} />}
    </section>
  );
}

export function MessageList(props: MessageListProps) {
  const history = useMemo(() => ({ data: props.data, sessionId: props.sessionId, generation: props.historyWindow?.version.generation ?? "" }), [props.data, props.sessionId, props.historyWindow?.version.generation]);
  const detailed = props.historyWindow?.pages.flatMap((page) => page.runs.flatMap((run) =>
    props.data.runDetails(props.sessionId, run.id)?.pages.flatMap((page) => page.items) ?? [])) ?? [];
  const allMessages = [...props.messages, ...detailed.flatMap((item) => item.message ? [item.message] : [])];
  const toolCalls = new Set([...allMessages, ...(props.streamingMessage ? [props.streamingMessage] : [])]
    .flatMap(contentBlocks).filter((block) => block.type === "toolCall").map(toolCallID));
  const resultEntries = new Map<string, string>();
  [...props.messages.map((message, index) => ({ message, entryId: props.entryIds[index] })),
    ...detailed.map((item) => ({ message: item.message, entryId: item.node.id }))].forEach(({ message, entryId }) => {
    if (entryId && message?.role === "toolResult" && typeof message.toolCallId === "string") resultEntries.set(message.toolCallId, entryId);
  });
  return (
    <ImageCacheContext.Provider value={props.imageCache}>
      <HistoryContext.Provider value={history}>
        <ToolCallsContext.Provider value={toolCalls}>
          <ToolResultEntriesContext.Provider value={resultEntries}>
            <MessageListContent key={`${props.sessionId}:${history.generation}`} {...props} />
          </ToolResultEntriesContext.Provider>
        </ToolCallsContext.Provider>
      </HistoryContext.Provider>
    </ImageCacheContext.Provider>
  );
}

function MessageListContent({
  data,
  historyWindow,
  toolProgress,
  onLoadHistory,
  sessionId,
  messages,
  pendingMessages,
  entryIds,
  streamingMessage,
  busy,
  compacting,
  mobile,
  anchorsEnabled,
  onFork,
  onOpenTerminal,
  onEdit,
}: MessageListProps) {
  const stageRef = useRef<HTMLDivElement>(null);
  const transcriptRef = useRef<HTMLDivElement>(null);
  const turnRefs = useRef<(HTMLDivElement | null)[]>([]);
  const messageAnchorsRef = useRef<MessageAnchorsHandle>(null);
  const atBottomRef = useRef(true);
  const initializedScrollRef = useRef(false);
  const [showScrollToBottom, setShowScrollToBottom] = useState(false);
  const anchorGestureRef = useRef<AnchorGesture | null>(null);
  const anchorPreviewTimerRef = useRef<number | null>(null);
  const anchorPreviewVisibleRef = useRef(false);
  const [activeAnchorIndex, setActiveAnchorIndex] = useState(0);
  const [anchorOpen, setAnchorOpen] = useState(false);
  const [editingEntryId, setEditingEntryId] = useState<string | null>(null);
  const [loadingHistory, setLoadingHistory] = useState(false);
  const loadingHistoryRef = useRef(false);
  const currentWindowRef = useRef(historyWindow);
  currentWindowRef.current = historyWindow;
  const scrollScope = JSON.stringify([sessionId, historyWindow?.version.generation ?? ""]);
  const pendingScrollRef = useRef<{ scope: string; position: ScrollPosition } | null>(null);
  const scrollNavigationRef = useRef<{ entryId: string; destination?: number } | null>(null);
  const historyRequestRef = useRef(0);
  const stablePositionRef = useRef<ScrollPosition | null>(null);
  const previousScrollTopRef = useRef(0);
  const toolCalls = useContext(ToolCallsContext);
  const { turns, leading } = (() => {
    const nextTurns: ConversationTurn[] = [];
    const nextLeading: IndexedMessage[] = [];
    const generation = historyWindow?.version.generation ?? "";
    const messagesByID = new Map(messages.map((message, index) => [entryIds[index], message]));
    const covered = new Set<string>();
    for (const run of historyWindow?.pages.flatMap((page) => page.runs) ?? []) {
      const header = messagesByID.get(run.header.id);
      if (!header) continue;
      covered.add(run.header.id);
      if (run.result) covered.add(run.result.id);
      const detail = data.runDetails(sessionId, run.id);
      const tail = (detail?.pages.flatMap((page) => page.items) ?? []).flatMap((item) => {
        covered.add(item.node.id);
        const message = data.messageFor(sessionId, generation, item);
        return message && item.node.id !== run.header.id ? [{ message, entryId: item.node.id }] : [];
      });
      const final = run.result && messagesByID.get(run.result.id);
      nextTurns.push({ id: run.id, run, anchor: { message: header, entryId: run.header.id }, tail,
        result: final && run.result ? { message: final, entryId: run.result.id } : undefined });
    }
    // Only unsettled live messages need provisional groups. Durable boundaries
    // and final answers always come from the complete core read projection.
    for (let index = 0; index < messages.length; index++) {
      const entryId = entryIds[index], message = messages[index]!;
      if (entryId && covered.has(entryId)) continue;
      const item = { message, entryId, pending: true };
      if (message.role === "user") nextTurns.push({ id: entryId || String(message.id ?? `live-${index}`), anchor: item, tail: [] });
      else if (nextTurns.length) {
        const turn = nextTurns.at(-1)!;
        if (!turn.run || data.runFollowsLive(sessionId, turn.run.id)) turn.tail.push(item);
      }
      else nextLeading.push(item);
    }
    return { turns: nextTurns, leading: nextLeading };
  })();
  const activeTurnIndex = historyWindow && (historyWindow.end < historyWindow.total || historyWindow.preview || historyWindow.latestAvailable) ? -1 : turns.length - 1;
  const activeRun = turns[activeTurnIndex]?.run;
  const followsLive = historyWindow?.following !== false
    && (turns.length === 0 || activeTurnIndex >= 0 && (!activeRun || data.runFollowsLive(sessionId, activeRun.id)));
  const toolResults = new Map<string, AgentMessage>();
  for (const message of [...messages, ...turns.flatMap((turn) => turn.tail.map((item) => item.message))]) {
    if (message.role === "toolResult" && typeof message.toolCallId === "string") toolResults.set(message.toolCallId, message);
  }
  for (const tool of followsLive ? toolProgress ?? [] : []) {
    if (toolResults.has(tool.toolCallId)) continue;
    toolResults.set(tool.toolCallId, { role: "toolResult", toolCallId: tool.toolCallId, toolName: tool.toolName,
      ...tool.partialResult, partial: true,
      content: (tool.partialResult?.content as MessageContentBlock[] | undefined) ?? String(tool.partialResult?.text ?? "") });
  }
  const anchors = data.navigation(sessionId)?.items ?? [];
  const anchorIndices = useMemo(() => new Map(anchors.map((anchor, index) => [anchor.id, index])), [anchors]);
  const anchorIndexRef = useRef(anchorIndices);
  anchorIndexRef.current = anchorIndices;


  const retainedEditingEntry = data.editing.get(scrollScope) ?? null;
  useEffect(() => { setEditingEntryId(retainedEditingEntry); }, [retainedEditingEntry]);

  const beginMessageEdit = (entryId: string) => {
    const key = data.draftKey(sessionId, historyWindow?.version.generation ?? "", entryId);
    if (!data.drafts.has(key)) {
      const item = historyWindow?.pages.flatMap((page) => page.items).find((item) => item.node.id === entryId);
      if (item?.message) {
        const original = messageText(item.message);
        data.drafts.set(key, { text: original, original, contentToken: item.node.contentToken });
      }
    }
    data.editing.set(scrollScope, entryId);
    setEditingEntryId(entryId);
  };

  const cancelMessageEdit = () => {
    if (editingEntryId) data.drafts.delete(data.draftKey(sessionId, historyWindow?.version.generation ?? "", editingEntryId));
    data.editing.delete(scrollScope);
    setEditingEntryId(null);
  };

  const submitMessageEdit = async (entryId: string, text: string) => {
    await onEdit(entryId, text);
    data.drafts.delete(data.draftKey(sessionId, historyWindow?.version.generation ?? "", entryId));
    data.editing.delete(scrollScope);
    setEditingEntryId(null);
  };

  const capturePosition = (): ScrollPosition => {
    const transcript = transcriptRef.current;
    if (!transcript) return { entryId: "", offset: 0, bottom: true };
    const { top, bottom } = transcript.getBoundingClientRect();
    const element = [...transcript.querySelectorAll<HTMLElement>("[data-entry-id], [data-run-summary]")].find((item) => {
      const box = item.getBoundingClientRect();
      return box.bottom > top + 8 && box.top < bottom;
    });
    return { entryId: element?.dataset.entryId ?? element?.dataset.runSummary ?? "", target: element?.dataset.runSummary ? "process" : undefined, offset: (element?.getBoundingClientRect().top ?? top) - top, bottom: atBottomRef.current && currentWindowRef.current?.following !== false };
  };

  const restorePosition = (position: ScrollPosition): boolean => {
    const transcript = transcriptRef.current;
    if (!transcript) return false;
    if (position.bottom) {
      if (transcript.scrollHeight - transcript.clientHeight - transcript.scrollTop > 0.5) transcript.scrollTop = transcript.scrollHeight;
      atBottomRef.current = currentWindowRef.current?.following !== false;
      return true;
    }
    const element = [...transcript.querySelectorAll<HTMLElement>(position.target === "process" ? "[data-run-summary]" : "[data-entry-id]")].find((item) => (position.target === "process" ? item.dataset.runSummary : item.dataset.entryId) === position.entryId);
    if (!element) return false;
    const delta = element.getBoundingClientRect().top - transcript.getBoundingClientRect().top - position.offset;
    if (Math.abs(delta) > 0.5) transcript.scrollTop += delta;
    atBottomRef.current = false;
    return true;
  };

  const restorePendingPosition = () => {
    if (loadingHistoryRef.current) return;
    const pending = pendingScrollRef.current;
    if (!pending) return;
    if (pending.scope !== scrollScope) { pendingScrollRef.current = null; return; }
    if (!restorePosition(pending.position)) return;
    pendingScrollRef.current = null;
    const position = capturePosition();
    stablePositionRef.current = position;
    data.scroll.set(scrollScope, position);
    data.lastScroll.set(sessionId, position);
    previousScrollTopRef.current = transcriptRef.current?.scrollTop ?? 0;
  };

  const restoreNavigation = (): boolean => {
    const navigation = scrollNavigationRef.current, transcript = transcriptRef.current;
    if (!navigation || !transcript) return false;
    const target = [...transcript.querySelectorAll<HTMLElement>("[data-entry-id]")].find((element) => element.dataset.entryId === navigation.entryId);
    if (!target) return true;
    const destination = Math.min(transcript.scrollHeight - transcript.clientHeight,
      Math.max(0, target.getBoundingClientRect().top - transcript.getBoundingClientRect().top + transcript.scrollTop - 20));
    if (navigation.destination === undefined || Math.abs(navigation.destination - destination) > 0.5) {
      navigation.destination = destination;
      transcript.scrollTo({ top: destination, behavior: "smooth" });
    }
    if (Math.abs(transcript.scrollTop - destination) < 1) scrollNavigationRef.current = null;
    return true;
  };

  const restoreAfterRender = (position: ScrollPosition | null) => {
    if (!initializedScrollRef.current) return;
    if (pendingScrollRef.current) { restorePendingPosition(); return; }
    if (restoreNavigation()) return;
    if (!position || !restorePosition(position)) return;
    const current = capturePosition();
    stablePositionRef.current = current;
    data.scroll.set(scrollScope, current);
    data.lastScroll.set(sessionId, current);
    previousScrollTopRef.current = transcriptRef.current?.scrollTop ?? 0;
  };

  const loadHistory = async (direction: "older" | "newer" | "latest", aroundId?: string) => {
    if (loadingHistoryRef.current && direction !== "latest" && !aroundId) return;
    const request = ++historyRequestRef.current;
    loadingHistoryRef.current = true;
    setLoadingHistory(true);
    scrollNavigationRef.current = null;
    pendingScrollRef.current = direction === "latest" || aroundId
      ? { scope: scrollScope, position: aroundId ? { entryId: aroundId, offset: 24, bottom: false } : { entryId: "", offset: 0, bottom: true } }
      : null;
    atBottomRef.current = false;
    try {
      await onLoadHistory(direction, aroundId);
      if (request === historyRequestRef.current && aroundId && !data.containsEntry(sessionId, aroundId)) pendingScrollRef.current = null;
    } catch { if (request === historyRequestRef.current) pendingScrollRef.current = null; }
    finally {
      if (request === historyRequestRef.current) { loadingHistoryRef.current = false; setLoadingHistory(false); }
    }
  };
  const loadProcess = async (runId: string, direction: "initial" | "older" | "newer") => {
    if (direction !== "initial" && (pendingScrollRef.current || scrollNavigationRef.current)) return false;
    const request = historyRequestRef.current;
    if (direction !== "initial") atBottomRef.current = false;
    await data.loadRun(sessionId, runId, direction);
    return request === historyRequestRef.current;
  };
  const prepareExpansion = (runId: string) => {
    const transcript = transcriptRef.current;
    const summary = transcript && [...transcript.querySelectorAll<HTMLElement>("[data-run-summary]")].find((element) => element.dataset.runSummary === runId);
    if (!transcript || !summary) return;
    atBottomRef.current = false;
    stablePositionRef.current = { entryId: runId, target: "process", offset: summary.getBoundingClientRect().top - transcript.getBoundingClientRect().top, bottom: false };
  };
  const loadHistoryRef = useRef(loadHistory);
  loadHistoryRef.current = loadHistory;

  const syncActiveAnchor = () => {
    const transcript = transcriptRef.current;
    // Initial DOM geometry is not a saved browsing position. Restore the
    // viewport before observations are allowed to overwrite its cache.
    if (!transcript || !initializedScrollRef.current || pendingScrollRef.current) return;
    atBottomRef.current = atBottomRef.current && transcript.scrollHeight - transcript.scrollTop - transcript.clientHeight < 96 && currentWindowRef.current?.following !== false;
    const position = capturePosition();
    stablePositionRef.current = position;
    data.scroll.set(scrollScope, position);
    data.lastScroll.set(sessionId, position);
    setShowScrollToBottom(!atBottomRef.current);
    if (turns.length === 0) return;
    const focus = transcript.getBoundingClientRect().top + transcript.clientHeight * 0.28;
    let nearest = 0;
    let distance = Number.POSITIVE_INFINITY;
    turnRefs.current.forEach((element, index) => {
      if (!element) return;
      const nextDistance = Math.abs(element.getBoundingClientRect().top - focus);
      if (nextDistance < distance) {
        distance = nextDistance;
        nearest = anchorIndexRef.current.get(turns[index]?.anchor.entryId ?? "") ?? nearest;
      }
    });
    setActiveAnchorIndex(nearest);
  };

  const scrollToAnchor = (index: number) => {
    const transcript = transcriptRef.current, anchor = anchors[index];
    if (!transcript || !anchor) return;
    const target = [...transcript.querySelectorAll<HTMLElement>("[data-entry-id]")].find((element) => element.dataset.entryId === anchor.id);
    setActiveAnchorIndex(index);
    if (!target) { void loadHistory("latest", anchor.id); return; }
    historyRequestRef.current++;
    pendingScrollRef.current = null;
    loadingHistoryRef.current = false; setLoadingHistory(false);
    atBottomRef.current = false;
    scrollNavigationRef.current = { entryId: anchor.id };
    // Commit the new reading window before starting the animation. Removing a
    // live tail can otherwise clamp scrollTop and cancel an in-progress scroll.
    data.browseHistory(sessionId);
  };

  const interruptNavigation = () => {
    if (!scrollNavigationRef.current && !pendingScrollRef.current) return;
    scrollNavigationRef.current = null;
    pendingScrollRef.current = null;
    historyRequestRef.current++;
    data.cancelHistoryNavigation(sessionId);
    loadingHistoryRef.current = false;
    setLoadingHistory(false);
    const transcript = transcriptRef.current;
    if (transcript) transcript.scrollTo({ top: transcript.scrollTop, behavior: "instant" });
  };

  const focused = data.focusedEntry(sessionId);
  useLayoutEffect(() => {
    if (!focused || focused.entryId) return;
    pendingScrollRef.current = null;
    scrollNavigationRef.current = null;
    atBottomRef.current = true;
    if (transcriptRef.current) transcriptRef.current.scrollTop = transcriptRef.current.scrollHeight;
    stablePositionRef.current = { entryId: "", offset: 0, bottom: true };
    setShowScrollToBottom(false);
  }, [focused?.serial]);

  useLayoutEffect(() => { syncActiveAnchor(); }, [anchors.length]);

  const anchorTrackMetrics = () => {
    const track = document.querySelector<HTMLElement>("#pi-message-anchors .pi-anchor-track");
    if (!track || anchors.length === 0) return null;
    const bounds = track.getBoundingClientRect();
    const innerTop = bounds.top + 8;
    const innerHeight = Math.max(0, bounds.height - 16);
    if (bounds.width <= 0 || innerHeight <= 0) return null;
    return {
      innerTop,
      innerHeight,
      slotHeight: innerHeight / anchors.length,
    };
  };

  const anchorIndexAtY = (clientY: number, gesture: AnchorGesture): number | null => {
    const metrics = gesture.trackMetrics ?? anchorTrackMetrics();
    if (!metrics) return null;
    gesture.trackMetrics = metrics;
    if (clientY < metrics.innerTop || clientY > metrics.innerTop + metrics.innerHeight) return null;
    const estimated = Math.round((clientY - metrics.innerTop) / metrics.slotHeight - 0.5);
    return Math.max(0, Math.min(anchors.length - 1, estimated));
  };

  const clearAnchorPreviewTimer = () => {
    if (anchorPreviewTimerRef.current === null) return;
    window.clearTimeout(anchorPreviewTimerRef.current);
    anchorPreviewTimerRef.current = null;
  };

  const hideAnchorPreview = () => {
    clearAnchorPreviewTimer();
    anchorPreviewVisibleRef.current = false;
    messageAnchorsRef.current?.setPreviewPosition(null);
    messageAnchorsRef.current?.setPreviewIndex(null);
  };

  const positionAnchorPreview = (gesture: AnchorGesture, clientY: number) => {
    messageAnchorsRef.current?.setPreviewPosition({
      clientY,
      stageTop: gesture.stageTop,
      stageBottom: gesture.stageBottom,
    });
  };

  const scheduleAnchorPreview = (index: number) => {
    clearAnchorPreviewTimer();
    anchorPreviewTimerRef.current = window.setTimeout(() => {
      anchorPreviewTimerRef.current = null;
      const gesture = anchorGestureRef.current;
      if (
        !gesture?.engaged
        || !gesture.hasSelection
        || !gesture.insideHitRegion
        || gesture.index !== index
      ) return;
      positionAnchorPreview(gesture, gesture.lastY);
      anchorPreviewVisibleRef.current = true;
      messageAnchorsRef.current?.setPreviewIndex(index);
    }, ANCHOR_PREVIEW_DWELL_MS);
  };

  const onAnchorPointerDown = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (!mobile || !anchorsEnabled || anchors.length === 0 || event.pointerType !== "touch" || !event.isPrimary) return;
    const target = event.target instanceof Element ? event.target : null;
    if (
      (
        blocksEdgeGestureStart(target)
        || isTextSelectionInteraction(target, event.clientX, event.clientY)
      )
      && !target?.closest("#pi-message-anchors")
    ) return;
    const bounds = event.currentTarget.getBoundingClientRect();
    if (bounds.right - event.clientX > MOBILE_EDGE_SIZE) return;
    anchorGestureRef.current = {
      pointerId: event.pointerId,
      startX: event.clientX,
      startY: event.clientY,
      lastY: event.clientY,
      engaged: false,
      hasSelection: false,
      insideHitRegion: false,
      index: activeAnchorIndex,
      trackMetrics: null,
      stageTop: bounds.top,
      stageBottom: bounds.bottom,
    };
  };

  const onAnchorPointerMove = (event: ReactPointerEvent<HTMLDivElement>) => {
    const gesture = anchorGestureRef.current;
    if (!gesture || gesture.pointerId !== event.pointerId) return;
    const dx = event.clientX - gesture.startX;
    const dy = event.clientY - gesture.startY;
    gesture.lastY = event.clientY;
    if (!gesture.engaged) {
      if (isTextSelectionInteraction(event.target, event.clientX, event.clientY)) {
        anchorGestureRef.current = null;
        return;
      }
      if (Math.abs(dy) > TOUCH_SLOP && Math.abs(dy) > Math.abs(dx)) {
        anchorGestureRef.current = null;
        return;
      }
      if (dx > -TOUCH_SLOP || Math.abs(dx) < Math.abs(dy) * 1.1) return;
      gesture.engaged = true;
      event.currentTarget.setPointerCapture(event.pointerId);
      setAnchorOpen(true);
    }
    event.preventDefault();
    const index = anchorIndexAtY(event.clientY, gesture);
    if (index === null) {
      gesture.insideHitRegion = false;
      clearAnchorPreviewTimer();
      return;
    }
    const changed = !gesture.hasSelection || gesture.index !== index;
    const entered = !gesture.insideHitRegion;
    gesture.hasSelection = true;
    gesture.insideHitRegion = true;
    gesture.index = index;
    if (changed) {
      messageAnchorsRef.current?.setGestureIndex(index);
    }
    if (anchorPreviewVisibleRef.current) {
      positionAnchorPreview(gesture, event.clientY);
      if (changed || entered) messageAnchorsRef.current?.setPreviewIndex(index);
    } else if (changed || entered) {
      scheduleAnchorPreview(index);
    }
  };

  const finishAnchorGesture = (event: ReactPointerEvent<HTMLDivElement>, cancelled: boolean) => {
    const gesture = anchorGestureRef.current;
    if (!gesture || gesture.pointerId !== event.pointerId) return;
    if (event.currentTarget.hasPointerCapture(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId);
    }
    const releasedIndex = anchorIndexAtY(event.clientY, gesture);
    if (releasedIndex !== null) {
      gesture.hasSelection = true;
      gesture.index = releasedIndex;
    }
    const selected = gesture.hasSelection ? gesture.index : null;
    anchorGestureRef.current = null;
    messageAnchorsRef.current?.setGestureIndex(null);
    hideAnchorPreview();
    if (!gesture.engaged) return;
    setAnchorOpen(false);
    if (cancelled || selected === null) return;
    scrollToAnchor(selected);
  };

  useLayoutEffect(() => {
    const transcript = transcriptRef.current;
    if (!transcript) return;
    const saved = data.scroll.get(scrollScope) ?? data.lastScroll.get(sessionId);
    if (!saved || !restorePosition(saved)) {
      transcript.scrollTop = transcript.scrollHeight;
      atBottomRef.current = currentWindowRef.current?.following !== false;
      if (saved?.entryId) {
        pendingScrollRef.current = { scope: scrollScope, position: saved };
        void data.pageHistory(sessionId, "latest", saved.entryId).catch(() => { pendingScrollRef.current = null; });
      }
    }
    setShowScrollToBottom(false);
    anchorGestureRef.current = null;
    messageAnchorsRef.current?.setGestureIndex(null);
    clearAnchorPreviewTimer();
    anchorPreviewVisibleRef.current = false;
    messageAnchorsRef.current?.setPreviewIndex(null);
    messageAnchorsRef.current?.setPreviewPosition(null);
    setAnchorOpen(false);
    setActiveAnchorIndex(Math.max(0, anchors.length - 1));
    initializedScrollRef.current = true;
    previousScrollTopRef.current = transcript.scrollTop;
    syncActiveAnchor();
    const frame = requestAnimationFrame(() => {
      if (saved) restorePosition(saved);
      else transcript.scrollTop = transcript.scrollHeight;
      previousScrollTopRef.current = transcript.scrollTop;
      syncActiveAnchor();
    });
    return () => { initializedScrollRef.current = false; cancelAnimationFrame(frame); };
  }, [scrollScope]);

  useLayoutEffect(restorePendingPosition);

  useLayoutEffect(() => {
    const transcript = transcriptRef.current;
    if (!transcript || !atBottomRef.current) return;
    transcript.scrollTop = transcript.scrollHeight;
  }, [messages.length, pendingMessages.length, streamingMessage, busy, compacting]);

  useEffect(() => {
    if (!mobile || anchorsEnabled) return;
    anchorGestureRef.current = null;
    messageAnchorsRef.current?.setGestureIndex(null);
    setAnchorOpen(false);
    hideAnchorPreview();
  }, [anchorsEnabled, mobile]);

  useEffect(() => {
    if (!mobile || !anchorOpen) return;
    const dismiss = (event: PointerEvent) => {
      const target = event.target instanceof Element ? event.target : null;
      if (target?.closest("#pi-message-anchors")) return;
      setAnchorOpen(false);
      messageAnchorsRef.current?.setGestureIndex(null);
      hideAnchorPreview();
    };
    document.addEventListener("pointerdown", dismiss, true);
    return () => document.removeEventListener("pointerdown", dismiss, true);
  }, [anchorOpen, mobile]);

  useEffect(() => () => {
    if (anchorPreviewTimerRef.current !== null) {
      window.clearTimeout(anchorPreviewTimerRef.current);
    }
  }, []);

  // The history owner can change while this conversation is already visible.
  // Measure its composer and restore the viewport before the remounted list paints.
  useLayoutEffect(() => {
    const transcript = transcriptRef.current;
    if (!transcript) return;
    const composer = stageRef.current?.closest(".pi-conversation")?.querySelector(".pi-composer-wrap");
    const onScroll = () => {
      if (!initializedScrollRef.current) return;
      const pending = pendingScrollRef.current;
      if (pending) return;
      const upwards = transcript.scrollTop < previousScrollTopRef.current;
      const downwards = transcript.scrollTop > previousScrollTopRef.current;
      previousScrollTopRef.current = transcript.scrollTop;
      if (upwards) atBottomRef.current = false;
      else if (downwards && !scrollNavigationRef.current) atBottomRef.current = transcript.scrollHeight - transcript.scrollTop - transcript.clientHeight < 96;
      syncActiveAnchor();
      if (restoreNavigation()) return;
      if (loadingHistoryRef.current) return;
      const bounds = transcript.getBoundingClientRect();
      const reading = capturePosition();
      const readingElement = [...transcript.querySelectorAll<HTMLElement>("[data-entry-id]")].find((element) => element.dataset.entryId === reading.entryId);
      const readingProcess = readingElement?.closest(".pi-turn-process-body");
      const processPage = [...transcript.querySelectorAll<HTMLButtonElement>(`[data-run-page="${upwards ? "older" : "newer"}"]:not(:disabled)`)]
        .find((button) => {
          const box = button.getBoundingClientRect();
          return button.closest(".pi-turn-process-body") === readingProcess && box.bottom > bounds.top - 100 && box.top < bounds.bottom + 100;
        });
      if ((upwards || downwards) && processPage) { processPage.click(); return; }
      const window = currentWindowRef.current;
      if (upwards && transcript.scrollTop < 320 && window?.older) void loadHistoryRef.current("older");
      else if (downwards && transcript.scrollHeight - transcript.scrollTop - transcript.clientHeight < 320 && window && !window.following) {
        if (window.newer || window.end < window.total || window.latestAvailable && !window.preview) void loadHistoryRef.current("newer");
      }
    };
    const onResize = () => {
      if (composer) {
        stageRef.current?.style.setProperty("--pi-composer-height", `${composer.getBoundingClientRect().height}px`);
      }
      restorePendingPosition();
      if (restoreNavigation()) return;
      if (atBottomRef.current) transcript.scrollTop = transcript.scrollHeight;
      else if (!pendingScrollRef.current && stablePositionRef.current) restorePosition(stablePositionRef.current);
      previousScrollTopRef.current = transcript.scrollTop;
      if (!loadingHistoryRef.current && !pendingScrollRef.current) syncActiveAnchor();
    };
    transcript.addEventListener("scroll", onScroll, { passive: true });
    const observer = new ResizeObserver(onResize);
    observer.observe(transcript);
    if (transcript.firstElementChild) observer.observe(transcript.firstElementChild);
    if (composer) observer.observe(composer);
    onResize();
    return () => {
      transcript.removeEventListener("scroll", onScroll);
      observer.disconnect();
    };
  }, [turns.length, scrollScope]);

  return (
    <div
      ref={stageRef}
      className="pi-message-stage"
      onPointerDownCapture={(event) => { interruptNavigation(); onAnchorPointerDown(event); }}
      onWheelCapture={interruptNavigation}
      onKeyDownCapture={(event) => {
        if (["ArrowUp", "ArrowDown", "PageUp", "PageDown", "Home", "End", " "].includes(event.key)
          && !(event.target as Element).closest("input, textarea, [contenteditable]")) interruptNavigation();
      }}
      onPointerMoveCapture={onAnchorPointerMove}
      onPointerUpCapture={(event) => finishAnchorGesture(event, false)}
      onPointerCancelCapture={(event) => finishAnchorGesture(event, true)}
    >
      <div className="pi-transcript-scroll pi-overlay-scroll-host">
        <TranscriptViewport viewportRef={transcriptRef} start={historyWindow?.start} end={historyWindow?.end} loading={loadingHistory}
          capture={() => initializedScrollRef.current ? capturePosition() : null} restore={restoreAfterRender}>
            {historyWindow?.older && <button className="pi-history-more" type="button" disabled={loadingHistory} onClick={() => void loadHistory("older")}>查看更早的对话</button>}
            {leading.map(({ message, entryId }, index) => (
              <Message
                key={entryId || String(message.id ?? `leading-${message.role}-${index}`)}
                message={message}
                toolResults={toolResults}
                entryId={entryId}
                onFork={onFork}
                onOpenTerminal={onOpenTerminal}
                editing={editingEntryId === entryId}
                editAvailable={!busy && editingEntryId === null}
                editEnabled={!busy}
                onEditStart={beginMessageEdit}
                onEditCancel={cancelMessageEdit}
                onEdit={submitMessageEdit}
              />
            ))}
            {turns.map((turn, index) => (
              <div
                className="pi-turn-anchor"
                key={turn.id}
                ref={(element) => {
                  turnRefs.current[index] = element;
                }}
              >
                <Turn
                  turn={turn}
                  toolResults={toolResults}
                  streamingMessage={index === activeTurnIndex ? streamingMessage : null}
                  busy={index === activeTurnIndex && busy && !compacting && (!turn.result || streamingMessage !== null)}
                  compacting={index === activeTurnIndex && compacting}
                  active={index === activeTurnIndex}
                  onLoadProcess={loadProcess}
                  onExpand={prepareExpansion}
                  onFork={onFork}
                  onOpenTerminal={onOpenTerminal}
                  editingEntryId={editingEntryId}
                  editAvailable={!busy && editingEntryId === null}
                  editEnabled={!busy}
                  onEditStart={beginMessageEdit}
                  onEditCancel={cancelMessageEdit}
                  onEdit={submitMessageEdit}
                />
              </div>
            ))}
            {turns.length === 0 && streamingMessage && (
              <Message message={streamingMessage} toolResults={toolResults} onFork={onFork} onOpenTerminal={onOpenTerminal} streaming />
            )}
            {turns.length === 0 && busy && !compacting && !streamingMessage && (
              <div className="pi-working" role="status">
                <span />
                <span />
                <span />
              </div>
            )}
            {turns.length === 0 && compacting && <CompactionStatus />}
            {(followsLive ? toolProgress ?? [] : []).filter((tool) => !toolCalls.has(tool.toolCallId)).map((tool) => <ToolCall key={tool.toolCallId}
              block={{ type: "toolCall", id: tool.toolCallId, name: tool.toolName, arguments: tool.args }}
              result={toolResults.get(tool.toolCallId)} streaming onOpenTerminal={onOpenTerminal} />)}
            {pendingMessages.map((message, index) => (
              <Message
                key={String(message.id ?? `pending-${index}`)}
                message={message}
                toolResults={toolResults}
                onFork={onFork}
                onOpenTerminal={onOpenTerminal}
              />
            ))}
            {(historyWindow?.newer || historyWindow?.latestAvailable && !historyWindow.preview) && <button className="pi-history-more" type="button" disabled={loadingHistory} onClick={() => void loadHistory("newer")}>查看后续对话</button>}
        </TranscriptViewport>
        <OverlayScrollbar viewportRef={transcriptRef} />
        {loadingHistory && <div className="pi-history-loading" role="status"><LoaderCircle className="pi-loading-spinner" size={14} />正在加载历史…</div>}
        {showScrollToBottom && (
          <button
            className="pi-scroll-to-bottom"
            type="button"
            aria-label="回到底部"
            title="回到底部"
            data-swipe-ignore
            onClick={() => {
              const transcript = transcriptRef.current;
              if (!transcript) return;
              if (currentWindowRef.current?.following === false || currentWindowRef.current?.latestAvailable || !followsLive) {
                void loadHistory("latest");
                return;
              }
              atBottomRef.current = true;
              transcript.scrollTop = transcript.scrollHeight;
              setShowScrollToBottom(false);
              syncActiveAnchor();
            }}
          >
            <ArrowDown size={18} />
          </button>
        )}
      </div>
      <MessageAnchors
        ref={messageAnchorsRef}
        items={anchors}
        activeIndex={activeAnchorIndex}
        mobile={mobile}
        open={!mobile || (anchorsEnabled && anchorOpen)}
        onSelect={(index) => {
          scrollToAnchor(index);
          if (!mobile) return;
          setAnchorOpen(false);
          messageAnchorsRef.current?.setGestureIndex(null);
          hideAnchorPreview();
        }}
      />
    </div>
  );
}
