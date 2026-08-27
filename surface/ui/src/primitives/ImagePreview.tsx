import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type PointerEvent as ReactPointerEvent,
  type WheelEvent as ReactWheelEvent,
} from "react";
import { createPortal } from "react-dom";
import { X } from "lucide-react";
import { useDialogFocus } from "./useDialogFocus";

interface ImagePreviewProps {
  src: string;
  alt?: string;
  onClose(): void;
}

interface ViewState {
  scale: number;
  x: number;
  y: number;
}

interface Point {
  x: number;
  y: number;
}

interface DragState extends Point {
  pointerId: number;
  viewX: number;
  viewY: number;
}

interface PinchState {
  distance: number;
  scale: number;
  imageX: number;
  imageY: number;
}

const MIN_SCALE = 1;
const MAX_SCALE = 5;

function clamp(value: number, minimum: number, maximum: number): number {
  return Math.min(maximum, Math.max(minimum, value));
}

function pointDistance(left: Point, right: Point): number {
  return Math.hypot(right.x - left.x, right.y - left.y);
}

function pointMidpoint(left: Point, right: Point): Point {
  return { x: (left.x + right.x) / 2, y: (left.y + right.y) / 2 };
}

export function ImagePreview({ src, alt = "附件图片", onClose }: ImagePreviewProps) {
  const [view, setView] = useState<ViewState>({ scale: 1, x: 0, y: 0 });
  const dialogRef = useRef<HTMLDivElement>(null);
  const closeRef = useRef<HTMLButtonElement>(null);
  const stageRef = useRef<HTMLDivElement>(null);
  const imageRef = useRef<HTMLImageElement>(null);
  const viewRef = useRef(view);
  const pointersRef = useRef(new Map<number, Point>());
  const dragRef = useRef<DragState | null>(null);
  const pinchRef = useRef<PinchState | null>(null);
  const movedRef = useRef(false);

  viewRef.current = view;
  useDialogFocus(true, dialogRef, closeRef);

  const applyView = useCallback((candidate: ViewState) => {
    const scale = clamp(candidate.scale, MIN_SCALE, MAX_SCALE);
    const image = imageRef.current;
    const stage = stageRef.current;
    const maxX = image && stage
      ? Math.max(0, (image.clientWidth * scale - stage.clientWidth) / 2)
      : 0;
    const maxY = image && stage
      ? Math.max(0, (image.clientHeight * scale - stage.clientHeight) / 2)
      : 0;
    const next = scale <= MIN_SCALE
      ? { scale: MIN_SCALE, x: 0, y: 0 }
      : {
          scale,
          x: clamp(candidate.x, -maxX, maxX),
          y: clamp(candidate.y, -maxY, maxY),
    };
    viewRef.current = next;
    setView(next);
  }, []);

  const reset = useCallback(() => applyView({ scale: 1, x: 0, y: 0 }), [applyView]);

  const zoomAt = useCallback((nextScale: number, clientX: number, clientY: number) => {
    const stage = stageRef.current;
    if (!stage) return;
    const bounds = stage.getBoundingClientRect();
    const pointX = clientX - (bounds.left + bounds.width / 2);
    const pointY = clientY - (bounds.top + bounds.height / 2);
    const current = viewRef.current;
    const imageX = (pointX - current.x) / current.scale;
    const imageY = (pointY - current.y) / current.scale;
    applyView({
      scale: nextScale,
      x: pointX - imageX * nextScale,
      y: pointY - imageY * nextScale,
    });
  }, [applyView]);

  useEffect(() => {
    reset();
  }, [reset, src]);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [onClose]);

  useEffect(() => {
    const onResize = () => applyView(viewRef.current);
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, [applyView]);

  const onWheel = (event: ReactWheelEvent<HTMLDivElement>) => {
    event.preventDefault();
    const factor = Math.exp(-event.deltaY * 0.0015);
    zoomAt(viewRef.current.scale * factor, event.clientX, event.clientY);
  };

  const beginPinch = () => {
    const points = Array.from(pointersRef.current.values());
    const first = points[0];
    const second = points[1];
    const stage = stageRef.current;
    if (!first || !second || !stage) return;
    const midpoint = pointMidpoint(first, second);
    const bounds = stage.getBoundingClientRect();
    const current = viewRef.current;
    const pointX = midpoint.x - (bounds.left + bounds.width / 2);
    const pointY = midpoint.y - (bounds.top + bounds.height / 2);
    pinchRef.current = {
      distance: Math.max(1, pointDistance(first, second)),
      scale: current.scale,
      imageX: (pointX - current.x) / current.scale,
      imageY: (pointY - current.y) / current.scale,
    };
    dragRef.current = null;
  };

  const onPointerDown = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (event.pointerType === "mouse" && event.button !== 0) return;
    const captureTarget = event.target instanceof HTMLElement ? event.target : event.currentTarget;
    captureTarget.setPointerCapture(event.pointerId);
    pointersRef.current.set(event.pointerId, { x: event.clientX, y: event.clientY });
    movedRef.current = false;
    if (pointersRef.current.size >= 2) {
      beginPinch();
      return;
    }
    if (viewRef.current.scale > 1) {
      dragRef.current = {
        pointerId: event.pointerId,
        x: event.clientX,
        y: event.clientY,
        viewX: viewRef.current.x,
        viewY: viewRef.current.y,
      };
    }
  };

  const onPointerMove = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (!pointersRef.current.has(event.pointerId)) return;
    pointersRef.current.set(event.pointerId, { x: event.clientX, y: event.clientY });
    if (pointersRef.current.size >= 2) {
      if (!pinchRef.current) beginPinch();
      const pinch = pinchRef.current;
      const points = Array.from(pointersRef.current.values());
      const first = points[0];
      const second = points[1];
      const stage = stageRef.current;
      if (!pinch || !first || !second || !stage) return;
      const midpoint = pointMidpoint(first, second);
      const bounds = stage.getBoundingClientRect();
      const pointX = midpoint.x - (bounds.left + bounds.width / 2);
      const pointY = midpoint.y - (bounds.top + bounds.height / 2);
      const scale = clamp(
        pinch.scale * pointDistance(first, second) / pinch.distance,
        MIN_SCALE,
        MAX_SCALE,
      );
      movedRef.current = true;
      applyView({
        scale,
        x: pointX - pinch.imageX * scale,
        y: pointY - pinch.imageY * scale,
      });
      return;
    }

    const drag = dragRef.current;
    if (!drag || drag.pointerId !== event.pointerId) return;
    const deltaX = event.clientX - drag.x;
    const deltaY = event.clientY - drag.y;
    if (Math.abs(deltaX) + Math.abs(deltaY) > 3) movedRef.current = true;
    applyView({
      scale: viewRef.current.scale,
      x: drag.viewX + deltaX,
      y: drag.viewY + deltaY,
    });
  };

  const finishPointer = (event: ReactPointerEvent<HTMLDivElement>) => {
    pointersRef.current.delete(event.pointerId);
    const captureTarget = event.target instanceof HTMLElement ? event.target : event.currentTarget;
    if (captureTarget.hasPointerCapture(event.pointerId)) {
      captureTarget.releasePointerCapture(event.pointerId);
    }
    pinchRef.current = null;
    dragRef.current = null;
    const remaining = Array.from(pointersRef.current.entries())[0];
    if (remaining && viewRef.current.scale > 1) {
      const [pointerId, point] = remaining;
      dragRef.current = {
        pointerId,
        x: point.x,
        y: point.y,
        viewX: viewRef.current.x,
        viewY: viewRef.current.y,
      };
    }
  };

  if (typeof document === "undefined") return null;

  return createPortal(
    <div
      ref={dialogRef}
      className="pi-image-preview"
      role="dialog"
      aria-modal="true"
      aria-label="图片预览"
      tabIndex={-1}
    >
      <div
        ref={stageRef}
        className={`pi-image-preview-stage ${view.scale > 1 ? "is-zoomed" : ""}`}
        onClick={(event) => {
          if (event.target === event.currentTarget && !movedRef.current) onClose();
        }}
        onWheel={onWheel}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={finishPointer}
        onPointerCancel={finishPointer}
      >
        <img
          ref={imageRef}
          src={src}
          alt={alt}
          draggable={false}
          style={{ transform: `translate3d(${view.x}px, ${view.y}px, 0) scale(${view.scale})` }}
          onDoubleClick={(event) => {
            if (viewRef.current.scale > 1) reset();
            else zoomAt(2, event.clientX, event.clientY);
          }}
          onLoad={() => applyView(viewRef.current)}
        />
      </div>
      <button
        ref={closeRef}
        className="pi-image-preview-close"
        type="button"
        aria-label="关闭图片预览"
        title="关闭"
        onClick={onClose}
      >
        <X size={20} />
      </button>
      <div className="pi-image-preview-scale" aria-hidden="true">
        {Math.round(view.scale * 100)}%
      </div>
    </div>,
    document.body,
  );
}
