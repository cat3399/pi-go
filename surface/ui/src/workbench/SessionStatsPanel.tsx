import { useEffect, useRef, useState } from "react";
import { Check, Copy, X } from "lucide-react";
import type { SessionStatsInfo } from "../contracts";

interface SessionStatsPanelProps {
  open: boolean;
  stats: SessionStatsInfo | null;
  onClose(): void;
}

function compactNumber(value: number): string {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`;
  if (value >= 1_000) return `${Math.round(value / 1_000)}k`;
  return value.toLocaleString("zh-CN");
}

function StatSection(props: { title: string; rows: Array<[string, string]>; compact?: boolean }) {
  return (
    <section className={`pi-stats-section ${props.compact ? "is-compact" : ""}`}>
      <h3>{props.title}</h3>
      <dl>
        {props.rows.map(([label, value]) => (
          <div key={`${props.title}:${label}`}>
            <dt>{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

function CopyableValue(props: { label: string; value: string }) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    await navigator.clipboard.writeText(props.value);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1600);
  };

  return (
    <div className="pi-stats-copy-row">
      <span>{props.label}</span>
      <code>{props.value}</code>
      <button type="button" aria-label={`复制${props.label}`} title={`复制${props.label}`} onClick={() => void copy()}>
        {copied ? <Check size={12} /> : <Copy size={12} />}
      </button>
    </div>
  );
}

export function SessionStatsPanel(props: SessionStatsPanelProps) {
  const panelRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!props.open) return;
    const close = (event: MouseEvent) => {
      if (!panelRef.current?.contains(event.target as Node)) props.onClose();
    };
    document.addEventListener("mousedown", close);
    return () => document.removeEventListener("mousedown", close);
  }, [props]);

  if (!props.open) return null;

  const stats = props.stats;
  const context = stats?.contextUsage;
  const messageRows: Array<[string, string]> = stats ? [
    ["用户", stats.userMessages.toLocaleString("zh-CN")],
    ["助手", stats.assistantMessages.toLocaleString("zh-CN")],
    ["工具调用", stats.toolCalls.toLocaleString("zh-CN")],
    ["工具结果", stats.toolResults.toLocaleString("zh-CN")],
    ["总计", stats.totalMessages.toLocaleString("zh-CN")],
  ] : [];
  const tokenRows: Array<[string, string]> = stats ? [
    ["输入", stats.tokens.input.toLocaleString("zh-CN")],
    ["输出", stats.tokens.output.toLocaleString("zh-CN")],
    ...(stats.tokens.cacheRead > 0
      ? [["缓存读取", stats.tokens.cacheRead.toLocaleString("zh-CN")] as [string, string]]
      : []),
    ...(stats.tokens.cacheWrite > 0
      ? [["缓存写入", stats.tokens.cacheWrite.toLocaleString("zh-CN")] as [string, string]]
      : []),
    ["总计", stats.tokens.total.toLocaleString("zh-CN")],
  ] : [];
  const usageRows: Array<[string, string]> = stats ? [
    ...(stats.cost > 0 ? [["费用", `$${stats.cost.toFixed(4)}`] as [string, string]] : []),
    ...(context?.contextWindow
      ? [[
          "上下文",
          `${context.percent === null ? "?" : `${context.percent.toFixed(1)}%`} / ${compactNumber(context.contextWindow)}`,
        ] as [string, string]]
      : []),
  ] : [];

  return (
    <div className="pi-session-stats-popover" ref={panelRef} role="dialog" aria-label="会话统计">
      <div className="pi-stats-header">
        <h2>会话</h2>
        <button className="pi-icon-button" type="button" aria-label="关闭" onClick={props.onClose}>
          <X size={16} />
        </button>
      </div>
      {!stats ? (
        <div className="pi-stats-loading">正在读取会话统计…</div>
      ) : (
        <>
          {stats.sessionName && <div className="pi-stats-name">{stats.sessionName}</div>}
          <div className="pi-stats-identifiers">
            <CopyableValue label="文件" value={stats.sessionFile ?? "内存中"} />
            <CopyableValue label="ID" value={stats.sessionId} />
          </div>
          <div className="pi-stats-grid">
            <StatSection title="消息" rows={messageRows} compact />
            <StatSection title="Token" rows={tokenRows} compact />
            {usageRows.length > 0 && <StatSection title="使用量" rows={usageRows} compact />}
          </div>
        </>
      )}
    </div>
  );
}
