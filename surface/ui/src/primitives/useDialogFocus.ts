import { type RefObject, useEffect } from "react";

const focusableSelector = [
  "a[href]",
  "button:not([disabled])",
  "input:not([disabled])",
  "textarea:not([disabled])",
  "[tabindex]:not([tabindex='-1'])",
].join(",");

function focusableElements(container: HTMLElement): HTMLElement[] {
  return Array.from(container.querySelectorAll<HTMLElement>(focusableSelector)).filter((element) => (
    element.getAttribute("aria-hidden") !== "true"
    && element.getClientRects().length > 0
  ));
}

export function useDialogFocus(
  open: boolean,
  containerRef: RefObject<HTMLElement | null>,
  initialFocusRef?: RefObject<HTMLElement | null>,
) {
  useEffect(() => {
    if (!open) return;
    const container = containerRef.current;
    if (!container) return;

    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const frame = requestAnimationFrame(() => {
      const target = initialFocusRef?.current ?? focusableElements(container)[0] ?? container;
      target.focus({ preventScroll: true });
    });

    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Tab") return;
      const elements = focusableElements(container);
      if (elements.length === 0) {
        event.preventDefault();
        container.focus({ preventScroll: true });
        return;
      }

      const first = elements[0]!;
      const last = elements[elements.length - 1]!;
      const active = document.activeElement;
      if (event.shiftKey && (active === first || active === container || !container.contains(active))) {
        event.preventDefault();
        last.focus({ preventScroll: true });
      } else if (!event.shiftKey && (active === last || !container.contains(active))) {
        event.preventDefault();
        first.focus({ preventScroll: true });
      }
    };

    document.addEventListener("keydown", onKeyDown);
    return () => {
      cancelAnimationFrame(frame);
      document.removeEventListener("keydown", onKeyDown);
      if (previous?.isConnected && !previous.closest("[inert], [aria-hidden='true']")) {
        previous.focus({ preventScroll: true });
      }
    };
  }, [containerRef, initialFocusRef, open]);
}
