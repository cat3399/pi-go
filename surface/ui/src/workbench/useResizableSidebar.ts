import {
  type KeyboardEvent,
  type PointerEvent,
  useCallback,
  useEffect,
  useRef,
  useState,
} from "react";

export const SIDEBAR_DEFAULT_WIDTH = 275;
export const SIDEBAR_MIN_WIDTH = 220;
export const SIDEBAR_MAX_WIDTH = 420;

const STORAGE_KEY = "pi-sidebar-width";

function maximumWidth(): number {
  return Math.max(SIDEBAR_MIN_WIDTH, Math.min(SIDEBAR_MAX_WIDTH, window.innerWidth - 480));
}

function clampWidth(value: number): number {
  return Math.round(Math.max(SIDEBAR_MIN_WIDTH, Math.min(maximumWidth(), value)));
}

function storedWidth(): number {
  try {
    const value = Number.parseFloat(localStorage.getItem(STORAGE_KEY) ?? "");
    return Number.isFinite(value) ? clampWidth(value) : SIDEBAR_DEFAULT_WIDTH;
  } catch {
    return SIDEBAR_DEFAULT_WIDTH;
  }
}

function saveWidth(value: number): void {
  try {
    localStorage.setItem(STORAGE_KEY, String(value));
  } catch {
    // Resizing remains available when storage is unavailable.
  }
}

export function useResizableSidebar() {
  const [width, setWidth] = useState(storedWidth);
  const [resizing, setResizing] = useState(false);
  const widthRef = useRef(width);
  const dragRef = useRef<{ pointerId: number; startX: number; startWidth: number } | null>(null);
  widthRef.current = width;

  const updateWidth = useCallback((value: number, persist = false) => {
    const next = clampWidth(value);
    widthRef.current = next;
    setWidth(next);
    if (persist) saveWidth(next);
  }, []);

  useEffect(() => {
    const reclamp = () => updateWidth(widthRef.current, true);
    window.addEventListener("resize", reclamp);
    return () => window.removeEventListener("resize", reclamp);
  }, [updateWidth]);

  const onPointerDown = (event: PointerEvent<HTMLDivElement>) => {
    if (event.button !== 0) return;
    event.preventDefault();
    dragRef.current = {
      pointerId: event.pointerId,
      startX: event.clientX,
      startWidth: widthRef.current,
    };
    event.currentTarget.setPointerCapture(event.pointerId);
    setResizing(true);
  };

  const onPointerMove = (event: PointerEvent<HTMLDivElement>) => {
    const drag = dragRef.current;
    if (!drag || drag.pointerId !== event.pointerId) return;
    updateWidth(drag.startWidth + event.clientX - drag.startX);
  };

  const finishResize = (event: PointerEvent<HTMLDivElement>) => {
    const drag = dragRef.current;
    if (!drag || drag.pointerId !== event.pointerId) return;
    dragRef.current = null;
    setResizing(false);
    saveWidth(widthRef.current);
    if (event.currentTarget.hasPointerCapture(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId);
    }
  };

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const step = event.shiftKey ? 24 : 8;
    if (event.key === "ArrowLeft") {
      event.preventDefault();
      updateWidth(widthRef.current - step, true);
    } else if (event.key === "ArrowRight") {
      event.preventDefault();
      updateWidth(widthRef.current + step, true);
    } else if (event.key === "Home") {
      event.preventDefault();
      updateWidth(SIDEBAR_MIN_WIDTH, true);
    } else if (event.key === "End") {
      event.preventDefault();
      updateWidth(maximumWidth(), true);
    }
  };

  return {
    width,
    resizing,
    separatorProps: {
      onPointerDown,
      onPointerMove,
      onPointerUp: finishResize,
      onPointerCancel: finishResize,
      onLostPointerCapture: finishResize,
      onKeyDown,
    },
  };
}
