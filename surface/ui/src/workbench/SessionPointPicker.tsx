import { useEffect, useMemo, useRef, useState, type CSSProperties } from "react";
import { GitFork, Search, Waypoints, X } from "lucide-react";
import type { AgentMessage, SessionTreeNode } from "../contracts";
import { useDialogFocus } from "../primitives/useDialogFocus";
import { messageText } from "./message";

interface SessionPointPickerProps {
  mode: "tree" | "fork" | null;
  messages: AgentMessage[];
  entryIds: string[];
  tree: SessionTreeNode[];
  activeLeafId: string | null;
  onClose(): void;
  onSelect(entryId: string): Promise<void>;
}

interface SessionPoint {
  id: string;
  label: string;
  timestamp: number | null;
  role: string;
  depth: number;
  current: boolean;
}

function messageTimestamp(message: AgentMessage): number | null {
  if (typeof message.timestamp === "number") {
    return message.timestamp < 10_000_000_000 ? message.timestamp * 1000 : message.timestamp;
  }
  if (typeof message.timestamp === "string") {
    const parsed = Date.parse(message.timestamp);
    return Number.isNaN(parsed) ? null : parsed;
  }
  return null;
}

function treePoints(nodes: SessionTreeNode[], activeLeafId: string | null): SessionPoint[] {
  const points: SessionPoint[] = [];
  const append = (values: SessionTreeNode[], branchDepth: number) => {
    for (const node of values) {
      const message = node.entry.message;
      const preview = message ? messageText(message).replace(/\s+/g, " ").trim() : "";
      const label = [node.label?.trim(), preview].filter(Boolean).join(" — ") || node.entry.type;
      points.push({
        id: node.entry.id,
        label,
        timestamp: message ? messageTimestamp(message) : null,
        role: message?.role || node.entry.type,
        depth: branchDepth,
        current: node.entry.id === activeLeafId,
      });
      append(node.children, node.children.length > 1 ? branchDepth + 1 : branchDepth);
    }
  };
  append(nodes, 0);
  return points;
}

export function SessionPointPicker(props: SessionPointPickerProps) {
  const [query, setQuery] = useState("");
  const [submitting, setSubmitting] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);
  const dialogRef = useRef<HTMLElement>(null);
  const open = props.mode !== null;
  useDialogFocus(open, dialogRef, inputRef);
  const points = useMemo(() => props.mode === "tree"
    ? treePoints(props.tree, props.activeLeafId)
    : props.messages.flatMap((message, index): SessionPoint[] => {
        const id = props.entryIds[index];
        if (!id || message.role !== "user") return [];
        const label = messageText(message).replace(/\s+/g, " ").trim();
        if (!label) return [];
        return [{
          id,
          label,
          timestamp: messageTimestamp(message),
          role: "user",
          depth: 0,
          current: false,
        }];
      }), [props.activeLeafId, props.entryIds, props.messages, props.mode, props.tree]);
  const filtered = useMemo(() => {
    const value = query.trim().toLocaleLowerCase();
    if (!value) return points;
    return points.filter((point) => (
      point.label.toLocaleLowerCase().includes(value)
      || point.id.toLocaleLowerCase().includes(value)
      || point.role.toLocaleLowerCase().includes(value)
    ));
  }, [points, query]);

  useEffect(() => {
    if (!open) {
      setQuery("");
      setSubmitting("");
      return;
    }
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") props.onClose();
    };
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open, props.onClose]);

  if (!props.mode) return null;
  const fork = props.mode === "fork";
  const Icon = fork ? GitFork : Waypoints;

  const select = async (entryId: string) => {
    if (submitting) return;
    setSubmitting(entryId);
    try {
      await props.onSelect(entryId);
      props.onClose();
    } finally {
      setSubmitting("");
    }
  };

  return (
    <div className="pi-command-picker-backdrop" role="presentation" onMouseDown={(event) => {
      if (event.target === event.currentTarget) props.onClose();
    }}>
      <section ref={dialogRef} className="pi-command-picker" role="dialog" aria-modal="true" aria-label={fork ? "选择分支起点" : "选择会话位置"} tabIndex={-1}>
        <header>
          <span><Icon size={15} />{fork ? "从消息创建分支" : "定位到会话位置"}</span>
          <button type="button" aria-label="关闭" onClick={props.onClose}><X size={15} /></button>
        </header>
        <label className="pi-command-picker-search">
          <Search size={14} />
          <input
            ref={inputRef}
            value={query}
            placeholder={fork ? "搜索用户消息" : "搜索会话节点"}
            onChange={(event) => setQuery(event.target.value)}
          />
        </label>
        <div className="pi-command-picker-list">
          {filtered.length === 0 ? <p>{fork ? "没有匹配的用户消息" : "没有匹配的会话节点"}</p> : filtered.map((point, index) => (
            <button
              type="button"
              key={point.id}
              className={point.current ? "is-current" : ""}
              style={{ "--pi-point-depth": `${point.depth * 12}px` } as CSSProperties}
              disabled={Boolean(submitting) || point.current}
              onClick={() => void select(point.id)}
            >
              <span>{fork ? String(index + 1).padStart(2, "0") : point.role.slice(0, 1).toLocaleUpperCase()}</span>
              <strong>{point.label}</strong>
              <time>
                {point.current
                  ? "当前"
                  : point.timestamp !== null
                    ? new Date(point.timestamp).toLocaleString("zh-CN", { hour: "2-digit", minute: "2-digit" })
                    : ""}
              </time>
            </button>
          ))}
        </div>
      </section>
    </div>
  );
}
