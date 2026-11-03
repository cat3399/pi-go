import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from "react";
import { MarkdownBody } from "../content/MarkdownBody";
import { MessageAnchors, type MessageAnchorItem, type MessageAnchorsHandle } from "./MessageAnchors";
import { useAnchorGestures } from "./useAnchorGestures";

interface MarkdownFilePreviewProps {
  content: string;
  mobile: boolean;
  anchorsEnabled: boolean;
  onAnchorsAvailableChange(available: boolean): void;
}

export function MarkdownFilePreview({ content, mobile, anchorsEnabled, onAnchorsAvailableChange }: MarkdownFilePreviewProps) {
  const scope = useId();
  const viewportRef = useRef<HTMLDivElement>(null);
  const bodyRef = useRef<HTMLDivElement>(null);
  const navigatorRef = useRef<MessageAnchorsHandle>(null);
  const headingsRef = useRef<HTMLElement[]>([]);
  const offsetsRef = useRef<number[]>([]);
  const [items, setItems] = useState<MessageAnchorItem[]>([]);
  const [activeIndex, setActiveIndex] = useState(0);
  // Scrolling the chapter navigator must not re-parse a large document.
  const body = useMemo(() => <MarkdownBody>{content}</MarkdownBody>, [content]);

  useLayoutEffect(() => {
    const viewport = viewportRef.current;
    const body = bodyRef.current;
    if (!viewport || !body) return;
    const headings = Array.from(body.querySelectorAll<HTMLElement>("h1, h2, h3, h4, h5, h6"));
    headingsRef.current = headings;
    setItems(headings.map((heading, index) => ({
      id: `${scope}-${index}`,
      label: (heading.textContent ?? "").replace(/\s+/g, " ").trim(),
      level: Number(heading.tagName.slice(1)),
    })));
    viewport.scrollTop = 0;
    setActiveIndex(0);

    const syncActive = () => {
      const offsets = offsetsRef.current;
      if (offsets.length === 0) return;
      if (viewport.scrollTop > 0 && viewport.scrollTop + viewport.clientHeight >= viewport.scrollHeight - 2) {
        setActiveIndex(offsets.length - 1);
        return;
      }
      const focus = viewport.scrollTop + 24;
      let low = 0, high = offsets.length;
      while (low < high) {
        const middle = (low + high) >>> 1;
        if (offsets[middle]! <= focus) low = middle + 1;
        else high = middle;
      }
      setActiveIndex(Math.max(0, low - 1));
    };
    const measure = () => {
      const origin = viewport.getBoundingClientRect().top - viewport.scrollTop;
      offsetsRef.current = headings.map((heading) => heading.getBoundingClientRect().top - origin);
    };
    let frame = 0;
    let geometryChanged = false;
    const schedule = () => {
      if (frame) return;
      frame = requestAnimationFrame(() => {
        frame = 0;
        if (geometryChanged) { measure(); geometryChanged = false; }
        syncActive();
      });
    };
    const resize = () => { geometryChanged = true; schedule(); };
    const observer = new ResizeObserver(resize);
    observer.observe(viewport);
    observer.observe(body);
    viewport.addEventListener("scroll", schedule, { passive: true });
    body.addEventListener("load", resize, true);
    measure();
    syncActive();
    return () => {
      observer.disconnect();
      viewport.removeEventListener("scroll", schedule);
      body.removeEventListener("load", resize, true);
      if (frame) cancelAnimationFrame(frame);
    };
  }, [content, scope]);

  useEffect(() => {
    onAnchorsAvailableChange(items.length > 0);
    return () => onAnchorsAvailableChange(false);
  }, [items.length, onAnchorsAvailableChange]);

  const gestures = useAnchorGestures({
    navigatorRef, mobile, enabled: anchorsEnabled, itemCount: items.length, activeIndex, scope,
    onSelect(index) {
      const viewport = viewportRef.current, heading = headingsRef.current[index];
      if (!viewport || !heading) return;
      const top = viewport.scrollTop + heading.getBoundingClientRect().top - viewport.getBoundingClientRect().top - 16;
      setActiveIndex(index);
      heading.tabIndex = -1;
      heading.focus({ preventScroll: true });
      viewport.scrollTo({ top: Math.max(0, top), behavior: window.matchMedia("(prefers-reduced-motion: reduce)").matches ? "instant" : "smooth" });
    },
  });

  return (
    <div className="pi-markdown-preview-stage"
      onPointerDownCapture={gestures.onPointerDownCapture}
      onPointerMoveCapture={gestures.onPointerMoveCapture}
      onPointerUpCapture={gestures.onPointerUpCapture}
      onPointerCancelCapture={gestures.onPointerCancelCapture}>
      <div ref={viewportRef} className="pi-file-preview-content">
        <div ref={bodyRef} className="pi-file-preview-markdown">{body}</div>
      </div>
      <MessageAnchors ref={navigatorRef} kind="chapters" items={items} activeIndex={activeIndex}
        mobile={mobile} open={gestures.open} onSelect={gestures.select} />
    </div>
  );
}
