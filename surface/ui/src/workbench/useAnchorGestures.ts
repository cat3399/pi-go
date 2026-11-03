import { useCallback, useEffect, useLayoutEffect, useRef, useState, type PointerEvent as ReactPointerEvent, type RefObject } from "react";
import type { MessageAnchorsHandle } from "./MessageAnchors";
import { blocksEdgeGestureStart, isTextSelectionInteraction } from "./edge-gesture-target";

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

interface AnchorGesturesOptions {
  navigatorRef: RefObject<MessageAnchorsHandle | null>;
  mobile: boolean;
  enabled: boolean;
  itemCount: number;
  activeIndex: number;
  scope: string;
  onSelect(index: number): void;
}

// Shared by transcript messages and Markdown chapters. Each navigator owns its
// track and gesture state, so two visible panels never query each other's DOM.
export function useAnchorGestures({ navigatorRef, mobile, enabled, itemCount, activeIndex, scope, onSelect }: AnchorGesturesOptions) {
  const gestureRef = useRef<AnchorGesture | null>(null);
  const anchorPreviewTimerRef = useRef<number | null>(null);
  const anchorPreviewVisibleRef = useRef(false);
  const [open, setOpen] = useState(false);
  const reset = useCallback(() => {
    gestureRef.current = null;
    if (anchorPreviewTimerRef.current !== null) window.clearTimeout(anchorPreviewTimerRef.current);
    anchorPreviewTimerRef.current = null;
    anchorPreviewVisibleRef.current = false;
    navigatorRef.current?.setGestureIndex(null);
    navigatorRef.current?.setPreviewIndex(null);
    navigatorRef.current?.setPreviewPosition(null);
    setOpen(false);
  }, [navigatorRef]);

  const anchorTrackMetrics = () => {
    const track = navigatorRef.current?.getTrackElement();
    if (!track || itemCount === 0) return null;
    const bounds = track.getBoundingClientRect();
    const innerTop = bounds.top + 8;
    const innerHeight = Math.max(0, bounds.height - 16);
    if (bounds.width <= 0 || innerHeight <= 0) return null;
    return {
      innerTop,
      innerHeight,
      slotHeight: innerHeight / itemCount,
    };
  };

  const anchorIndexAtY = (clientY: number, gesture: AnchorGesture): number | null => {
    const metrics = gesture.trackMetrics ?? anchorTrackMetrics();
    if (!metrics) return null;
    gesture.trackMetrics = metrics;
    if (clientY < metrics.innerTop || clientY > metrics.innerTop + metrics.innerHeight) return null;
    const estimated = Math.round((clientY - metrics.innerTop) / metrics.slotHeight - 0.5);
    return Math.max(0, Math.min(itemCount - 1, estimated));
  };

  const clearAnchorPreviewTimer = () => {
    if (anchorPreviewTimerRef.current === null) return;
    window.clearTimeout(anchorPreviewTimerRef.current);
    anchorPreviewTimerRef.current = null;
  };

  const hideAnchorPreview = () => {
    clearAnchorPreviewTimer();
    anchorPreviewVisibleRef.current = false;
    navigatorRef.current?.setPreviewPosition(null);
    navigatorRef.current?.setPreviewIndex(null);
  };

  const positionAnchorPreview = (gesture: AnchorGesture, clientY: number) => {
    navigatorRef.current?.setPreviewPosition({
      clientY,
      stageTop: gesture.stageTop,
      stageBottom: gesture.stageBottom,
    });
  };

  const scheduleAnchorPreview = (index: number) => {
    clearAnchorPreviewTimer();
    anchorPreviewTimerRef.current = window.setTimeout(() => {
      anchorPreviewTimerRef.current = null;
      const gesture = gestureRef.current;
      if (
        !gesture?.engaged
        || !gesture.hasSelection
        || !gesture.insideHitRegion
        || gesture.index !== index
      ) return;
      positionAnchorPreview(gesture, gesture.lastY);
      anchorPreviewVisibleRef.current = true;
      navigatorRef.current?.setPreviewIndex(index);
    }, ANCHOR_PREVIEW_DWELL_MS);
  };

  const onAnchorPointerDown = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (!mobile || !enabled || itemCount === 0 || event.pointerType !== "touch" || !event.isPrimary) return;
    const target = event.target instanceof Element ? event.target : null;
    if (
      (
        blocksEdgeGestureStart(target)
        || isTextSelectionInteraction(target, event.clientX, event.clientY)
      )
      && !(target && navigatorRef.current?.contains(target))
    ) return;
    const bounds = event.currentTarget.getBoundingClientRect();
    if (bounds.right - event.clientX > MOBILE_EDGE_SIZE) return;
    gestureRef.current = {
      pointerId: event.pointerId,
      startX: event.clientX,
      startY: event.clientY,
      lastY: event.clientY,
      engaged: false,
      hasSelection: false,
      insideHitRegion: false,
      index: activeIndex,
      trackMetrics: null,
      stageTop: bounds.top,
      stageBottom: bounds.bottom,
    };
  };

  const onAnchorPointerMove = (event: ReactPointerEvent<HTMLDivElement>) => {
    const gesture = gestureRef.current;
    if (!gesture || gesture.pointerId !== event.pointerId) return;
    const dx = event.clientX - gesture.startX;
    const dy = event.clientY - gesture.startY;
    gesture.lastY = event.clientY;
    if (!gesture.engaged) {
      if (isTextSelectionInteraction(event.target, event.clientX, event.clientY)) {
        gestureRef.current = null;
        return;
      }
      if (Math.abs(dy) > TOUCH_SLOP && Math.abs(dy) > Math.abs(dx)) {
        gestureRef.current = null;
        return;
      }
      if (dx > -TOUCH_SLOP || Math.abs(dx) < Math.abs(dy) * 1.1) return;
      gesture.engaged = true;
      event.currentTarget.setPointerCapture(event.pointerId);
      setOpen(true);
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
      navigatorRef.current?.setGestureIndex(index);
    }
    if (anchorPreviewVisibleRef.current) {
      positionAnchorPreview(gesture, event.clientY);
      if (changed || entered) navigatorRef.current?.setPreviewIndex(index);
    } else if (changed || entered) {
      scheduleAnchorPreview(index);
    }
  };

  const finishAnchorGesture = (event: ReactPointerEvent<HTMLDivElement>, cancelled: boolean) => {
    const gesture = gestureRef.current;
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
    gestureRef.current = null;
    navigatorRef.current?.setGestureIndex(null);
    hideAnchorPreview();
    if (!gesture.engaged) return;
    setOpen(false);
    if (cancelled || selected === null) return;
    onSelect(selected);
  };

  useLayoutEffect(reset, [scope, reset]);
  useEffect(() => { if (!enabled) reset(); }, [enabled, reset]);
  useEffect(() => {
    if (!mobile || !open) return;
    const dismiss = (event: PointerEvent) => {
      if (event.target instanceof Node && navigatorRef.current?.contains(event.target)) return;
      reset();
    };
    document.addEventListener("pointerdown", dismiss, true);
    return () => document.removeEventListener("pointerdown", dismiss, true);
  }, [mobile, open, navigatorRef, reset]);
  useEffect(() => () => {
    if (anchorPreviewTimerRef.current !== null) window.clearTimeout(anchorPreviewTimerRef.current);
  }, []);

  return {
    open: !mobile || (enabled && open),
    select(index: number) { onSelect(index); if (mobile) reset(); },
    onPointerDownCapture: onAnchorPointerDown,
    onPointerMoveCapture: onAnchorPointerMove,
    onPointerUpCapture: (event: ReactPointerEvent<HTMLDivElement>) => finishAnchorGesture(event, false),
    onPointerCancelCapture: (event: ReactPointerEvent<HTMLDivElement>) => finishAnchorGesture(event, true),
  };
}
