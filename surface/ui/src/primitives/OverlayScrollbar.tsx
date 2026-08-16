import {
  type PointerEvent as ReactPointerEvent,
  type RefObject,
  type WheelEvent as ReactWheelEvent,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

interface ScrollMetrics {
  scrollable: boolean;
  thumbHeight: number;
  thumbOffset: number;
}

interface OverlayScrollbarProps<T extends HTMLElement> {
  viewportRef: RefObject<T | null>;
}

const MINIMUM_THUMB_HEIGHT = 28;
const IDLE_DELAY_MS = 650;

export function OverlayScrollbar<T extends HTMLElement>({ viewportRef }: OverlayScrollbarProps<T>) {
  const trackRef = useRef<HTMLDivElement>(null);
  const frameRef = useRef<number | null>(null);
  const idleTimerRef = useRef<number | null>(null);
  const dragRef = useRef<{ pointerId: number; grabOffset: number } | null>(null);
  const [active, setActive] = useState(false);
  const [metrics, setMetrics] = useState<ScrollMetrics>({
    scrollable: false,
    thumbHeight: 0,
    thumbOffset: 0,
  });

  const measure = useCallback(() => {
    const viewport = viewportRef.current;
    const track = trackRef.current;
    if (!viewport || !track) return;

    const viewportHeight = viewport.clientHeight;
    const contentHeight = viewport.scrollHeight;
    const trackHeight = track.clientHeight;
    const scrollable = viewportHeight > 0 && trackHeight > 0 && contentHeight > viewportHeight + 1;
    const thumbHeight = scrollable
      ? Math.min(trackHeight, Math.max(MINIMUM_THUMB_HEIGHT, trackHeight * viewportHeight / contentHeight))
      : 0;
    const maximumScroll = Math.max(0, contentHeight - viewportHeight);
    const maximumTravel = Math.max(0, trackHeight - thumbHeight);
    const thumbOffset = maximumScroll > 0
      ? Math.min(maximumTravel, Math.max(0, viewport.scrollTop / maximumScroll * maximumTravel))
      : 0;

    setMetrics((current) => {
      if (
        current.scrollable === scrollable
        && Math.abs(current.thumbHeight - thumbHeight) < 0.5
        && Math.abs(current.thumbOffset - thumbOffset) < 0.5
      ) {
        return current;
      }
      return { scrollable, thumbHeight, thumbOffset };
    });
  }, [viewportRef]);

  const scheduleMeasure = useCallback(() => {
    if (frameRef.current !== null) return;
    frameRef.current = window.requestAnimationFrame(() => {
      frameRef.current = null;
      measure();
    });
  }, [measure]);

  const reveal = useCallback(() => {
    setActive(true);
    if (idleTimerRef.current !== null) window.clearTimeout(idleTimerRef.current);
    idleTimerRef.current = window.setTimeout(() => {
      idleTimerRef.current = null;
      if (!dragRef.current) setActive(false);
    }, IDLE_DELAY_MS);
  }, []);

  useLayoutEffect(() => {
    scheduleMeasure();
  }, [scheduleMeasure]);

  useEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport) return;

    const onScroll = () => {
      reveal();
      scheduleMeasure();
    };
    const resizeObserver = new ResizeObserver(scheduleMeasure);
    const mutationObserver = new MutationObserver(scheduleMeasure);

    viewport.addEventListener("scroll", onScroll, { passive: true });
    resizeObserver.observe(viewport);
    mutationObserver.observe(viewport, { childList: true, characterData: true, subtree: true });
    window.addEventListener("resize", scheduleMeasure);
    scheduleMeasure();

    return () => {
      viewport.removeEventListener("scroll", onScroll);
      resizeObserver.disconnect();
      mutationObserver.disconnect();
      window.removeEventListener("resize", scheduleMeasure);
    };
  }, [reveal, scheduleMeasure, viewportRef]);

  useEffect(() => () => {
    if (frameRef.current !== null) window.cancelAnimationFrame(frameRef.current);
    if (idleTimerRef.current !== null) window.clearTimeout(idleTimerRef.current);
  }, []);

  const scrollToTrackPosition = (trackPosition: number, grabOffset: number) => {
    const viewport = viewportRef.current;
    const track = trackRef.current;
    if (!viewport || !track || !metrics.scrollable) return;
    const maximumTravel = Math.max(0, track.clientHeight - metrics.thumbHeight);
    const maximumScroll = Math.max(0, viewport.scrollHeight - viewport.clientHeight);
    const thumbOffset = Math.min(maximumTravel, Math.max(0, trackPosition - grabOffset));
    viewport.scrollTop = maximumTravel > 0 ? thumbOffset / maximumTravel * maximumScroll : 0;
  };

  const onPointerDown = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (event.button !== 0 || !metrics.scrollable) return;
    const track = trackRef.current;
    if (!track) return;
    event.preventDefault();
    event.stopPropagation();

    const trackPosition = event.clientY - track.getBoundingClientRect().top;
    const onThumb = trackPosition >= metrics.thumbOffset
      && trackPosition <= metrics.thumbOffset + metrics.thumbHeight;
    const grabOffset = onThumb ? trackPosition - metrics.thumbOffset : metrics.thumbHeight / 2;
    if (!onThumb) scrollToTrackPosition(trackPosition, grabOffset);
    dragRef.current = { pointerId: event.pointerId, grabOffset };
    event.currentTarget.setPointerCapture(event.pointerId);
    reveal();
  };

  const onPointerMove = (event: ReactPointerEvent<HTMLDivElement>) => {
    const drag = dragRef.current;
    const track = trackRef.current;
    if (!drag || drag.pointerId !== event.pointerId || !track) return;
    event.preventDefault();
    const trackPosition = event.clientY - track.getBoundingClientRect().top;
    scrollToTrackPosition(trackPosition, drag.grabOffset);
    reveal();
  };

  const finishDrag = (event: ReactPointerEvent<HTMLDivElement>) => {
    const drag = dragRef.current;
    if (!drag || drag.pointerId !== event.pointerId) return;
    dragRef.current = null;
    if (event.currentTarget.hasPointerCapture(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId);
    }
    reveal();
  };

  const onWheel = (event: ReactWheelEvent<HTMLDivElement>) => {
    const viewport = viewportRef.current;
    if (!viewport || !metrics.scrollable) return;
    event.preventDefault();
    viewport.scrollTop += event.deltaY;
    reveal();
  };

  return (
    <div
      ref={trackRef}
      className={`pi-overlay-scrollbar ${metrics.scrollable ? "is-scrollable" : ""} ${active ? "is-active" : ""}`}
      aria-hidden="true"
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={finishDrag}
      onPointerCancel={finishDrag}
      onLostPointerCapture={finishDrag}
      onWheel={onWheel}
    >
      <span
        className="pi-overlay-scrollbar-thumb"
        style={{
          height: `${metrics.thumbHeight}px`,
          transform: `translateY(${metrics.thumbOffset}px)`,
        }}
      />
    </div>
  );
}
