import {
  forwardRef,
  type CSSProperties,
  type HTMLAttributes,
  type Ref,
  type RefObject,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";

type PopoverAlign = "start" | "center" | "end";
type PopoverPlacement = "auto" | "above" | "below";

interface ViewportBounds {
  top: number;
  right: number;
  bottom: number;
  left: number;
}

interface AnchoredPopoverProps extends HTMLAttributes<HTMLDivElement> {
  anchorRef: RefObject<HTMLElement | null>;
  open: boolean;
  align?: PopoverAlign;
  placement?: PopoverPlacement;
  gap?: number;
  gutter?: number;
  maxHeight?: number;
  minWidth?: number | "anchor";
  onDismiss?(): void;
}

function viewportBounds(): ViewportBounds {
  const viewport = window.visualViewport;
  if (!viewport) {
    return { top: 0, right: window.innerWidth, bottom: window.innerHeight, left: 0 };
  }
  return {
    top: viewport.offsetTop,
    right: viewport.offsetLeft + viewport.width,
    bottom: viewport.offsetTop + viewport.height,
    left: viewport.offsetLeft,
  };
}

function assignRef<T>(ref: Ref<T> | undefined, value: T | null): void {
  if (typeof ref === "function") {
    ref(value);
  } else if (ref) {
    ref.current = value;
  }
}

/**
 * Portal-backed anchored surface with shared viewport collision handling.
 * Callers keep ownership of menu/listbox semantics while placement, dismissal,
 * resize, scroll, soft-keyboard and safe-edge behavior stay consistent.
 */
export const AnchoredPopover = forwardRef<HTMLDivElement, AnchoredPopoverProps>(function AnchoredPopover({
  anchorRef,
  open,
  align = "end",
  placement = "auto",
  gap = 6,
  gutter = 8,
  maxHeight = 320,
  minWidth,
  onDismiss,
  style,
  children,
  ...surfaceProps
}, forwardedRef) {
  const surfaceRef = useRef<HTMLDivElement>(null);
  const frameRef = useRef<number | null>(null);
  const [position, setPosition] = useState<CSSProperties>({
    top: 0,
    left: 0,
    visibility: "hidden",
  });

  const setSurfaceRef = useCallback((element: HTMLDivElement | null) => {
    surfaceRef.current = element;
    assignRef(forwardedRef, element);
  }, [forwardedRef]);

  const updatePosition = useCallback(() => {
    const anchor = anchorRef.current;
    const surface = surfaceRef.current;
    if (!anchor || !surface) return;

    const viewport = viewportBounds();
    const anchorRect = anchor.getBoundingClientRect();
    const surfaceRect = surface.getBoundingClientRect();
    const viewportWidth = Math.max(0, viewport.right - viewport.left - gutter * 2);
    const measuredWidth = Math.min(surfaceRect.width, viewportWidth);
    // The current max-height may come from a shorter previous page. scrollHeight
    // preserves the content's natural height so placement cannot get stuck on
    // the wrong side after a menu changes pages.
    const borderHeight = Math.max(0, surfaceRect.height - surface.clientHeight);
    const naturalHeight = Math.max(surfaceRect.height, surface.scrollHeight + borderHeight);
    const desiredHeight = Math.min(naturalHeight, maxHeight);
    const spaceAbove = Math.max(0, anchorRect.top - viewport.top - gutter - gap);
    const spaceBelow = Math.max(0, viewport.bottom - anchorRect.bottom - gutter - gap);
    const placeAbove = placement === "above"
      || (placement === "auto" && desiredHeight > spaceBelow && spaceAbove > spaceBelow);
    const availableHeight = placeAbove ? spaceAbove : spaceBelow;
    const constrainedHeight = Math.max(0, Math.min(maxHeight, availableHeight));
    const renderedHeight = Math.min(naturalHeight, constrainedHeight);

    let left = anchorRect.left;
    if (align === "center") left = anchorRect.left + (anchorRect.width - measuredWidth) / 2;
    if (align === "end") left = anchorRect.right - measuredWidth;
    left = Math.max(
      viewport.left + gutter,
      Math.min(left, viewport.right - gutter - measuredWidth),
    );

    const top = placeAbove
      ? anchorRect.top - gap - renderedHeight
      : anchorRect.bottom + gap;
    setPosition({
      top: Math.max(viewport.top + gutter, Math.min(top, viewport.bottom - gutter - renderedHeight)),
      left,
      maxWidth: viewportWidth,
      maxHeight: constrainedHeight,
      minWidth: minWidth === "anchor"
        ? Math.min(anchorRect.width, viewportWidth)
        : typeof minWidth === "number"
          ? Math.min(minWidth, viewportWidth)
          : undefined,
      visibility: "visible",
    });
  }, [align, anchorRef, gap, gutter, maxHeight, minWidth, placement]);

  const schedulePositionUpdate = useCallback(() => {
    if (frameRef.current !== null) cancelAnimationFrame(frameRef.current);
    frameRef.current = requestAnimationFrame(() => {
      frameRef.current = null;
      updatePosition();
    });
  }, [updatePosition]);

  useLayoutEffect(() => {
    if (!open) return;
    updatePosition();
  }, [children, open, updatePosition]);

  useEffect(() => {
    if (!open) return;
    const viewport = window.visualViewport;
    const observer = typeof ResizeObserver === "undefined"
      ? null
      : new ResizeObserver(schedulePositionUpdate);
    if (anchorRef.current) observer?.observe(anchorRef.current);
    if (surfaceRef.current) observer?.observe(surfaceRef.current);
    window.addEventListener("resize", schedulePositionUpdate);
    window.addEventListener("scroll", schedulePositionUpdate, true);
    viewport?.addEventListener("resize", schedulePositionUpdate);
    viewport?.addEventListener("scroll", schedulePositionUpdate);
    return () => {
      observer?.disconnect();
      window.removeEventListener("resize", schedulePositionUpdate);
      window.removeEventListener("scroll", schedulePositionUpdate, true);
      viewport?.removeEventListener("resize", schedulePositionUpdate);
      viewport?.removeEventListener("scroll", schedulePositionUpdate);
      if (frameRef.current !== null) cancelAnimationFrame(frameRef.current);
      frameRef.current = null;
    };
  }, [anchorRef, open, schedulePositionUpdate]);

  useEffect(() => {
    if (!open || !onDismiss) return;
    const onPointerDown = (event: PointerEvent) => {
      const target = event.target instanceof Node ? event.target : null;
      if (!target || anchorRef.current?.contains(target) || surfaceRef.current?.contains(target)) return;
      onDismiss();
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") onDismiss();
    };
    document.addEventListener("pointerdown", onPointerDown, true);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown, true);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [anchorRef, onDismiss, open]);

  if (!open) return null;
  return createPortal(
    <div
      {...surfaceProps}
      ref={setSurfaceRef}
      style={{ ...style, ...position, position: "fixed" }}
    >
      {children}
    </div>,
    document.body,
  );
});
