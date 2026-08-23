import { forwardRef, KeyboardEvent, useEffect, useId, useImperativeHandle, useMemo, useRef, useState } from "react";
import { Folder, Plus, Square, X } from "lucide-react";
import type {
  ContextUsage,
  ImageAttachment,
  ModelsView,
  ProjectInfo,
  SelectedModel,
  SessionInfo,
  SessionStatsInfo,
  SlashCommandInfo,
  TokenUsageInfo,
} from "../contracts";
import { ContextUsageIndicator } from "../primitives/ContextUsageIndicator";
import { SelectMenu } from "../primitives/SelectMenu";
import {
  matchSlashCommands,
  slashQuery,
  SLASH_SOURCE_LABELS,
  type SlashCommandPaletteItem,
  type SlashCommandArgumentItem,
  type SlashCommandPaletteSource,
} from "./slash-commands";
import type { ToolPreset } from "../tool-presets";
import type { StreamingInputBehavior } from "../streaming-input-behavior";
import { ComposerInput, ComposerSendButton } from "./ComposerInput";
import type { SendBehavior } from "./useApplicationController";

interface ComposerProps {
  centered: boolean;
  active: boolean;
  mobile: boolean;
  models: ModelsView | null;
  model: SelectedModel | null;
  thinkingLevel: string;
  contextUsage: ContextUsage | null;
  latestUsage: TokenUsageInfo | null;
  sessionStats: SessionStatsInfo | null;
  busy: boolean;
  streamingInputBehavior: StreamingInputBehavior;
  sessions: SessionInfo[];
  projects: ProjectInfo[];
  workingDirectory: string;
  toolPreset: ToolPreset;
  slashCommands: SlashCommandInfo[];
  onSend(text: string, behavior?: SendBehavior, images?: ImageAttachment[]): Promise<void>;
  onAbort(): Promise<void>;
  onModelChange(model: SelectedModel): Promise<void>;
  onThinkingLevelChange(level: string): Promise<void>;
  onProjectChange(path: string): Promise<void>;
}

export interface ComposerHandle {
  insertText(value: string): void;
  setDraft(value: string): void;
  focus(): void;
}

const MAX_ATTACHED_IMAGE_BYTES = 10 * 1024 * 1024;
const MAX_ATTACHED_IMAGES = 10;

function thinkingLevelLabel(level: string): string {
  return level === "max" ? "最高" : level;
}

function projectName(path: string): string {
  const parts = path.split(/[\\/]/).filter(Boolean);
  return parts[parts.length - 1] || path;
}

const rootSlashSources: SlashCommandPaletteSource[] = ["builtin", "extension", "prompt", "skill"];

