import {
  forwardRef,
  useEffect,
  useImperativeHandle,
  useLayoutEffect,
  useRef,
  useState,
  type CSSProperties,
} from "react";

export interface MessageAnchorItem {
  id: string;
  label: string;
}

interface MessageAnchorsProps {
  items: MessageAnchorItem[];
  activeIndex: number;
  mobile: boolean;
  open: boolean;
  onSelect(index: number): void;
}

export interface MessageAnchorsHandle {
  setGestureIndex(index: number | null): void;
  setPreviewIndex(index: number | null): void;
  setPreviewPosition(position: AnchorPreviewPosition | null): void;
}

interface AnchorPreviewPosition {
  clientY: number;
  stageTop: number;
  stageBottom: number;
}

function positionPreview(element: HTMLDivElement | null, position: AnchorPreviewPosition | null): void {
  if (!element) return;
  if (!position) {
    element.style.removeProperty("--pi-anchor-scrub-top");
    return;
  }
  const halfHeight = element.getBoundingClientRect().height / 2;
  const minimumCenter = position.stageTop + halfHeight;
  const maximumCenter = Math.max(minimumCenter, position.stageBottom - halfHeight);
  const previewCenter = Math.max(minimumCenter, Math.min(maximumCenter, position.clientY));
  element.style.setProperty("--pi-anchor-scrub-top", `${previewCenter - halfHeight}px`);
}

export const MessageAnchors = forwardRef<MessageAnchorsHandle, MessageAnchorsProps>(function MessageAnchors(props, ref) {
  const [gestureIndex, setGestureIndex] = useState<number | null>(null);
  const [previewIndex, setPreviewIndex] = useState<number | null>(null);
  const previewRef = useRef<HTMLDivElement>(null);
  const previewPositionRef = useRef<AnchorPreviewPosition | null>(null);

  useImperativeHandle(ref, () => ({
    setGestureIndex,
    setPreviewIndex,
    setPreviewPosition(position) {
      previewPositionRef.current = position;
      positionPreview(previewRef.current, position);
    },
  }), []);
  useEffect(() => {
    if (!props.open) {
      setGestureIndex(null);
      setPreviewIndex(null);
    }
  }, [props.open]);

  const displayedActiveIndex = gestureIndex === null
    ? props.activeIndex
    : Math.max(0, Math.min(gestureIndex, props.items.length - 1));
  const displayedPreviewIndex = previewIndex === null || props.items.length === 0
    ? null
    : Math.max(0, Math.min(previewIndex, props.items.length - 1));
  const preview = displayedPreviewIndex === null ? null : props.items[displayedPreviewIndex];
  useLayoutEffect(() => {
    positionPreview(previewRef.current, previewPositionRef.current);
  }, [displayedPreviewIndex, preview?.label]);
  if (props.items.length === 0) return null;
  const desktopHeight = Math.min(320, 16 + props.items.length * 18);
  const mobileHeight = Math.max(40, 16 + props.items.length * 24);
  const mobileDotSize = props.items.length > 120 ? 3 : props.items.length > 80 ? 4 : props.items.length > 60 ? 5 : 6;

  return (
    <>
      <aside
        id="pi-message-anchors"
        className={`pi-message-anchors ${props.mobile ? "is-mobile" : "is-desktop"} ${props.open ? "is-open" : ""} ${gestureIndex !== null ? "is-scrubbing" : ""}`}
        aria-label="消息锚点"
        style={props.mobile
          ? {
              "--pi-anchor-mobile-height": `${mobileHeight}px`,
              "--pi-anchor-size": `${mobileDotSize}px`,
            } as CSSProperties
          : { height: `min(${desktopHeight}px, calc(100% - 112px))` }}
      >
        <div className="pi-anchor-track" aria-label="快速定位到对话">
          {props.items.length > 1 && <span className="pi-anchor-line" />}
          {props.items.map((item, index) => (
            <button
              key={item.id}
              type="button"
              className={index === displayedActiveIndex ? "is-active" : ""}
              tabIndex={props.mobile ? -1 : 0}
              aria-label={`第 ${index + 1} 轮：${item.label}`}
              onClick={() => props.onSelect(index)}
            >
              <span className="pi-anchor-dot" />
              {!props.mobile && (
                <span className="pi-anchor-preview">
                  <small>{String(index + 1).padStart(2, "0")}</small>
                  <span>{item.label || "（无文字消息）"}</span>
                </span>
              )}
            </button>
          ))}
        </div>
      </aside>
      {props.mobile && (
        <div
          ref={previewRef}
          className={`pi-anchor-scrub-preview ${props.open && preview ? "is-visible" : ""}`}
          aria-hidden="true"
        >
          <small>{preview && displayedPreviewIndex !== null ? String(displayedPreviewIndex + 1).padStart(2, "0") : ""}</small>
          <span>{preview?.label || (preview ? "（无文字消息）" : "")}</span>
        </div>
      )}
    </>
  );
});
