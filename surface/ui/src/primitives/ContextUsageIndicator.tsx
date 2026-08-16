import {
  type CSSProperties,
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";
import type { ContextUsage } from "../contracts";

interface ContextUsageIndicatorProps {
  usage: ContextUsage | null;
}

function formatTokens(value: number): string {
  return Math.max(0, Math.round(value)).toLocaleString("zh-CN");
}

export function ContextUsageIndicator({ usage }: ContextUsageIndicatorProps) {
  const rawPercent = usage?.percent;
  const percent = typeof rawPercent === "number" && Number.isFinite(rawPercent)
    ? Math.max(0, Math.min(100, rawPercent))
    : 0;
  const label = typeof rawPercent !== "number"
    ? "上下文占用未知"
    : `上下文占用 ${rawPercent.toFixed(1)}%`;
  const [open, setOpen] = useState(false);
  const [position, setPosition] = useState<CSSProperties | null>(null);
  const triggerRef = useRef<HTMLSpanElement>(null);
  const openTimerRef = useRef<number | null>(null);
  const tooltipId = useId();

  const clearOpenTimer = useCallback(() => {
    if (openTimerRef.current === null) return;
    window.clearTimeout(openTimerRef.current);
    openTimerRef.current = null;
  }, []);

  const updatePosition = useCallback(() => {
    const trigger = triggerRef.current;
    if (!trigger) return;
    const rect = trigger.getBoundingClientRect();
    setPosition({
      left: Math.max(116, Math.min(window.innerWidth - 116, rect.left + rect.width / 2)),
      bottom: window.innerHeight - rect.top + 7,
    });
  }, []);

  const show = (immediate = false) => {
    clearOpenTimer();
    if (immediate) {
      updatePosition();
      setOpen(true);
      return;
    }
    openTimerRef.current = window.setTimeout(() => {
      openTimerRef.current = null;
      updatePosition();
      setOpen(true);
    }, 140);
  };

  const hide = () => {
    clearOpenTimer();
    setOpen(false);
    setPosition(null);
  };

  useEffect(() => {
    if (!open) return;
    const reposition = () => updatePosition();
    window.addEventListener("resize", reposition);
    window.addEventListener("scroll", reposition, true);
    return () => {
      window.removeEventListener("resize", reposition);
      window.removeEventListener("scroll", reposition, true);
    };
  }, [open, updatePosition]);

  useEffect(() => () => clearOpenTimer(), [clearOpenTimer]);

  const detail = usage?.tokens !== null && usage?.tokens !== undefined && usage.contextWindow > 0
    ? `${formatTokens(usage.tokens)} / ${formatTokens(usage.contextWindow)} tokens`
    : usage?.contextWindow
      ? `已使用 token 数未知 · 上限 ${formatTokens(usage.contextWindow)}`
      : "尚无上下文占用数据";

  return (
    <>
      <span
        ref={triggerRef}
        className="pi-context-ring"
        role="img"
        tabIndex={0}
        aria-label={label}
        aria-describedby={open ? tooltipId : undefined}
        data-context-percent={percent.toFixed(2)}
        onMouseEnter={() => show()}
        onMouseLeave={hide}
        onFocus={() => show(true)}
        onBlur={hide}
      >
        <svg viewBox="0 0 16 16" aria-hidden="true">
          <circle className="pi-context-ring-track" cx="8" cy="8" r="5.25" />
          <circle
            className="pi-context-ring-value"
            cx="8"
            cy="8"
            r="5.25"
            pathLength="100"
            strokeDasharray={`${percent} 100`}
          />
        </svg>
      </span>
      {open && position && createPortal(
        <div id={tooltipId} className="pi-context-tooltip" role="tooltip" style={position}>
          <div>
            <span>上下文占用</span>
            <strong>{typeof rawPercent === "number" ? `${rawPercent.toFixed(1)}%` : "未知"}</strong>
          </div>
          <small>{detail}</small>
        </div>,
        document.body,
      )}
    </>
  );
}
