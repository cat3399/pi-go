import {
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type PointerEvent as ReactPointerEvent,
  type ReactNode,
} from "react";
import { BookOpen, Check, ChevronRight, Copy, FilePenLine, GitFork, Search, SquareTerminal } from "lucide-react";
import type { AgentMessage, MessageContentBlock } from "../contracts";
import { MarkdownBody } from "../content/MarkdownBody";
import { OverlayScrollbar } from "../primitives/OverlayScrollbar";
import { messageText, visibleMessage } from "./message";
import { MessageAnchors, type MessageAnchorsHandle } from "./MessageAnchors";
import { blocksEdgeGestureStart, isTextSelectionInteraction } from "./edge-gesture-target";

interface MessageListProps {
  sessionId: string;
  messages: AgentMessage[];
  pendingMessages: AgentMessage[];
  entryIds: string[];
  streamingMessage: AgentMessage | null;
  busy: boolean;
  mobile: boolean;
  anchorsEnabled: boolean;
  onFork(entryId: string): Promise<void>;
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

function imageSource(block: MessageContentBlock): string {
  if (typeof block.data === "string" && typeof block.mimeType === "string") {
    return `data:${block.mimeType};base64,${block.data}`;
  }
  const source = block.source;
  if (!source || typeof source !== "object" || Array.isArray(source)) return "";
  const value = source as Record<string, unknown>;
  if (value.type === "base64" && typeof value.data === "string") {
    return `data:${typeof value.media_type === "string" ? value.media_type : "application/octet-stream"};base64,${value.data}`;
  }
  return typeof value.url === "string" ? value.url : "";
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
          {result && !failed && <Check size={12} />}
          {streaming && !result ? (name === "write" ? "写入中" : "修改中") : failed ? "失败" : result ? "成功" : "等待"}
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
    case "bash":
      return { icon: SquareTerminal, verb: complete ? "已运行" : "正在运行", target: inputString(input, "command"), card: "Shell" };
    default:
      return { icon: SquareTerminal, verb: complete ? "已调用" : "正在调用", target: name, card: name };
  }
}

