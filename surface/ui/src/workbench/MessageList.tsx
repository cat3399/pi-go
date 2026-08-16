import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { BookOpen, Check, ChevronRight, Copy, FilePenLine, GitFork, Search, SquareTerminal } from "lucide-react";
import type { AgentMessage, MessageContentBlock } from "../contracts";
import { MarkdownBody } from "../content/MarkdownBody";
import { OverlayScrollbar } from "../primitives/OverlayScrollbar";
import { messageText, visibleMessage } from "./message";

interface MessageListProps {
  messages: AgentMessage[];
  entryIds: string[];
  streamingMessage: AgentMessage | null;
  busy: boolean;
  onFork(entryId: string): Promise<void>;
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

function toolPresentation(name: string, input: Record<string, unknown>, complete: boolean) {
  switch (name) {
    case "read":
      return { icon: BookOpen, verb: complete ? "已读取" : "正在读取", target: inputString(input, "path"), card: "文件" };
    case "write":
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

function EditPreview({ input }: { input: Record<string, unknown> }) {
  const edits = Array.isArray(input.edits) ? input.edits : [];
  if (edits.length === 0) return null;
  return (
    <div className="pi-edit-preview">
      {edits.map((value, index) => {
        if (!value || typeof value !== "object" || Array.isArray(value)) return null;
        const edit = value as Record<string, unknown>;
        const oldText = inputString(edit, "oldText");
        const newText = inputString(edit, "newText");
        return (
          <div key={index}>
            {oldText && <pre className="is-removed">{oldText}</pre>}
            {newText && <pre className="is-added">{newText}</pre>}
          </div>
        );
      })}
    </div>
  );
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
    await navigator.clipboard.writeText(props.value);
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
  const [expanded, setExpanded] = useState(false);
  const name = toolCallName(block);
  const input = toolInputRecord(block);
  const output = resultText(result);
  const failed = result?.isError === true;
  const complete = Boolean(result);
  const presentation = toolPresentation(name, input, complete);
  const Icon = presentation.icon;
  const serializedInput = JSON.stringify(input, null, 2);
  const command = inputString(input, "command");
  const inputCopyValue = name === "bash" ? command : serializedInput;
  const hasEditPreview = (name === "edit" || name === "write")
    && Array.isArray(input.edits)
    && input.edits.length > 0;

  return (
    <div className={`pi-tool ${failed ? "is-error" : ""} ${expanded ? "is-open" : ""}`}>
      <button className="pi-tool-summary" type="button" aria-expanded={expanded} onClick={() => setExpanded((value) => !value)}>
        <Icon size={15} />
        <span>{failed ? "调用失败" : presentation.verb}</span>
        {presentation.target && <code>{presentation.target}</code>}
        <ChevronRight className="pi-disclosure" size={14} />
      </button>
      {expanded && (
        <div className="pi-tool-body">
          <div className="pi-tool-card-header">
            <span>{presentation.card}</span>
            <span className="pi-tool-card-status">
              {result && !failed && <Check size={12} />}
              {streaming && !result ? "运行中" : failed ? "失败" : result ? "成功" : "等待"}
            </span>
          </div>
          <ToolDataSection kind="input" value={inputCopyValue}>
            {hasEditPreview
              ? <EditPreview input={input} />
              : <pre className="pi-tool-input">{name === "bash" ? `$ ${command}` : serializedInput}</pre>}
          </ToolDataSection>
          {output && (
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
    await navigator.clipboard.writeText(text);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1500);
  };

  return (
    <article className={`pi-message pi-message-${role} ${streaming ? "is-streaming" : ""} ${process ? "is-process" : ""}`}>
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

export function MessageList({ messages, entryIds, streamingMessage, busy, onFork }: MessageListProps) {
  const transcriptRef = useRef<HTMLDivElement>(null);
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

  return (
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
            <Turn
              key={String(turn.anchor.message.id ?? `turn-${index}`)}
              turn={turn}
              toolResults={toolResults}
              streamingMessage={index === activeTurnIndex ? streamingMessage : null}
              busy={index === activeTurnIndex && busy}
              active={index === activeTurnIndex}
              onFork={onFork}
            />
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
        </div>
      </div>
      <OverlayScrollbar viewportRef={transcriptRef} />
    </div>
  );
}
