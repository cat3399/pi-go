import { useRef, type PointerEvent as ReactPointerEvent, type PointerEventHandler, type RefObject } from "react";

type GestureMode = "open-sidebar" | "close-sidebar";

interface GestureState {
  mode: GestureMode;
  pointerId: number;
  startX: number;
  startY: number;
  lastX: number;
  lastAt: number;
  velocityX: number;
  engaged: boolean;
  panelWidth: number;
}

interface MobilePanelGesturesOptions {
  enabled: boolean;
  sidebarOpen: boolean;
  setSidebarOpen(open: boolean): void;
}

interface MobilePanelGestureBindings {
  ref: RefObject<HTMLDivElement | null>;
  onPointerDownCapture: PointerEventHandler<HTMLDivElement>;
  onPointerMoveCapture: PointerEventHandler<HTMLDivElement>;
  onPointerUpCapture: PointerEventHandler<HTMLDivElement>;
  onPointerCancelCapture: PointerEventHandler<HTMLDivElement>;
}

const EDGE_SIZE = 44;
const TOUCH_SLOP = 8;

export function useMobilePanelGestures(options: MobilePanelGesturesOptions): MobilePanelGestureBindings {
  const rootRef = useRef<HTMLDivElement>(null);
  const gestureRef = useRef<GestureState | null>(null);

  const clearGesture = () => {
    const root = rootRef.current;
    if (root) {
      delete root.dataset.piGesture;
      root.style.removeProperty("--pi-sidebar-gesture-x");
      root.style.removeProperty("--pi-gesture-backdrop-opacity");
    }
    gestureRef.current = null;
  };

  const onPointerDownCapture: PointerEventHandler<HTMLDivElement> = (event) => {
    if (!options.enabled || event.pointerType !== "touch" || !event.isPrimary) return;
    const target = event.target instanceof Element ? event.target : null;
    if (target?.closest("input, textarea, select, button, a, [data-swipe-ignore]")) return;
    let mode: GestureMode | null = null;
    let panel: HTMLElement | null = null;

    if (options.sidebarOpen && target?.closest("#pi-workbench-sidebar")) {
      mode = "close-sidebar";
      panel = document.getElementById("pi-workbench-sidebar");
    } else if (!options.sidebarOpen && event.clientX <= EDGE_SIZE) {
      mode = "open-sidebar";
      panel = document.getElementById("pi-workbench-sidebar");
    }
    if (!mode || !panel) return;

    const now = performance.now();
    gestureRef.current = {
      mode,
      pointerId: event.pointerId,
      startX: event.clientX,
      startY: event.clientY,
      lastX: event.clientX,
      lastAt: now,
      velocityX: 0,
      engaged: false,
      panelWidth: panel.getBoundingClientRect().width,
    };
  };

  const onPointerMoveCapture: PointerEventHandler<HTMLDivElement> = (event) => {
    const gesture = gestureRef.current;
    if (!gesture || gesture.pointerId !== event.pointerId) return;
    const dx = event.clientX - gesture.startX;
    const dy = event.clientY - gesture.startY;
    if (!gesture.engaged) {
      if (Math.abs(dy) > TOUCH_SLOP && Math.abs(dy) > Math.abs(dx)) {
        clearGesture();
        return;
      }
      if (Math.abs(dx) < TOUCH_SLOP || Math.abs(dx) < Math.abs(dy) * 1.1) return;
      gesture.engaged = true;
      event.currentTarget.setPointerCapture(event.pointerId);
      event.currentTarget.dataset.piGesture = gesture.mode;
    }

    event.preventDefault();
    const width = Math.max(1, gesture.panelWidth);
    let offset = 0;
    let progress = 0;
    if (gesture.mode === "open-sidebar") {
      offset = Math.min(0, -width + Math.max(0, dx));
      progress = 1 + offset / width;
      event.currentTarget.style.setProperty("--pi-sidebar-gesture-x", `${offset}px`);
    } else {
      offset = Math.max(-width, Math.min(0, dx));
      progress = 1 + offset / width;
      event.currentTarget.style.setProperty("--pi-sidebar-gesture-x", `${offset}px`);
    }
    event.currentTarget.style.setProperty("--pi-gesture-backdrop-opacity", String(Math.max(0, Math.min(1, progress))));
    const now = performance.now();
    const sampleTime = now - gesture.lastAt;
    if (sampleTime > 0) gesture.velocityX = (event.clientX - gesture.lastX) / sampleTime;
    gesture.lastX = event.clientX;
    gesture.lastAt = now;
  };

  const finishGesture = (event: ReactPointerEvent<HTMLDivElement>, cancelled: boolean) => {
    const gesture = gestureRef.current;
    if (!gesture || gesture.pointerId !== event.pointerId) return;
    if (event.currentTarget.hasPointerCapture(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId);
    }
    if (!cancelled && gesture.engaged) {
      const dx = event.clientX - gesture.startX;
      const distance = Math.abs(dx) / Math.max(1, gesture.panelWidth);
      const forward = gesture.mode === "open-sidebar" ? dx > 0 : dx < 0;
      const forwardVelocity = gesture.mode === "open-sidebar" ? gesture.velocityX : -gesture.velocityX;
      const complete = forward && (distance >= 0.25 || forwardVelocity >= 0.35);
      if (gesture.mode === "open-sidebar") {
        options.setSidebarOpen(complete);
      } else {
        options.setSidebarOpen(!complete);
      }
    }
    requestAnimationFrame(clearGesture);
  };

  return {
    ref: rootRef,
    onPointerDownCapture,
    onPointerMoveCapture,
    onPointerUpCapture: (event) => finishGesture(event, false),
    onPointerCancelCapture: (event) => finishGesture(event, true),
  };
}
