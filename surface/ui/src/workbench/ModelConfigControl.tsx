import {
  type KeyboardEvent,
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
} from "react";
import { Check, ChevronDown, ChevronLeft, ChevronRight } from "lucide-react";
import type { ModelListItem, ModelsView, SelectedModel } from "../contracts";
import { AnchoredPopover } from "../primitives/AnchoredPopover";
import { useInputModality } from "../primitives/useInputModality";

interface ModelConfigControlProps {
  active: boolean;
  models: ModelsView | null;
  model: SelectedModel | null;
  thinkingLevel: string;
  disabled: boolean;
  onModelChange(model: SelectedModel): Promise<void>;
  onThinkingLevelChange(level: string): Promise<void>;
}

type ConfigPage = "root" | "models" | "thinking";

const thinkingLabels: Record<string, string> = {
  auto: "自动",
  off: "关闭",
  minimal: "极低",
  low: "低",
  medium: "中",
  high: "高",
  xhigh: "很高",
  max: "最高",
};

export function thinkingLevelLabel(level: string): string {
  const normalized = level.trim();
  return thinkingLabels[normalized] ?? (normalized || "未设置");
}

function modelKey(model: SelectedModel | null): string {
  return model ? `${model.provider}:${model.modelId}` : "";
}

function routeKey(model: Pick<ModelListItem, "provider" | "id">): string {
  return `${model.provider}\u0000${model.id}`;
}

function channelLabel(model: ModelListItem): string {
  return model.providerName?.trim() || model.provider;
}

