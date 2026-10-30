import { useCallback, useEffect, useRef, useState, type KeyboardEvent, type MouseEvent, type PointerEvent } from "react";
import { AtSign, Eye, Trash2 } from "lucide-react";
import { AnchoredPopover } from "../primitives/AnchoredPopover";

type FileAction = "preview" | "mention" | "delete";

export function useFileContextMenu(onAction: (action: FileAction) => void) {
  const anchorRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const cancelGestureRef = useRef<(() => void) | null>(null);
  const suppressClickUntil = useRef(0);
  const [mode, setMode] = useState<"pointer" | "touch" | null>(null);
  const [point, setPoint] = useState<{ x: number; y: number }>();
  const [activeAction, setActiveAction] = useState<FileAction | null>(null);

  const dismiss = useCallback(() => {
    cancelGestureRef.current?.();
    if (menuRef.current?.contains(document.activeElement)) anchorRef.current?.focus({ preventScroll: true });
    setMode(null);
    setActiveAction(null);
  }, []);

  const choose = (action: FileAction) => {
    dismiss();
    onAction(action);
  };

  useEffect(() => () => cancelGestureRef.current?.(), []);

  useEffect(() => {
    if (mode === "pointer") menuRef.current?.querySelector<HTMLButtonElement>("[role=menuitem]")?.focus({ preventScroll: true });
  }, [mode]);

  const onPointerDown = (event: PointerEvent<HTMLButtonElement>) => {
    if (event.pointerType === "mouse" || !event.isPrimary || event.button !== 0) return;
    cancelGestureRef.current?.();
    setMode(null);
    setActiveAction(null);
    suppressClickUntil.current = 0;
    const { pointerId, clientX: startX, clientY: startY } = event;
    let opened = false;

    const actionAt = (x: number, y: number): FileAction | null => {
      const item = document.elementFromPoint(x, y)?.closest<HTMLButtonElement>("[data-file-action]");
      if (!item || !menuRef.current?.contains(item) || item.disabled) return null;
      const action = item.dataset.fileAction;
      return action === "preview" || action === "mention" || action === "delete" ? action : null;
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

  const onContextMenu = (event: MouseEvent<HTMLButtonElement>) => {
    event.preventDefault();
    event.stopPropagation();
    if (performance.now() < suppressClickUntil.current) return;
    setPoint({ x: event.clientX, y: event.clientY });
    setMode("pointer");
  };

  const onTriggerKeyDown = (event: KeyboardEvent<HTMLButtonElement>) => {
    if (event.key !== "ContextMenu" && !(event.shiftKey && event.key === "F10")) return;
    event.preventDefault();
    event.stopPropagation();
    setPoint(undefined);
    setMode("pointer");
  };

  const onClickCapture = (event: MouseEvent<HTMLButtonElement>) => {
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
    const items = Array.from(menuRef.current?.querySelectorAll<HTMLButtonElement>("[role=menuitem]") ?? []);
    const current = items.findIndex((item) => item === document.activeElement);
    const next = event.key === "Home" ? 0 : event.key === "End" ? items.length - 1
      : (current + (event.key === "ArrowDown" ? 1 : -1) + items.length) % items.length;
    items[next]?.focus();
  };

  return { anchorRef, menuRef, point, open: mode !== null, activeAction, choose, dismiss, onPointerDown, onContextMenu, onTriggerKeyDown, onClickCapture, onMenuKeyDown };
}

export function FileContextMenu({ name, isDir, menu }: {
  name: string;
  isDir: boolean;
  menu: ReturnType<typeof useFileContextMenu>;
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
      className="pi-file-context-menu"
      role="menu"
      aria-label={`${name} 操作`}
      onKeyDown={menu.onMenuKeyDown}
      onContextMenu={(event) => event.preventDefault()}
    >
      {!isDir && (
        <button type="button" role="menuitem" data-file-action="preview" className={menu.activeAction === "preview" ? "is-active" : ""} onClick={() => menu.choose("preview")}>
          <Eye size={15} /><span>预览</span>
        </button>
      )}
      <button type="button" role="menuitem" data-file-action="mention" className={menu.activeAction === "mention" ? "is-active" : ""} onClick={() => menu.choose("mention")}>
        <AtSign size={15} /><span>引用</span>
      </button>
      <button type="button" role="menuitem" data-file-action="delete" className={`is-danger ${menu.activeAction === "delete" ? "is-active" : ""}`} onClick={() => menu.choose("delete")}>
        <Trash2 size={15} /><span>删除</span>
      </button>
    </AnchoredPopover>
  );
}
