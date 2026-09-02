import { forwardRef, KeyboardEvent, useEffect, useId, useImperativeHandle, useMemo, useRef, useState } from "react";
import { FileText, FileUp, Folder, ImagePlus, LoaderCircle, Plus, Square, X } from "lucide-react";
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
  UploadConflictStrategy,
  UploadResult,
  UploadTargetInspection,
} from "../contracts";
import { AnchoredPopover } from "../primitives/AnchoredPopover";
import { ContextUsageIndicator } from "../primitives/ContextUsageIndicator";
import { ImagePreview } from "../primitives/ImagePreview";
import { SelectMenu } from "../primitives/SelectMenu";
import { validateUploadFiles } from "../upload-files";
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
import { ModelConfigControl, thinkingLevelLabel } from "./ModelConfigControl";
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
  latestTokensPerSecond: number | null;
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
  onInspectUploadTargets(directory: string, fileNames: string[]): Promise<UploadTargetInspection>;
  onUploadFiles(directory: string, files: File[], strategy: UploadConflictStrategy): Promise<UploadResult>;
  onPreviewFile(path: string): void;
  onFilesUploaded(): void;
}

export interface ComposerHandle {
  insertText(value: string): void;
  setDraft(value: string): void;
  removeWorkspaceFile(path: string): void;
  focus(): void;
}

const MAX_ATTACHED_IMAGE_BYTES = 10 * 1024 * 1024;
const MAX_ATTACHED_IMAGES = 10;

type UploadPhase = "idle" | "checking" | "uploading";

interface UploadedWorkspaceFile {
  name: string;
  path: string;
}

interface PendingUploadConflict {
  directory: string;
  files: File[];
  conflicts: string[];
  nonReplaceable: string[];
}

function projectName(path: string): string {
  const parts = path.split(/[\\/]/).filter(Boolean);
  return parts[parts.length - 1] || path;
}

function joinFilePath(directory: string, name: string): string {
  const separator = directory.includes("\\") && !directory.includes("/") ? "\\" : "/";
  return `${directory.replace(/[\\/]+$/, "")}${separator}${name}`;
}

function normalizedFilePath(path: string): string {
  return path.replace(/\\/g, "/").replace(/\/+$/, "");
}

function fileMention(name: string): string {
  return name.includes(" ") ? `@"${name}"` : `@${name}`;
}

const rootSlashSources: SlashCommandPaletteSource[] = ["builtin", "extension", "prompt", "skill"];

