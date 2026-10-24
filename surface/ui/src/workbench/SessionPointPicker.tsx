import { useEffect, useRef, useState, type CSSProperties } from "react";
import { GitFork, Search, Waypoints, X } from "lucide-react";
import type { EntryNode, NodesPage, ReadResourceRequest } from "../contracts";
import { useDialogFocus } from "../primitives/useDialogFocus";

interface SessionPointPickerProps {
  mode: "tree" | "fork" | null;
  sessionId: string | null;
  loadNodes(query?: ReadResourceRequest): Promise<NodesPage>;
  activeLeafId: string | null;
  onClose(): void;
  onSelect(entryId: string): Promise<void>;
}

export function SessionPointPicker(props: SessionPointPickerProps) {
  const [query, setQuery] = useState("");
  const [submitting, setSubmitting] = useState("");
  const [nodes, setNodes] = useState<EntryNode[]>([]);
  const [next, setNext] = useState<string | undefined>();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const listRef = useRef<HTMLDivElement>(null);
  const loadingRef = useRef(false);
  const retryAtRef = useRef(0);
  const [retryKey, setRetryKey] = useState(0);
  const requestRef = useRef(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const dialogRef = useRef<HTMLElement>(null);
  const open = props.mode !== null;
  useDialogFocus(open, dialogRef, inputRef);
  useEffect(() => {
    const request = ++requestRef.current;
    if (!open || !props.sessionId) return;
    setLoading(true);
    setError("");
    setNodes([]);
    setNext(undefined);
    const timer = setTimeout(() => {
      void props.loadNodes({ search: query.trim(), ...(props.mode === "fork" ? { role: "user" } : { tree: true }) }).then((page) => {
        if (request !== requestRef.current) return;
        setNodes(page.items);
        setNext(page.next);
      }).catch((error) => { if (request === requestRef.current) setError(error instanceof Error ? error.message : String(error)); })
        .finally(() => { if (request === requestRef.current) setLoading(false); });
    }, query ? 150 : 0);
    return () => { clearTimeout(timer); requestRef.current++; };
  }, [open, props.sessionId, props.mode, props.loadNodes, query]);

  const loadMore = async () => {
    if (!next || loading || loadingRef.current) return;
    loadingRef.current = true;
    const request = requestRef.current;
    setLoading(true);
    try {
      const page = await props.loadNodes({ search: query.trim(), ...(props.mode === "fork" ? { role: "user" } : { tree: true }), cursor: next });
      if (request !== requestRef.current) return;
      setNodes((current) => [...current, ...page.items]);
      setNext(page.next);
    } catch (error) { retryAtRef.current = Date.now() + 2000; if (request === requestRef.current) setError(error instanceof Error ? error.message : String(error)); }
    finally { loadingRef.current = false; if (request === requestRef.current) setLoading(false); }
  };

  useEffect(() => {
    const list = listRef.current, last = list?.lastElementChild;
    if (!open || !next || loading || !list || !last) return;
    if (retryAtRef.current > Date.now()) {
      const timer = setTimeout(() => setRetryKey((value) => value + 1), retryAtRef.current - Date.now());
      return () => clearTimeout(timer);
    }
    const observer = new IntersectionObserver((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) void loadMore();
    }, { root: list, rootMargin: "160px" });
    observer.observe(last);
    return () => observer.disconnect();
  }, [open, next, loading, nodes.length, retryKey]);

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
    } catch (error) {
      setError(error instanceof Error ? error.message : String(error));
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
        <div ref={listRef} className="pi-command-picker-list" aria-busy={loading || undefined}>
          {error && <p role="alert">{error}</p>}
          {!loading && !error && nodes.length === 0 && <p>{fork ? "没有匹配的用户消息" : "没有匹配的会话节点"}</p>}
          {nodes.map((point, index) => (
            <button
              type="button"
              key={point.id}
              className={!fork && point.id === props.activeLeafId ? "is-current" : ""}
              style={{ "--pi-point-depth": `${(point.branchDepth ?? 0) * 12}px` } as CSSProperties}
              disabled={Boolean(submitting) || (!fork && point.id === props.activeLeafId)}
              onClick={() => void select(point.id)}
            >
              <span>{fork ? String(index + 1).padStart(2, "0") : (point.role || point.type).slice(0, 1).toLocaleUpperCase()}</span>
              <strong>{[point.label, point.preview].filter(Boolean).join(" — ") || point.type}</strong>
              <time>
                {!fork && point.id === props.activeLeafId
                  ? "当前"
                  : Boolean(point.role && point.timestamp)
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