export const Composer = forwardRef<ComposerHandle, ComposerProps>(function Composer(props, ref) {
  const [text, setText] = useState("");
  const [images, setImages] = useState<ImageAttachment[]>([]);
  const [submitting, setSubmitting] = useState(false);
  const [slashActiveIndex, setSlashActiveIndex] = useState(0);
  const [slashDismissed, setSlashDismissed] = useState(false);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const slashItemRefs = useRef<(HTMLButtonElement | null)[]>([]);
  const slashMenuId = useId();
  const imagesRef = useRef(images);
  const composingRef = useRef(false);
  const compositionEndedAtRef = useRef(0);
  imagesRef.current = images;

  const selectedModelKey = props.model
    ? `${props.model.provider}\u0000${props.model.modelId}`
    : "";
  const modelCapabilityKey = props.model
    ? `${props.model.provider}:${props.model.modelId}`
    : "";
  const thinkingLevels = [
    ...(!props.active ? ["auto"] : []),
    ...(props.models?.thinkingLevels[modelCapabilityKey] ?? []),
  ].filter((value, index, values) => values.indexOf(value) === index);
  if (props.thinkingLevel && !thinkingLevels.includes(props.thinkingLevel)) {
    thinkingLevels.unshift(props.thinkingLevel);
  }
  const modelNotice = props.models?.modelError || props.models?.modelScopeWarnings?.join("\n") || "";
  const modelOptions = props.models?.modelList.map((model) => ({
    value: `${model.provider}\u0000${model.id}`,
    label: model.name || model.id,
  })) ?? [];
  const thinkingOptions = thinkingLevels.map((level) => ({
    value: level,
    label: thinkingLevelLabel(level),
  }));
  const projectOptions = (props.projects ?? []).map((project) => ({
    value: project.path,
    label: projectName(project.path),
  }));

  const query = useMemo(() => slashQuery(text), [text]);
  const argumentCommands = useMemo<SlashCommandArgumentItem[]>(() => {
    if (!query?.argumentMode) return [];
    switch (query.command) {
      case "model":
        return (props.models?.modelList ?? []).map((model) => ({
          name: `${model.provider}/${model.id}`,
          description: model.name || model.id,
          source: "argument",
        }));
      case "thinking":
        return thinkingLevels.map((level) => ({
          name: level,
          description: thinkingLevelLabel(level),
          source: "argument",
        }));
      case "resume":
        return props.sessions.map((session) => ({
          name: session.id,
          description: session.name?.trim() || session.firstMessage?.trim() || session.cwd,
          source: "argument",
        }));
      case "tools":
        return [
          { name: "default", description: "读取、终端、编辑与写入", source: "argument" as const },
          { name: "full", description: "启用全部内置文件工具", source: "argument" as const },
          { name: "none", description: "关闭内置工具", source: "argument" as const },
        ]
          .sort((left, right) => Number(right.name === props.toolPreset) - Number(left.name === props.toolPreset))
          .map((item) => ({
            ...item,
            description: `${item.description}${item.name === props.toolPreset ? " · 当前" : ""}`,
          }));
      default:
        return [];
    }
  }, [props.models?.modelList, props.sessions, props.toolPreset, query, thinkingLevels]);
  const argumentCommand = query?.argumentMode
    && (query.command === "model" || query.command === "thinking" || query.command === "resume" || query.command === "tools");
  const slashCommands = useMemo(
    () => query === null
      ? []
      : matchSlashCommands(
          query.argumentMode ? argumentCommands : props.slashCommands,
          query.query,
          !query.argumentMode,
        ),
    [argumentCommands, props.slashCommands, query],
  );
  const slashMenuOpen = query !== null && !slashDismissed && (!query.argumentMode || argumentCommand);
  const slashSources = query?.argumentMode ? ["argument" as const] : rootSlashSources;
  const slashGroups = useMemo(() => slashSources
    .map((source) => ({
      source,
      commands: slashCommands.filter((command) => command.source === source),
    }))
    .filter((group) => group.commands.length > 0), [slashCommands]);
  const orderedSlashCommands = useMemo(
    () => slashGroups.flatMap((group) => group.commands),
    [slashGroups],
  );
  useEffect(() => {
    setSlashActiveIndex(0);
    setSlashDismissed(false);
  }, [query]);

  useEffect(() => {
    if (!slashMenuOpen) return;
    slashItemRefs.current[slashActiveIndex]?.scrollIntoView({ block: "nearest" });
  }, [slashActiveIndex, slashMenuOpen]);

  const insertText = (value: string) => {
    const element = textareaRef.current;
    const start = element?.selectionStart ?? text.length;
    const end = element?.selectionEnd ?? start;
    const before = text.slice(0, start);
    const prefix = before && !/\s$/.test(before) && value && !/^\s/.test(value) ? " " : "";
    const insertion = `${prefix}${value}`;
    const next = `${before}${insertion}${text.slice(end)}`;
    const cursor = start + insertion.length;
    setText(next);
    setSlashDismissed(false);
    requestAnimationFrame(() => {
      const textarea = textareaRef.current;
      if (!textarea) return;
      textarea.focus();
      textarea.setSelectionRange(cursor, cursor);
    });
  };

  useImperativeHandle(ref, () => ({
    insertText,
    setDraft(value) {
      setText(value);
      setSlashDismissed(false);
      requestAnimationFrame(() => {
        textareaRef.current?.focus();
        textareaRef.current?.setSelectionRange(value.length, value.length);
      });
    },
    focus() {
      textareaRef.current?.focus();
    },
  }));

  useEffect(() => () => {
    for (const image of imagesRef.current) {
      if (image.previewUrl.startsWith("blob:")) URL.revokeObjectURL(image.previewUrl);
    }
  }, []);

  useEffect(() => {
    const element = textareaRef.current;
    if (!element) return;
    element.style.height = "0";
    element.style.height = `${Math.min(element.scrollHeight, 180)}px`;
  }, [text]);

  const send = async () => {
    const value = text.trim();
    if (!value || submitting || (props.busy && images.length > 0)) return;
    setSubmitting(true);
    setText("");
    try {
      await props.onSend(value, props.busy ? props.streamingInputBehavior : "prompt", images);
      for (const image of images) {
        if (image.previewUrl.startsWith("blob:")) URL.revokeObjectURL(image.previewUrl);
      }
      setImages([]);
    } catch {
      setText(value);
    } finally {
      setSubmitting(false);
    }
  };

  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    const composing = composingRef.current
      || event.nativeEvent.isComposing
      || event.keyCode === 229
      || Date.now() - compositionEndedAtRef.current < 80;
    if (composing) return;

    if (slashMenuOpen) {
      if (event.key === "ArrowDown") {
        event.preventDefault();
        setSlashActiveIndex((index) => Math.min(orderedSlashCommands.length - 1, index + 1));
        return;
      }
      if (event.key === "ArrowUp") {
        event.preventDefault();
        setSlashActiveIndex((index) => Math.max(0, index - 1));
        return;
      }
      if (event.key === "Escape") {
        event.preventDefault();
        setSlashDismissed(true);
        return;
      }
      const selected = orderedSlashCommands[slashActiveIndex];
      if (selected && (event.key === "Tab" || (!props.mobile && event.key === "Enter" && !event.shiftKey))) {
        event.preventDefault();
        applySlashCommand(selected);
        return;
      }
    }

    if (!props.mobile && event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      void send();
    }
  };

  const applySlashCommand = (command: SlashCommandPaletteItem | SlashCommandArgumentItem) => {
    const value = query?.argumentMode ? `${query.prefix}${command.name}` : `/${command.name} `;
    setText(value);
    setSlashDismissed(Boolean(query?.argumentMode));
    requestAnimationFrame(() => {
      textareaRef.current?.focus();
      textareaRef.current?.setSelectionRange(value.length, value.length);
    });
  };

  const addImages = async (files: File[]) => {
    if (props.busy) return;
    const accepted = files
      .filter((file) => file.type.startsWith("image/") && file.size <= MAX_ATTACHED_IMAGE_BYTES)
      .slice(0, Math.max(0, MAX_ATTACHED_IMAGES - imagesRef.current.length));
    const next = await Promise.all(accepted.map((file) => new Promise<ImageAttachment>((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => {
        const result = String(reader.result ?? "");
        const separator = result.indexOf(",");
        if (separator < 0) {
          reject(new Error("无法读取图片"));
          return;
        }
        resolve({
          data: result.slice(separator + 1),
          mimeType: file.type,
          previewUrl: URL.createObjectURL(file),
        });
      };
      reader.onerror = () => reject(reader.error ?? new Error("无法读取图片"));
      reader.readAsDataURL(file);
    })));
    setImages((current) => [...current, ...next].slice(0, MAX_ATTACHED_IMAGES));
  };

  const removeImage = (index: number) => {
    setImages((current) => {
      const next = [...current];
      const [removed] = next.splice(index, 1);
      if (removed?.previewUrl.startsWith("blob:")) URL.revokeObjectURL(removed.previewUrl);
      return next;
    });
  };

  return (
    <div className={`pi-composer-wrap ${props.centered ? "is-centered" : ""}`}>
      {modelNotice && (
        <div className="pi-model-notice" role="alert">
          {modelNotice}
        </div>
      )}
      {slashMenuOpen && (
        <div id={slashMenuId} className="pi-slash-menu" role="listbox" aria-label="斜杠命令">
          {slashCommands.length === 0 ? (
            <div className="pi-slash-empty">没有匹配的命令</div>
          ) : slashGroups.map((group) => (
            <section className="pi-slash-group" key={group.source}>
              <h2>{SLASH_SOURCE_LABELS[group.source]}</h2>
              {group.commands.map((command) => {
                const index = orderedSlashCommands.indexOf(command);
                return (
                  <button
                    id={`${slashMenuId}-option-${index}`}
                    key={`${command.source}:${command.name}`}
                    ref={(element) => {
                      slashItemRefs.current[index] = element;
                    }}
                    className={index === slashActiveIndex ? "is-active" : ""}
                    type="button"
                    role="option"
                    aria-selected={index === slashActiveIndex}
                    onPointerEnter={(event) => {
                      if (event.pointerType === "mouse") setSlashActiveIndex(index);
                    }}
                    onClick={() => applySlashCommand(command)}
                  >
                    <code>
                      {query?.argumentMode ? command.name : `/${command.name}`}
                      {!query?.argumentMode && "argumentHint" in command && command.argumentHint ? ` ${command.argumentHint}` : ""}
                    </code>
                    <span>{command.description || "运行命令"}</span>
                  </button>
                );
              })}
            </section>
          ))}
        </div>
      )}
      <ComposerInput
        ref={textareaRef}
        leading={(
          <>
            <input
              ref={fileInputRef}
              type="file"
              accept="image/*"
              multiple
              disabled={props.busy}
              hidden
              onChange={(event) => {
                void addImages(Array.from(event.target.files ?? []));
                event.target.value = "";
              }}
            />
            {images.length > 0 && (
              <div className="pi-composer-images">
                {images.map((image, index) => (
                  <div key={`${image.previewUrl}-${index}`}>
                    <img src={image.previewUrl} alt="" />
                    <button type="button" aria-label="移除图片" onClick={() => removeImage(index)}>
                      <X size={10} />
                    </button>
                  </div>
                ))}
              </div>
            )}
          </>
        )}
        rows={1}
        value={text}
        placeholder={props.busy
          ? (props.streamingInputBehavior === "steer" ? "输入消息以插入当前运行" : "输入消息以排到当前运行之后")
          : (props.mobile ? "输入消息，回车换行" : "输入消息 · / 命令 · Shift+Enter 换行")}
        aria-label="消息"
        aria-haspopup="listbox"
        aria-expanded={slashMenuOpen}
        aria-controls={slashMenuOpen ? slashMenuId : undefined}
        aria-activedescendant={slashMenuOpen && orderedSlashCommands[slashActiveIndex]
          ? `${slashMenuId}-option-${slashActiveIndex}`
          : undefined}
        enterKeyHint={props.mobile ? "enter" : "send"}
        onChange={(event) => {
          setText(event.target.value);
          setSlashDismissed(false);
        }}
        onKeyDown={onKeyDown}
        onCompositionStart={() => {
          composingRef.current = true;
        }}
        onCompositionEnd={() => {
          composingRef.current = false;
          compositionEndedAtRef.current = Date.now();
        }}
        toolbarLeft={(
          <>
            <button
              className="pi-attach-button"
              type="button"
              aria-label="添加图片"
              disabled={props.busy || images.length >= MAX_ATTACHED_IMAGES}
              onClick={() => fileInputRef.current?.click()}
            >
              <Plus size={18} />
            </button>
            {!props.active && (
              <SelectMenu
                ariaLabel="选择项目"
                value={props.workingDirectory}
                options={projectOptions}
                placeholder="选择项目"
                variant="project"
                leadingIcon={<Folder size={15} />}
                disabled={props.busy || projectOptions.length === 0}
                onChange={(value) => void props.onProjectChange(value)}
              />
            )}
          </>
        )}
        toolbarRight={(
          <>
            <ContextUsageIndicator
              usage={props.contextUsage}
              latestUsage={props.latestUsage}
              sessionStats={props.sessionStats}
            />
            {!props.busy && (
              <>
                <SelectMenu
                  ariaLabel="模型"
                  value={selectedModelKey}
                  options={modelOptions}
                  placeholder="未选择模型"
                  variant="model"
                  showChevron={false}
                  disabled={!props.models || props.models.modelList.length === 0}
                  onChange={(value) => {
                    const [provider, modelId] = value.split("\u0000");
                    if (provider && modelId) void props.onModelChange({ provider, modelId });
                  }}
                />
                <SelectMenu
                  ariaLabel="思考等级"
                  value={props.thinkingLevel}
                  options={thinkingOptions}
                  disabled={thinkingLevels.length === 0}
                  onChange={(value) => void props.onThinkingLevelChange(value)}
                />
              </>
            )}
            {props.busy && (!text.trim() || images.length > 0) ? (
              <button
                className="pi-stop-button"
                type="button"
                aria-label="停止"
                onClick={() => void props.onAbort()}
              >
                <Square size={11} fill="currentColor" />
              </button>
            ) : (
              <ComposerSendButton
                className={props.busy ? "is-queue" : ""}
                aria-label={props.busy
                  ? (props.streamingInputBehavior === "steer" ? "插入消息" : "稍后发送")
                  : "发送"}
                disabled={!text.trim() || submitting}
                submitting={submitting}
                onClick={() => void send()}
              />
            )}
          </>
        )}
      />
    </div>
  );
});
