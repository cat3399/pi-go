import {
  type CSSProperties,
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";
import type { ContextUsage, SessionStatsInfo, TokenUsageInfo } from "../contracts";

interface ContextUsageIndicatorProps {
  usage: ContextUsage | null;
  latestUsage: TokenUsageInfo | null;
  sessionStats: SessionStatsInfo | null;
}

function formatTokens(value: number): string {
  const rounded = Math.max(0, value);
  if (rounded >= 1_000_000) return `${stripTrailingZero(rounded / 1_000_000)}M`;
  if (rounded >= 1_000) return `${stripTrailingZero(rounded / 1_000)}k`;
  return Math.round(rounded).toLocaleString("zh-CN");
}

function stripTrailingZero(value: number): string {
  return value.toFixed(1).replace(/\.0$/, "");
}

function promptTokens(usage: TokenUsageInfo): number {
  return usage.input + usage.cacheRead + usage.cacheWrite;
}

function cacheRate(usage: TokenUsageInfo): string {
  const input = promptTokens(usage);
  return input > 0 ? `${((usage.cacheRead / input) * 100).toFixed(1)}%` : "—";
}

function formatCost(value: number): string {
  if (!Number.isFinite(value) || value < 0) return "—";
  return `$${value.toFixed(4)}`;
}

function DetailRows(props: { rows: Array<[string, string]> }) {
  return (
    <dl>
      {props.rows.map(([label, value]) => (
        <div key={label}>
          <dt>{label}</dt>
          <dd>{value}</dd>
        </div>
      ))}
    </dl>
  );
}

export function ContextUsageIndicator({ usage, latestUsage, sessionStats }: ContextUsageIndicatorProps) {
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
      left: Math.max(132, Math.min(window.innerWidth - 132, rect.left + rect.width / 2)),
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

  const contextDetail = usage?.contextWindow
    ? `${usage.tokens === null ? "—" : formatTokens(usage.tokens)} / ${formatTokens(usage.contextWindow)} · ${typeof rawPercent === "number" ? `${rawPercent.toFixed(1)}%` : "未知"}`
    : "尚无数据";
  const latestRows: Array<[string, string]> = latestUsage ? [
    ["输入", formatTokens(promptTokens(latestUsage))],
    ["缓存", cacheRate(latestUsage)],
    ["输出", formatTokens(latestUsage.output)],
  ] : [["输入", "—"], ["缓存", "—"], ["输出", "—"]];
  const cumulativeInput = sessionStats
    ? sessionStats.tokens.input + sessionStats.tokens.cacheRead + sessionStats.tokens.cacheWrite
    : null;
  const cumulativeRows: Array<[string, string]> = sessionStats ? [
    ["输入", formatTokens(cumulativeInput ?? 0)],
    ["输出", formatTokens(sessionStats.tokens.output)],
    ["费用", formatCost(sessionStats.cost)],
  ] : [["输入", "—"], ["输出", "—"], ["费用", "—"]];

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
          <div className="pi-context-tooltip-summary">
            <span>上下文</span>
            <strong>{contextDetail}</strong>
          </div>
          <section>
            <h3>最近一轮</h3>
            <DetailRows rows={latestRows} />
          </section>
          <section>
            <h3>会话累计</h3>
            <DetailRows rows={cumulativeRows} />
          </section>
        </div>,
        document.body,
      )}
    </>
  );
}