export function ModelConfigControl(props: ModelConfigControlProps) {
  const [open, setOpen] = useState(false);
  const [page, setPage] = useState<ConfigPage>("root");
  const triggerRef = useRef<HTMLButtonElement>(null);
  const popoverRef = useRef<HTMLDivElement>(null);
  const {
    modality,
    modalityRef,
    markKeyboard,
    markPointer,
    suppressPointerFocus,
  } = useInputModality();
  const popoverId = useId();
  const selectedKey = props.model ? `${props.model.provider}\u0000${props.model.modelId}` : "";
  const capabilityKey = modelKey(props.model);
  const modelOptions = props.models?.modelList ?? [];
  const selectedModel = modelOptions.find((candidate) => routeKey(candidate) === selectedKey);
  const selectedModelName = selectedModel?.name?.trim()
    || props.models?.models[capabilityKey]
    || props.model?.modelId
    || "未选择模型";
  const thinkingLevels = [
    ...(!props.active ? ["auto"] : []),
    ...(props.models?.thinkingLevels[capabilityKey] ?? []),
  ].filter((value, index, values) => values.indexOf(value) === index);
  if (props.thinkingLevel && !thinkingLevels.includes(props.thinkingLevel)) {
    thinkingLevels.unshift(props.thinkingLevel);
  }
  const disabled = props.disabled || modelOptions.length === 0;
  const groups = useMemo(() => {
    const grouped = new Map<string, { id: string; label: string; models: ModelListItem[] }>();
    for (const candidate of modelOptions) {
      const current = grouped.get(candidate.provider) ?? {
        id: candidate.provider,
        label: channelLabel(candidate),
        models: [],
      };
      current.models.push(candidate);
      grouped.set(candidate.provider, current);
    }
    return [...grouped.values()].sort((left, right) => left.label.localeCompare(right.label));
  }, [modelOptions]);

  const close = useCallback((restoreFocus = false) => {
    setOpen(false);
    setPage("root");
    if (restoreFocus) requestAnimationFrame(() => triggerRef.current?.focus());
  }, []);

  const showPage = (nextPage: ConfigPage) => {
    setPage(nextPage);
  };

  useEffect(() => {
    if (!open || modality !== "keyboard") return;
    const frame = requestAnimationFrame(() => {
      const selected = popoverRef.current?.querySelector<HTMLElement>("[aria-current='true']");
      const first = popoverRef.current?.querySelector<HTMLElement>("[data-pi-menu-item]:not(:disabled)");
      (selected ?? first ?? popoverRef.current)?.focus({ preventScroll: true });
    });
    return () => cancelAnimationFrame(frame);
  }, [modality, open, page]);

  useEffect(() => {
    if (disabled) close();
  }, [close, disabled]);

  const onMenuKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    markKeyboard();
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      if (page === "root") close(true); else showPage("root");
      return;
    }
    if (event.key === "ArrowLeft" && page !== "root") {
      event.preventDefault();
      showPage("root");
      return;
    }
    if (event.key !== "ArrowDown" && event.key !== "ArrowUp" && event.key !== "Home" && event.key !== "End") {
      return;
    }
    const items = [...(popoverRef.current?.querySelectorAll<HTMLButtonElement>("[data-pi-menu-item]:not(:disabled)") ?? [])];
    if (items.length === 0) return;
    event.preventDefault();
    const currentIndex = items.indexOf(document.activeElement as HTMLButtonElement);
    let nextIndex = currentIndex;
    if (event.key === "Home") nextIndex = 0;
    if (event.key === "End") nextIndex = items.length - 1;
    if (event.key === "ArrowDown") nextIndex = (Math.max(0, currentIndex) + 1) % items.length;
    if (event.key === "ArrowUp") nextIndex = (currentIndex <= 0 ? items.length : currentIndex) - 1;
    items[nextIndex]?.focus({ preventScroll: true });
  };

  const fullLabel = `${selectedModelName}，推理强度${thinkingLevelLabel(props.thinkingLevel)}`;

  return (
    <>
      <button
        ref={triggerRef}
        className={`pi-model-config-trigger ${open ? "is-open" : ""}`}
        type="button"
        aria-label={`模型配置：${fullLabel}`}
        title={fullLabel}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? popoverId : undefined}
        disabled={disabled}
        onPointerDown={markPointer}
        onClick={() => {
          if (open) close();
          else {
            setPage("root");
            setOpen(true);
          }
        }}
        onKeyDown={(event) => {
          markKeyboard();
          if (event.key === "Tab" && open) {
            close();
            return;
          }
          if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
          event.preventDefault();
          if (!open) {
            setPage("root");
            setOpen(true);
            return;
          }
          const first = popoverRef.current?.querySelector<HTMLElement>("[data-pi-menu-item]:not(:disabled)");
          first?.focus({ preventScroll: true });
        }}
      >
        <span className="pi-model-config-summary">
          <span className="pi-model-config-model">{selectedModelName}</span>
          <span className="pi-model-config-thinking">{thinkingLevelLabel(props.thinkingLevel)}</span>
        </span>
        <ChevronDown className="pi-model-config-chevron" size={13} strokeWidth={1.7} />
      </button>
      <AnchoredPopover
        ref={popoverRef}
        anchorRef={triggerRef}
        open={open}
        id={popoverId}
        className={`pi-model-config-popover is-${page}`}
        role="menu"
        aria-label="模型配置"
        tabIndex={-1}
        data-focus-modality={modality}
        placement="above"
        minWidth={260}
        maxHeight={380}
        onDismiss={() => close()}
        onPointerDownCapture={markPointer}
        onMouseDownCapture={suppressPointerFocus}
        onKeyDown={onMenuKeyDown}
      >
        {page === "root" && (
          <div className="pi-model-config-root">
            <button data-pi-menu-item type="button" role="menuitem" onClick={() => showPage("models")}>
              <span>模型</span>
              <strong title={selectedModelName}>{selectedModelName}</strong>
              <ChevronRight size={16} />
            </button>
            <button
              data-pi-menu-item
              type="button"
              role="menuitem"
              disabled={thinkingLevels.length === 0}
              onClick={() => showPage("thinking")}
            >
              <span>推理强度</span>
              <strong>{thinkingLevelLabel(props.thinkingLevel)}</strong>
              <ChevronRight size={16} />
            </button>
          </div>
        )}
        {page === "models" && (
          <div className="pi-model-config-options">
            <button
              className="pi-model-config-back"
              data-pi-menu-item
              type="button"
              role="menuitem"
              onClick={() => showPage("root")}
            >
              <ChevronLeft size={15} />
              <span>模型</span>
            </button>
            {groups.map((group) => (
              <section key={group.id}>
                <h3>{group.label}</h3>
                {group.models.map((candidate) => {
                  const candidateKey = routeKey(candidate);
                  const selected = candidateKey === selectedKey;
                  return (
                    <button
                      data-pi-menu-item
                      type="button"
                      role="menuitemradio"
                      aria-checked={selected}
                      aria-current={selected ? "true" : undefined}
                      key={candidateKey}
                      title={`${channelLabel(candidate)} · ${candidate.name || candidate.id}`}
                      onClick={() => {
                        close(modalityRef.current === "keyboard");
                        if (!selected) {
                          void props.onModelChange({ provider: candidate.provider, modelId: candidate.id })
                            .catch(() => undefined);
                        }
                      }}
                    >
                      <span>{candidate.name || candidate.id}</span>
                      {selected && <Check size={14} />}
                    </button>
                  );
                })}
              </section>
            ))}
          </div>
        )}
        {page === "thinking" && (
          <div className="pi-model-config-options">
            <button
              className="pi-model-config-back"
              data-pi-menu-item
              type="button"
              role="menuitem"
              onClick={() => showPage("root")}
            >
              <ChevronLeft size={15} />
              <span>推理强度</span>
            </button>
            <section>
              {thinkingLevels.map((level) => {
                const selected = level === props.thinkingLevel;
                return (
                  <button
                    data-pi-menu-item
                    type="button"
                    role="menuitemradio"
                    aria-checked={selected}
                    aria-current={selected ? "true" : undefined}
                    key={level}
                    onClick={() => {
                      close(modalityRef.current === "keyboard");
                      if (!selected) void props.onThinkingLevelChange(level).catch(() => undefined);
                    }}
                  >
                    <span>{thinkingLevelLabel(level)}</span>
                    {selected && <Check size={14} />}
                  </button>
                );
              })}
            </section>
          </div>
        )}
      </AnchoredPopover>
    </>
  );
}
