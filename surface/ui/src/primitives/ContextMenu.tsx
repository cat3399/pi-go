import { useCallback, useEffect, useRef, useState, type KeyboardEvent, type MouseEvent, type PointerEvent, type ReactNode } from "react";
import { AnchoredPopover } from "./AnchoredPopover";

export function useContextMenu<Action extends string, Trigger extends HTMLElement = HTMLButtonElement>(onAction: (action: Action) => void) {
  const anchorRef = useRef<Trigger>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const cancelGestureRef = useRef<(() => void) | null>(null);
  const suppressClickUntil = useRef(0);
  const [mode, setMode] = useState<"pointer" | "touch" | null>(null);
  const [point, setPoint] = useState<{ x: number; y: number }>();
  const [activeAction, setActiveAction] = useState<Action | null>(null);

  const dismiss = useCallback(() => {
    cancelGestureRef.current?.();
    if (menuRef.current?.contains(document.activeElement)) anchorRef.current?.focus({ preventScroll: true });
    setMode(null);
    setActiveAction(null);
  }, []);

  const choose = (action: Action) => {
    dismiss();
    onAction(action);
  };

  useEffect(() => () => cancelGestureRef.current?.(), []);

  useEffect(() => {
    if (mode === "pointer") menuRef.current?.querySelector<HTMLButtonElement>("[role=menuitem]:not(:disabled)")?.focus({ preventScroll: true });
  }, [mode]);

  const onPointerDown = (event: PointerEvent<Trigger>) => {
    if (event.pointerType === "mouse" || !event.isPrimary || event.button !== 0) return;
    cancelGestureRef.current?.();
    setMode(null);
    setActiveAction(null);
    suppressClickUntil.current = 0;
    const { pointerId, clientX: startX, clientY: startY } = event;
    let opened = false;

    const actionAt = (x: number, y: number): Action | null => {
      const item = document.elementFromPoint(x, y)?.closest<HTMLButtonElement>("[data-context-action]");
      if (!item || !menuRef.current?.contains(item) || item.disabled) return null;
      return (item.dataset.contextAction as Action | undefined) ?? null;
    };

    const cleanup = () => {
      window.clearTimeout(timer);
      document.removeEventListener("pointermove", move);
      document.removeEventListener("pointerup", finish);
      document.removeEventListener("pointercancel", cancel);
      document.removeEventListener("pointerdown", additionalPointer, true);
      document.removeEventListener("touchmove", preventScroll);
      window.removeEventListener("scroll", cancel, true);
      window.removeEventListener("blur", cancel);
      cancelGestureRef.current = null;
      if (opened) suppressClickUntil.current = performance.now() + 700;
    };
    const cancel = () => {
      cleanup();
      setMode(null);
      setActiveAction(null);
    };
    const move = (next: globalThis.PointerEvent) => {
      if (next.pointerId !== pointerId) return;
      if (!opened) {
        if (Math.hypot(next.clientX - startX, next.clientY - startY) > 10) cancel();
        return;
      }
      next.preventDefault();
      setActiveAction(actionAt(next.clientX, next.clientY));
    };
    const finish = (next: globalThis.PointerEvent) => {
      if (next.pointerId !== pointerId) return;
      const action = opened ? actionAt(next.clientX, next.clientY) : null;
      cleanup();
      if (!opened) return;
      next.preventDefault();
      setMode(null);
      setActiveAction(null);
      if (action) onAction(action);
    };
    const additionalPointer = (next: globalThis.PointerEvent) => {
      if (next.pointerId !== pointerId) cancel();
    };
    // Keep ordinary scrolling until the hold opens the menu. React touch
    // listeners are passive, so the active drag needs a native listener.
    const preventScroll = (next: TouchEvent) => {
      if (opened && next.cancelable) next.preventDefault();
    };
    const timer = window.setTimeout(() => {
      opened = true;
      suppressClickUntil.current = Number.POSITIVE_INFINITY;
      setPoint({ x: startX, y: startY });
      setMode("touch");
    }, 350);

    cancelGestureRef.current = cleanup;
    document.addEventListener("pointermove", move, { passive: false });
    document.addEventListener("pointerup", finish);
    document.addEventListener("pointercancel", cancel);
    document.addEventListener("pointerdown", additionalPointer, true);
    document.addEventListener("touchmove", preventScroll, { passive: false });
    window.addEventListener("scroll", cancel, true);
    window.addEventListener("blur", cancel);
  };

  const onContextMenu = (event: MouseEvent<Trigger>) => {
    event.preventDefault();
    event.stopPropagation();
    if (performance.now() < suppressClickUntil.current) return;
    setPoint({ x: event.clientX, y: event.clientY });
    setMode("pointer");
  };

  const onTriggerKeyDown = (event: KeyboardEvent<Trigger>) => {
    if (event.key !== "ContextMenu" && !(event.shiftKey && event.key === "F10")) return;
    event.preventDefault();
    event.stopPropagation();
    setPoint(undefined);
    setMode("pointer");
  };

  const onClickCapture = (event: MouseEvent<Trigger>) => {
    if (event.detail === 0 || performance.now() >= suppressClickUntil.current) return;
    event.preventDefault();
    event.stopPropagation();
  };

  const onMenuKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === "Escape" || event.key === "Tab") {
      if (event.key === "Escape") event.preventDefault();
      event.stopPropagation();
      dismiss();
      return;
    }
    if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    event.stopPropagation();
    const items = Array.from(menuRef.current?.querySelectorAll<HTMLButtonElement>("[role=menuitem]:not(:disabled)") ?? []);
    const current = items.findIndex((item) => item === document.activeElement);
    const next = event.key === "Home" ? 0 : event.key === "End" ? items.length - 1
      : (current + (event.key === "ArrowDown" ? 1 : -1) + items.length) % items.length;
    items[next]?.focus();
  };

  return { anchorRef, menuRef, point, open: mode !== null, activeAction, choose, dismiss, onPointerDown, onContextMenu, onTriggerKeyDown, onClickCapture, onMenuKeyDown };
}

export interface ContextMenuItem<Action extends string> {
  id: Action;
  label: string;
  icon: ReactNode;
  disabled?: boolean;
  danger?: boolean;
}

export function ContextMenu<Action extends string, Trigger extends HTMLElement>({ name, items, menu }: {
  name: string;
  items: ContextMenuItem<Action>[];
  menu: ReturnType<typeof useContextMenu<Action, Trigger>>;
}) {
  return (
    <AnchoredPopover
      ref={menu.menuRef}
      anchorRef={menu.anchorRef}
      anchorPoint={menu.point}
      open={menu.open}
      onDismiss={menu.dismiss}
      align="start"
      gap={4}
      maxHeight={240}
      className="pi-context-menu"
      role="menu"
      aria-label={`${name} 操作`}
      onKeyDown={menu.onMenuKeyDown}
      onContextMenu={(event) => event.preventDefault()}
    >
      {items.map((item) => (
        <button key={item.id} type="button" role="menuitem" data-context-action={item.id} disabled={item.disabled}
          className={`${item.danger ? "is-danger" : ""} ${menu.activeAction === item.id ? "is-active" : ""}`}
          onClick={() => menu.choose(item.id)}>
          {item.icon}<span>{item.label}</span>
        </button>
      ))}
    </AnchoredPopover>
  );
}