function ToolDataSection(props: {
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
    <div className={`pi-tool-section pi-overlay-scroll-host is-${props.kind}`}>
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
}: {
  block: MessageContentBlock;
  result?: AgentMessage;
  streaming: boolean;
}) {
  const name = toolCallName(block);
  const [expanded, setExpanded] = useState(() => name === "edit" || name === "write");
  const input = toolInputRecord(block);
  const output = resultText(result);
  const failed = result?.isError === true;
  const complete = Boolean(result);
  const presentation = toolPresentation(name, input, complete);
  const Icon = presentation.icon;
  const serializedInput = JSON.stringify(input, null, 2);
  const command = inputString(input, "command");
  const inputCopyValue = name === "bash" ? command : serializedInput;
  const hasFileChange = name === "edit" || name === "write";

  return (
    <div className={`pi-tool ${failed ? "is-error" : ""} ${expanded ? "is-open" : ""}`}>
      <button className="pi-tool-summary" type="button" aria-expanded={expanded} onClick={() => setExpanded((value) => !value)}>
        <Icon size={15} />
        <span>{failed ? "调用失败" : presentation.verb}</span>
        {presentation.target && <code>{presentation.target}</code>}
        <ChevronRight className="pi-disclosure" size={14} />
      </button>
      {expanded && (
        <div className={`pi-tool-body ${hasFileChange ? "is-file-change" : ""}`}>
          {hasFileChange ? (
            <FileChangeCard name={name} input={input} result={result} streaming={streaming} />
          ) : (
            <>
              <div className="pi-tool-card-header">
                <span>{presentation.card}</span>
                <span className="pi-tool-card-status">
                  {result && !failed && <Check size={12} />}
                  {streaming && !result ? "运行中" : failed ? "失败" : result ? "成功" : "等待"}
                </span>
              </div>
              <ToolDataSection kind="input" value={inputCopyValue}>
                <pre className="pi-tool-input">{name === "bash" ? `$ ${command}` : serializedInput}</pre>
              </ToolDataSection>
            </>
          )}
          {output && (!hasFileChange || failed) && (
            <ToolDataSection kind="output" value={output}>
              <pre className="pi-tool-output">{output}</pre>
            </ToolDataSection>
          )}
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
  const [expanded, setExpanded] = useState(true);
  const complete = props.blocks.every((block) => props.toolResults.has(toolCallID(block)));
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

function ThinkingBlock({ block, streaming }: { block: MessageContentBlock; streaming: boolean }) {
  const [expanded, setExpanded] = useState(false);
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
  process = false,
}: {
  message: AgentMessage;
  streaming?: boolean;
  toolResults: Map<string, AgentMessage>;
  entryId?: string;
  onFork(entryId: string): Promise<void>;
  process?: boolean;
}) {
  const [copied, setCopied] = useState(false);
  const [userCopyVisible, setUserCopyVisible] = useState(false);
  const copyHideTimerRef = useRef<number | null>(null);
  const copiedTimerRef = useRef<number | null>(null);

  useEffect(() => () => {
    if (copyHideTimerRef.current !== null) window.clearTimeout(copyHideTimerRef.current);
    if (copiedTimerRef.current !== null) window.clearTimeout(copiedTimerRef.current);
  }, []);

  if (!visibleMessage(message)) return null;
  const blocks = contentBlocks(message);
  const text = messageText(message);
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
    if (block.type === "text" && typeof block.text === "string" && block.text) {
      renderedBlocks.push(<MarkdownBody key={index} isStreaming={streaming}>{block.text}</MarkdownBody>);
    } else if (block.type === "thinking" && typeof block.thinking === "string" && block.thinking) {
      renderedBlocks.push(<ThinkingBlock key={index} block={block} streaming={streaming} />);
    } else if (block.type === "toolCall") {
      const id = toolCallID(block);
      renderedBlocks.push(
        <ToolCall key={id || index} block={block} result={toolResults.get(id)} streaming={streaming} />,
      );
    } else if (block.type === "image") {
      const src = imageSource(block);
      if (src) renderedBlocks.push(<img className="pi-message-image" key={index} src={src} alt="" />);
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

  const clearCopyHideTimer = () => {
    if (copyHideTimerRef.current === null) return;
    window.clearTimeout(copyHideTimerRef.current);
    copyHideTimerRef.current = null;
  };

  const showUserCopy = () => {
    clearCopyHideTimer();
    setUserCopyVisible(true);
  };

  const hideUserCopyAfter = (delay: number) => {
    clearCopyHideTimer();
    copyHideTimerRef.current = window.setTimeout(() => {
      copyHideTimerRef.current = null;
      setUserCopyVisible(false);
    }, delay);
  };

  const userCopyAvailable = message.role === "user" && !streaming && Boolean(text);

  return (
    <article
      className={`pi-message pi-message-${role} ${streaming ? "is-streaming" : ""} ${process ? "is-process" : ""} ${userCopyVisible ? "is-copy-visible" : ""}`}
      onPointerEnter={(event) => {
        if (userCopyAvailable && event.pointerType === "mouse") showUserCopy();
      }}
      onPointerLeave={(event) => {
        if (userCopyAvailable && event.pointerType === "mouse") hideUserCopyAfter(420);
      }}
      onClick={(event) => {
        if (!userCopyAvailable) return;
        const target = event.target;
        if (target instanceof Element && target.closest("button, a")) return;
        showUserCopy();
        hideUserCopyAfter(3200);
      }}
    >
      {message.role === "bashExecution" && typeof message.command === "string" && (
        <div className="pi-bash-command">$ {message.command}</div>
      )}
      {typeof message.content === "string" && text && (
        role === "bash"
          ? <pre className="pi-bash-output">{text}</pre>
          : <MarkdownBody isStreaming={streaming}>{text}</MarkdownBody>
      )}
      {renderedBlocks}
      {message.role === "assistant" && message.stopReason === "error" && (
        <div className="pi-message-error">
          {typeof message.errorMessage === "string" ? message.errorMessage : "模型返回错误"}
        </div>
      )}
      {userCopyAvailable && (
        <button
          className="pi-user-message-copy"
          type="button"
          aria-label={copied ? "消息已复制" : "复制消息"}
          title={copied ? "已复制" : "复制消息"}
          onPointerEnter={showUserCopy}
          onPointerLeave={() => hideUserCopyAfter(420)}
          onClick={(event) => {
            event.stopPropagation();
            event.currentTarget.blur();
            void copy().catch(() => undefined).finally(() => hideUserCopyAfter(1200));
          }}
        >
          {copied ? <Check size={14} /> : <Copy size={14} />}
        </button>
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
  );
}

interface IndexedMessage {
  message: AgentMessage;
  entryId?: string;
}

interface ConversationTurn {
  anchor: IndexedMessage;
  tail: IndexedMessage[];
}

function messageTimestamp(message: AgentMessage | undefined): number | null {
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

function elapsedLabel(start: AgentMessage, endTime: number | null): string | null {
  const startTime = messageTimestamp(start);
  if (startTime === null || endTime === null || endTime < startTime) return null;
  const seconds = Math.max(1, Math.floor((endTime - startTime) / 1000));
  const minutes = Math.floor(seconds / 60);
  const remainder = seconds % 60;
  return minutes > 0 ? `耗时 ${minutes}分钟 ${remainder}秒` : `耗时 ${seconds}秒`;
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

function hasFinalAssistantAnswer(message: AgentMessage): boolean {
  if (message.role !== "assistant") return false;
  if (!Array.isArray(message.content)) return messageText(message).trim().length > 0;
  const split = splitFinalAssistant(message);
  return Boolean(split.answer && contentBlocks(split.answer).some((block) => (
    block.type === "image"
    || (block.type === "text" && typeof block.text === "string" && block.text.trim().length > 0)
  )));
}

function completedFinalIndex(messages: IndexedMessage[]): number {
  for (let index = messages.length - 1; index >= 0; index -= 1) {
    const message = messages[index]?.message;
    if (message && hasFinalAssistantAnswer(message)) return index;
  }
  for (let index = messages.length - 1; index >= 0; index -= 1) {
    const message = messages[index]?.message;
    if (message?.role === "assistant") return index;
  }
  return -1;
}

function isGroupAnchor(message: AgentMessage): boolean {
  return message.role === "user" || (message.role === "custom" && message.customType === "compaction");
}

function hasDisplayableProcessMessage(message: AgentMessage): boolean {
  if (message.role === "assistant") {
    return typeof message.content === "string"
      ? message.content.trim().length > 0
      : displayableAssistantBlocks(message).length > 0;
  }
  return visibleMessage(message);
}

function Turn(props: {
  turn: ConversationTurn;
  toolResults: Map<string, AgentMessage>;
  streamingMessage: AgentMessage | null;
  busy: boolean;
  active: boolean;
  onFork(entryId: string): Promise<void>;
}) {
  const [expanded, setExpanded] = useState(props.active && props.busy);
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    setExpanded(props.active && props.busy);
  }, [props.active, props.busy]);

  useEffect(() => {
    if (!props.active || !props.busy) return;
    setNow(Date.now());
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [props.active, props.busy]);

  const tail = props.turn.tail;
  const historicalFinalIndex = props.active && props.busy ? -1 : completedFinalIndex(tail);
  const historicalFinal = historicalFinalIndex >= 0 ? tail[historicalFinalIndex] : undefined;
  const finalSplit = historicalFinal ? splitFinalAssistant(historicalFinal.message) : null;
  const finalAnswer = finalSplit?.answer && historicalFinal
    ? { ...historicalFinal, message: finalSplit.answer }
    : undefined;
  const processMessages = historicalFinalIndex >= 0
    ? tail.slice(0, historicalFinalIndex)
    : [...tail];
  if (finalSplit?.process && historicalFinal) {
    processMessages.push({ ...historicalFinal, message: finalSplit.process });
  }
  if (props.active && props.streamingMessage) {
    processMessages.push({ message: props.streamingMessage });
  }
  const trailingMessages = historicalFinalIndex >= 0 ? tail.slice(historicalFinalIndex + 1) : [];
  const processVisible = processMessages.some(({ message }) => hasDisplayableProcessMessage(message));
  const elapsedEndTime = props.active && props.busy
    ? now
    : messageTimestamp(historicalFinal?.message)
      ?? [...tail].reverse().map(({ message }) => messageTimestamp(message)).find((value) => value !== null)
      ?? null;
  const processLabel = elapsedLabel(props.turn.anchor.message, elapsedEndTime);
  const processBody = (
    <div className="pi-turn-process-body">
      {processMessages.map(({ message, entryId }, index) => (
        <Message
          key={String(message.id ?? `process-${index}`)}
          message={message}
          entryId={entryId}
          toolResults={props.toolResults}
          onFork={props.onFork}
          streaming={message === props.streamingMessage}
          process
        />
      ))}
    </div>
  );

  return (
    <section className="pi-turn">
      <Message
        message={props.turn.anchor.message}
        entryId={props.turn.anchor.entryId}
        toolResults={props.toolResults}
        onFork={props.onFork}
      />
      {(processVisible || (props.active && props.busy)) && processLabel && (
        <div className={`pi-turn-process ${expanded ? "is-open" : ""}`}>
          <button className="pi-turn-process-summary" type="button" aria-expanded={expanded} onClick={() => setExpanded((value) => !value)}>
            <span>{processLabel}</span>
            <ChevronRight className="pi-disclosure" size={14} />
          </button>
          {expanded && processBody}
        </div>
      )}
      {processVisible && !processLabel && processBody}
      {finalAnswer && (
        <Message
          message={finalAnswer.message}
          entryId={finalAnswer.entryId}
          toolResults={props.toolResults}
          onFork={props.onFork}
        />
      )}
      {trailingMessages.map(({ message, entryId }, index) => (
        <Message
          key={String(message.id ?? `trailing-${index}`)}
          message={message}
          entryId={entryId}
          toolResults={props.toolResults}
          onFork={props.onFork}
        />
      ))}
    </section>
  );
}

export function MessageList({
  sessionId,
  messages,
  pendingMessages,
  entryIds,
  streamingMessage,
  busy,
  mobile,
  anchorsEnabled,
  onFork,
}: MessageListProps) {
  const stageRef = useRef<HTMLDivElement>(null);
  const transcriptRef = useRef<HTMLDivElement>(null);
  const turnRefs = useRef<(HTMLDivElement | null)[]>([]);
  const messageAnchorsRef = useRef<MessageAnchorsHandle>(null);
  const atBottomRef = useRef(true);
  const anchorGestureRef = useRef<AnchorGesture | null>(null);
  const anchorPreviewTimerRef = useRef<number | null>(null);
  const anchorPreviewVisibleRef = useRef(false);
  const [activeAnchorIndex, setActiveAnchorIndex] = useState(0);
  const [anchorOpen, setAnchorOpen] = useState(false);
  const toolResults = useMemo(() => {
    const values = new Map<string, AgentMessage>();
    for (const message of messages) {
      if (message.role !== "toolResult" || typeof message.toolCallId !== "string") continue;
      values.set(message.toolCallId, message);
    }
    return values;
  }, [messages]);
  const { turns, leading } = useMemo(() => {
    const nextTurns: ConversationTurn[] = [];
    const nextLeading: IndexedMessage[] = [];
    let current: ConversationTurn | null = null;
    messages.forEach((message, index) => {
      const indexed = { message, entryId: entryIds[index] };
      if (isGroupAnchor(message)) {
        current = { anchor: indexed, tail: [] };
        nextTurns.push(current);
      } else if (current) {
        current.tail.push(indexed);
      } else {
        nextLeading.push(indexed);
      }
    });
    return { turns: nextTurns, leading: nextLeading };
  }, [entryIds, messages]);
  const activeTurnIndex = turns.length - 1;
  const anchors = useMemo(() => turns.map((turn, index) => ({
    id: turn.anchor.entryId || String(turn.anchor.message.id ?? `turn-${index}`),
    label: messageText(turn.anchor.message).replace(/\s+/g, " ").trim(),
  })), [turns]);

  const syncActiveAnchor = () => {
    const transcript = transcriptRef.current;
    if (!transcript || turns.length === 0) return;
    atBottomRef.current = transcript.scrollHeight - transcript.scrollTop - transcript.clientHeight < 96;
    const focus = transcript.getBoundingClientRect().top + transcript.clientHeight * 0.28;
    let nearest = 0;
    let distance = Number.POSITIVE_INFINITY;
    turnRefs.current.forEach((element, index) => {
      if (!element) return;
      const nextDistance = Math.abs(element.getBoundingClientRect().top - focus);
      if (nextDistance < distance) {
        distance = nextDistance;
        nearest = index;
      }
    });
    setActiveAnchorIndex(nearest);
  };

  const scrollToAnchor = (index: number) => {
    const transcript = transcriptRef.current;
    const target = turnRefs.current[index];
    if (!transcript || !target) return;
    const top = target.getBoundingClientRect().top
      - transcript.getBoundingClientRect().top
      + transcript.scrollTop
      - 20;
    setActiveAnchorIndex(index);
    transcript.scrollTo({ top: Math.max(0, top), behavior: "smooth" });
  };

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
    transcript.scrollTop = transcript.scrollHeight;
    atBottomRef.current = true;
    anchorGestureRef.current = null;
    messageAnchorsRef.current?.setGestureIndex(null);
    clearAnchorPreviewTimer();
    anchorPreviewVisibleRef.current = false;
    messageAnchorsRef.current?.setPreviewIndex(null);
    messageAnchorsRef.current?.setPreviewPosition(null);
    setAnchorOpen(false);
    setActiveAnchorIndex(Math.max(0, turns.length - 1));
    const frame = requestAnimationFrame(() => {
      transcript.scrollTop = transcript.scrollHeight;
    });
    return () => cancelAnimationFrame(frame);
  }, [sessionId]);

  useLayoutEffect(() => {
    const transcript = transcriptRef.current;
    if (!transcript || !atBottomRef.current) return;
    transcript.scrollTop = transcript.scrollHeight;
  }, [messages.length, pendingMessages.length, streamingMessage, busy]);

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

  useEffect(() => {
    const transcript = transcriptRef.current;
    if (!transcript) return;
    const onScroll = () => syncActiveAnchor();
    const onResize = () => {
      if (atBottomRef.current) transcript.scrollTop = transcript.scrollHeight;
      syncActiveAnchor();
    };
    transcript.addEventListener("scroll", onScroll, { passive: true });
    const observer = new ResizeObserver(onResize);
    observer.observe(transcript);
    if (transcript.firstElementChild) observer.observe(transcript.firstElementChild);
    onScroll();
    return () => {
      transcript.removeEventListener("scroll", onScroll);
      observer.disconnect();
    };
  }, [turns.length]);

  return (
    <div
      ref={stageRef}
      className="pi-message-stage"
      onPointerDownCapture={onAnchorPointerDown}
      onPointerMoveCapture={onAnchorPointerMove}
      onPointerUpCapture={(event) => finishAnchorGesture(event, false)}
      onPointerCancelCapture={(event) => finishAnchorGesture(event, true)}
    >
      <div className="pi-transcript-scroll pi-overlay-scroll-host">
        <div ref={transcriptRef} className="pi-transcript pi-overlay-scroll-viewport" aria-live="polite">
          <div className="pi-transcript-inner">
            {leading.map(({ message, entryId }, index) => (
              <Message
                key={String(message.id ?? `leading-${message.role}-${index}`)}
                message={message}
                toolResults={toolResults}
                entryId={entryId}
                onFork={onFork}
              />
            ))}
            {turns.map((turn, index) => (
              <div
                className="pi-turn-anchor"
                key={String(turn.anchor.message.id ?? `turn-${index}`)}
                ref={(element) => {
                  turnRefs.current[index] = element;
                }}
              >
                <Turn
                  turn={turn}
                  toolResults={toolResults}
                  streamingMessage={index === activeTurnIndex ? streamingMessage : null}
                  busy={index === activeTurnIndex && busy}
                  active={index === activeTurnIndex}
                  onFork={onFork}
                />
              </div>
            ))}
            {turns.length === 0 && streamingMessage && (
              <Message message={streamingMessage} toolResults={toolResults} onFork={onFork} streaming />
            )}
            {turns.length === 0 && busy && !streamingMessage && (
              <div className="pi-working" role="status">
                <span />
                <span />
                <span />
              </div>
            )}
            {pendingMessages.map((message, index) => (
              <Message
                key={String(message.id ?? `pending-${index}`)}
                message={message}
                toolResults={toolResults}
                onFork={onFork}
              />
            ))}
          </div>
        </div>
        <OverlayScrollbar viewportRef={transcriptRef} />
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