export const Composer = forwardRef<ComposerHandle, ComposerProps>(function Composer(props, ref) {
  const [text, setText] = useState("");
  const [images, setImages] = useState<ImageAttachment[]>([]);
  const [uploadedFiles, setUploadedFiles] = useState<UploadedWorkspaceFile[]>([]);
  const [previewImage, setPreviewImage] = useState<{ src: string; alt: string } | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [attachmentMenuOpen, setAttachmentMenuOpen] = useState(false);
  const [uploadPhase, setUploadPhase] = useState<UploadPhase>("idle");
  const [uploadError, setUploadError] = useState("");
  const [uploadResult, setUploadResult] = useState<UploadResult | null>(null);
  const [pendingUploadConflict, setPendingUploadConflict] = useState<PendingUploadConflict | null>(null);
  const [slashActiveIndex, setSlashActiveIndex] = useState(0);
  const [slashDismissed, setSlashDismissed] = useState(false);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const imageInputRef = useRef<HTMLInputElement>(null);
  const uploadInputRef = useRef<HTMLInputElement>(null);
  const attachmentButtonRef = useRef<HTMLButtonElement>(null);
  const slashItemRefs = useRef<(HTMLButtonElement | null)[]>([]);
  const slashMenuId = useId();
  const attachmentMenuId = useId();
  const imageInputId = useId();
  const uploadInputId = useId();
  const imagesRef = useRef(images);
  const uploadGenerationRef = useRef(0);
  const composingRef = useRef(false);
  const compositionEndedAtRef = useRef(0);
  imagesRef.current = images;
  const uploadBusy = uploadPhase !== "idle";

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
    removeWorkspaceFile(path) {
      const deleted = normalizedFilePath(path);
      setUploadedFiles((current) => current.filter((file) => {
        const attached = normalizedFilePath(file.path);
        return attached !== deleted && !attached.startsWith(`${deleted}/`);
      }));
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
    uploadGenerationRef.current += 1;
    setUploadedFiles([]);
    setPendingUploadConflict(null);
    setUploadResult(null);
    setUploadError("");
    setUploadPhase("idle");
  }, [props.workingDirectory]);

  useEffect(() => {
    if (props.busy || uploadBusy) setAttachmentMenuOpen(false);
  }, [props.busy, uploadBusy]);

  useEffect(() => {
    const element = textareaRef.current;
    if (!element) return;
    element.style.height = "0";
    element.style.height = `${Math.min(element.scrollHeight, 180)}px`;
  }, [text]);

  const send = async () => {
    const value = text.trim();
    if (!value || submitting || (props.busy && images.length > 0)) return;
    const mentions = uploadedFiles.map((file) => fileMention(file.name)).join(" ");
    const prompt = mentions ? `${mentions}\n\n${value}` : value;
    setSubmitting(true);
    setText("");
    try {
      await props.onSend(prompt, props.busy ? props.streamingInputBehavior : "prompt", images);
      setPreviewImage(null);
      for (const image of images) {
        if (image.previewUrl.startsWith("blob:")) URL.revokeObjectURL(image.previewUrl);
      }
      setImages([]);
      setUploadedFiles([]);
      setUploadResult(null);
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
    const removed = imagesRef.current[index];
    if (!removed) return;
    if (removed.previewUrl === previewImage?.src) setPreviewImage(null);
    if (removed.previewUrl.startsWith("blob:")) URL.revokeObjectURL(removed.previewUrl);
    setImages((current) => current.filter((_, currentIndex) => currentIndex !== index));
  };

  const performUpload = async (
    pending: PendingUploadConflict,
    strategy: UploadConflictStrategy,
  ) => {
    const generation = uploadGenerationRef.current;
    setPendingUploadConflict(null);
    setUploadError("");
    setUploadResult(null);
    setUploadPhase("uploading");
    try {
      const result = await props.onUploadFiles(pending.directory, pending.files, strategy);
      if (generation !== uploadGenerationRef.current) return;
      if (result.conflicts?.length) {
        setPendingUploadConflict({
          ...pending,
          conflicts: result.conflicts,
          nonReplaceable: result.nonReplaceable ?? [],
        });
        return;
      }
      setUploadResult(result);
      if (result.uploaded.length > 0) {
        const next = result.uploaded.map((name) => ({
          name,
          path: joinFilePath(pending.directory, name),
        }));
        setUploadedFiles((current) => {
          const byPath = new Map(current.map((file) => [file.path, file]));
          for (const file of next) byPath.set(file.path, file);
          return [...byPath.values()];
        });
        props.onFilesUploaded();
      }
    } catch (error) {
      if (generation === uploadGenerationRef.current) {
        setUploadError(error instanceof Error ? error.message : String(error));
      }
    } finally {
      if (generation === uploadGenerationRef.current) setUploadPhase("idle");
    }
  };

  const prepareUpload = async (files: File[]) => {
    if (props.busy || uploadBusy || files.length === 0) return;
    const directory = props.workingDirectory.trim();
    const generation = uploadGenerationRef.current;
    setAttachmentMenuOpen(false);
    setPendingUploadConflict(null);
    setUploadResult(null);
    setUploadError("");
    try {
      if (!directory) throw new Error("请先选择工作区");
      validateUploadFiles(files);
      setUploadPhase("checking");
      const inspection = await props.onInspectUploadTargets(
        directory,
        files.map((file) => file.name),
      );
      if (generation !== uploadGenerationRef.current) return;
      const pending = {
        directory,
        files,
        conflicts: inspection.conflicts,
        nonReplaceable: inspection.nonReplaceable,
      };
      if (inspection.conflicts.length > 0) {
        setPendingUploadConflict(pending);
        return;
      }
      await performUpload(pending, "error");
    } catch (error) {
      if (generation === uploadGenerationRef.current) {
        setUploadError(error instanceof Error ? error.message : String(error));
      }
    } finally {
      if (generation === uploadGenerationRef.current) setUploadPhase("idle");
    }
  };

  const removeUploadedFile = (path: string) => {
    setUploadedFiles((current) => current.filter((file) => file.path !== path));
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
        className={!props.active ? "has-project" : ""}
        leading={(
          <>
            <input
              id={imageInputId}
              ref={imageInputRef}
              type="file"
              accept="image/*"
              multiple
              disabled={props.busy}
              hidden
              onChange={(event) => {
                setAttachmentMenuOpen(false);
                void addImages(Array.from(event.target.files ?? []));
                event.target.value = "";
              }}
            />
            <input
              id={uploadInputId}
              ref={uploadInputRef}
              type="file"
              multiple
              disabled={props.busy || uploadBusy}
              hidden
              onChange={(event) => {
                setAttachmentMenuOpen(false);
                void prepareUpload(Array.from(event.target.files ?? []));
                event.target.value = "";
              }}
            />
            {Boolean(uploadBusy || pendingUploadConflict || uploadError || uploadResult?.skipped.length || uploadResult?.errors.length) && (
              <div className="pi-composer-upload-feedback">
                {uploadBusy && (
                  <div className="pi-composer-upload-status" role="status">
                    <LoaderCircle size={14} />
                    <span>{uploadPhase === "checking" ? "正在检查文件…" : "正在上传文件…"}</span>
                  </div>
                )}
                {pendingUploadConflict && (
                  <div className="pi-composer-upload-conflict" role="alert">
                    <p>工作区中已有同名文件：{pendingUploadConflict.conflicts.join("、")}</p>
                    {pendingUploadConflict.nonReplaceable.length > 0 && (
                      <small>目录或符号链接不能被替换：{pendingUploadConflict.nonReplaceable.join("、")}</small>
                    )}
                    <div>
                      <button type="button" onClick={() => void performUpload(pendingUploadConflict, "overwrite")}>
                        替换文件
                      </button>
                      <button type="button" onClick={() => void performUpload(pendingUploadConflict, "skip")}>
                        跳过同名文件
                      </button>
                      <button type="button" onClick={() => setPendingUploadConflict(null)}>取消</button>
                    </div>
                  </div>
                )}
                {uploadError && (
                  <div className="pi-composer-upload-error" role="alert">
                    <span>{uploadError}</span>
                    <button type="button" aria-label="关闭上传错误" onClick={() => setUploadError("")}>
                      <X size={12} />
                    </button>
                  </div>
                )}
                {uploadResult && (uploadResult.skipped.length > 0 || uploadResult.errors.length > 0) && (
                  <div className="pi-composer-upload-result" role="status">
                    {uploadResult.skipped.length > 0 && <span>已跳过：{uploadResult.skipped.join("、")}</span>}
                    {uploadResult.errors.map((item) => (
                      <span className="is-error" key={item.name}>{item.name}：{item.error}</span>
                    ))}
                    <button type="button" aria-label="关闭上传结果" onClick={() => setUploadResult(null)}>
                      <X size={12} />
                    </button>
                  </div>
                )}
              </div>
            )}
            {images.length > 0 && (
              <div className="pi-composer-images">
                {images.map((image, index) => (
                  <div key={`${image.previewUrl}-${index}`}>
                    <button
                      className="pi-composer-image-preview"
                      type="button"
                      aria-label={`预览附件图片 ${index + 1}`}
                      title="预览图片"
                      onClick={() => setPreviewImage({ src: image.previewUrl, alt: `附件图片 ${index + 1}` })}
                    >
                      <img src={image.previewUrl} alt="" />
                    </button>
                    <button
                      className="pi-composer-image-remove"
                      type="button"
                      aria-label="移除图片"
                      onClick={() => removeImage(index)}
                    >
                      <X size={10} />
                    </button>
                  </div>
                ))}
              </div>
            )}
            {uploadedFiles.length > 0 && (
              <div className="pi-composer-files">
                {uploadedFiles.map((file) => (
                  <div className="pi-composer-file" key={file.path}>
                    <button
                      className="pi-composer-file-preview"
                      type="button"
                      title={`预览 ${file.name}`}
                      onClick={() => props.onPreviewFile(file.path)}
                    >
                      <FileText size={15} />
                      <span>{file.name}</span>
                    </button>
                    <button
                      className="pi-composer-file-remove"
                      type="button"
                      aria-label={`从本次对话移除 ${file.name}`}
                      title="仅从本次对话移除，工作区文件会保留"
                      onClick={() => removeUploadedFile(file.path)}
                    >
                      <X size={11} />
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
              ref={attachmentButtonRef}
              className="pi-attach-button"
              type="button"
              aria-label="添加附件"
              aria-haspopup="menu"
              aria-expanded={attachmentMenuOpen}
              aria-controls={attachmentMenuOpen ? attachmentMenuId : undefined}
              disabled={props.busy || uploadBusy}
              onClick={() => setAttachmentMenuOpen((open) => !open)}
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
            {props.active && (
              <ContextUsageIndicator
                usage={props.contextUsage}
                latestUsage={props.latestUsage}
                latestTokensPerSecond={props.latestTokensPerSecond}
                sessionStats={props.sessionStats}
              />
            )}
            <ModelConfigControl
              active={props.active}
              models={props.models}
              model={props.model}
              thinkingLevel={props.thinkingLevel}
              disabled={props.busy}
              onModelChange={props.onModelChange}
              onThinkingLevelChange={props.onThinkingLevelChange}
            />
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
      <AnchoredPopover
        anchorRef={attachmentButtonRef}
        open={attachmentMenuOpen}
        id={attachmentMenuId}
        className="pi-attachment-menu"
        role="menu"
        align="start"
        placement="above"
        minWidth={210}
        onDismiss={() => setAttachmentMenuOpen(false)}
      >
        <label
          htmlFor={images.length >= MAX_ATTACHED_IMAGES ? undefined : imageInputId}
          role="menuitem"
          tabIndex={images.length >= MAX_ATTACHED_IMAGES ? -1 : 0}
          aria-disabled={images.length >= MAX_ATTACHED_IMAGES}
          onKeyDown={(event) => {
            if (images.length >= MAX_ATTACHED_IMAGES || (event.key !== "Enter" && event.key !== " ")) return;
            event.preventDefault();
            imageInputRef.current?.click();
          }}
        >
          <ImagePlus size={17} />
          <span><strong>添加图片</strong><small>作为图片附件发送给模型</small></span>
        </label>
        <label
          htmlFor={!props.workingDirectory.trim() ? undefined : uploadInputId}
          role="menuitem"
          tabIndex={!props.workingDirectory.trim() ? -1 : 0}
          aria-disabled={!props.workingDirectory.trim()}
          onKeyDown={(event) => {
            if (!props.workingDirectory.trim() || (event.key !== "Enter" && event.key !== " ")) return;
            event.preventDefault();
            uploadInputRef.current?.click();
          }}
        >
          <FileUp size={17} />
          <span><strong>上传文件</strong><small>保存到当前工作区并添加到对话</small></span>
        </label>
      </AnchoredPopover>
      {previewImage && (
        <ImagePreview
          src={previewImage.src}
          alt={previewImage.alt}
          onClose={() => setPreviewImage(null)}
        />
      )}
    </div>
  );
});
