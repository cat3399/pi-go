import { KeyboardEvent, useEffect, useRef, useState } from "react";
import { ArrowUp, Plus, Square, X } from "lucide-react";
import type {
  ContextUsage,
  ImageAttachment,
  ModelsView,
  SelectedModel,
} from "../contracts";
import { ContextUsageIndicator } from "../primitives/ContextUsageIndicator";
import { SelectMenu } from "../primitives/SelectMenu";
import type { SendBehavior } from "./useApplicationController";

interface ComposerProps {
  centered: boolean;
  active: boolean;
  models: ModelsView | null;
  model: SelectedModel | null;
  thinkingLevel: string;
  contextUsage: ContextUsage | null;
  busy: boolean;
  onSend(text: string, behavior?: SendBehavior, images?: ImageAttachment[]): Promise<void>;
  onAbort(): Promise<void>;
  onModelChange(model: SelectedModel): Promise<void>;
  onThinkingLevelChange(level: string): Promise<void>;
}

const MAX_ATTACHED_IMAGE_BYTES = 10 * 1024 * 1024;
const MAX_ATTACHED_IMAGES = 10;

function thinkingLevelLabel(level: string): string {
  return level === "max" ? "最高" : level;
}

export function Composer(props: ComposerProps) {
  const [text, setText] = useState("");
  const [images, setImages] = useState<ImageAttachment[]>([]);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const imagesRef = useRef(images);
  imagesRef.current = images;

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
    if (!value) return;
    setText("");
    try {
      await props.onSend(value, props.busy ? "steer" : "prompt", images);
      for (const image of images) {
        if (image.previewUrl.startsWith("blob:")) URL.revokeObjectURL(image.previewUrl);
      }
      setImages([]);
    } catch {
      setText(value);
    }
  };

  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
      event.preventDefault();
      void send();
    }
  };

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
      <div className="pi-composer">
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
        <textarea
          ref={textareaRef}
          rows={1}
          value={text}
          placeholder="随心输入"
          aria-label="消息"
          onChange={(event) => setText(event.target.value)}
          onKeyDown={onKeyDown}
        />
        <div className="pi-composer-toolbar">
          <div className="pi-composer-left">
            <button
              className="pi-attach-button"
              type="button"
              aria-label="添加图片"
              disabled={props.busy || images.length >= MAX_ATTACHED_IMAGES}
              onClick={() => fileInputRef.current?.click()}
            >
              <Plus size={18} />
            </button>
          </div>
          <div className="pi-composer-meta">
            <ContextUsageIndicator usage={props.contextUsage} />
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
            {props.busy ? (
              <button
                className="pi-stop-button"
                type="button"
                aria-label="停止"
                onClick={() => void props.onAbort()}
              >
                <Square size={11} fill="currentColor" />
              </button>
            ) : (
              <button
                className="pi-send-button"
                type="button"
                aria-label="发送"
                disabled={!text.trim()}
                onClick={() => void send()}
              >
                <ArrowUp size={18} strokeWidth={2.1} />
              </button>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
