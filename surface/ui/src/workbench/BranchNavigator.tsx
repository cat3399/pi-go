import { useEffect, useMemo, useRef, useState } from "react";
import type { AgentMessage, SessionTreeEntry, SessionTreeNode } from "../contracts";
import { messageText } from "./message";

interface BranchNavigatorProps {
  tree: SessionTreeNode[];
  activeLeafId: string | null;
  disabled?: boolean;
  onLeafChange(leafId: string): Promise<void>;
}

function buildActivePath(nodes: SessionTreeNode[], targetId: string | null): Set<string> {
  if (!targetId) return new Set();

  const search = (values: SessionTreeNode[], path: string[]): string[] | null => {
    for (const node of values) {
      const next = [...path, node.entry.id];
      if (node.entry.id === targetId || node.compressedEntryIds?.includes(targetId)) return next;
      const found = search(node.children, next);
      if (found) return found;
    }
    return null;
  };

  return new Set(search(nodes, []) ?? []);
}

function compress(node: SessionTreeNode): { node: SessionTreeNode; skipped: number } {
  let current = node;
  let skipped = current.compressedEntryIds?.length ?? 0;
  while (current.children.length === 1) {
    const child = current.children[0];
    if (!child) break;
    current = child;
    skipped += 1 + (current.compressedEntryIds?.length ?? 0);
  }
  return { node: current, skipped };
}

function entryMessage(entry: SessionTreeEntry): AgentMessage | null {
  return entry.type === "message" && entry.message && typeof entry.message === "object"
    ? entry.message
    : null;
}

function nodeLabel(node: SessionTreeNode): string {
  if (node.label?.trim()) return node.label.trim();
  const message = entryMessage(node.entry);
  if (message) {
    const text = messageText(message).replace(/\s+/g, " ").trim();
    if (text) return text.length > 40 ? `${text.slice(0, 40)}…` : text;
    if (message.role === "assistant") return "[assistant]";
  }
  return node.entry.type;
}

function hasBranch(nodes: SessionTreeNode[]): boolean {
  return nodes.some((node) => node.children.length > 1 || hasBranch(node.children));
}

interface TreeNodeViewProps {
  node: SessionTreeNode;
  activePathIds: Set<string>;
  isLast: boolean;
  parentLines: boolean[];
  onSelect(id: string): void;
}

function TreeNodeView(props: TreeNodeViewProps) {
  const compressed = compress(props.node);
  const representative = compressed.node;
  const active = props.activePathIds.has(representative.entry.id);
  const onPath = props.activePathIds.has(props.node.entry.id) || active;
  const role = entryMessage(representative.entry)?.role;

  return (
    <div className="pi-branch-node">
      <button type="button" className="pi-branch-row" onClick={() => props.onSelect(representative.entry.id)}>
        {props.parentLines.map((line, index) => (
          <span className={`pi-branch-indent ${line ? "has-line" : ""}`} key={index} />
        ))}
        <span className={`pi-branch-connector ${props.isLast ? "is-last" : ""}`} />
        <span className={`pi-branch-dot ${active ? "is-active" : onPath ? "is-path" : ""}`} />
        {(role === "user" || role === "assistant") && (
          <span className={`pi-branch-role is-${role}`}>{role === "user" ? "U" : "A"}</span>
        )}
        {compressed.skipped > 0 && <span className="pi-branch-skipped">+{compressed.skipped}</span>}
        <span className={`pi-branch-label ${active ? "is-active" : onPath ? "is-path" : ""}`}>
          {nodeLabel(representative)}
        </span>
      </button>
      {representative.children.map((child, index) => (
        <TreeNodeView
          key={child.entry.id}
          node={child}
          activePathIds={props.activePathIds}
          isLast={index === representative.children.length - 1}
          parentLines={[...props.parentLines, !props.isLast]}
          onSelect={props.onSelect}
        />
      ))}
    </div>
  );
}

export function BranchNavigator(props: BranchNavigatorProps) {
  const [open, setOpen] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const activePathIds = useMemo(
    () => buildActivePath(props.tree, props.activeLeafId),
    [props.activeLeafId, props.tree],
  );
  const branched = hasBranch(props.tree);
  const compressedRoot = props.tree.length === 1 && props.tree[0] ? compress(props.tree[0]).node : null;
  const visibleRoots = compressedRoot && compressedRoot.children.length > 1
    ? compressedRoot.children
    : props.tree;

  useEffect(() => {
    if (!open) return;
    const close = (event: MouseEvent) => {
      if (!containerRef.current?.contains(event.target as Node)) setOpen(false);
    };
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [open]);

  const select = (id: string) => {
    setOpen(false);
    void props.onLeafChange(id);
  };

  return (
    <div className="pi-branch-navigator" ref={containerRef}>
      <button
        className="pi-icon-button"
        type="button"
        title="分支"
        aria-label="分支"
        aria-expanded={open}
        disabled={props.disabled}
        onClick={() => setOpen((value) => !value)}
      >
        <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <line x1="6" y1="3" x2="6" y2="15" />
          <circle cx="18" cy="6" r="3" />
          <circle cx="6" cy="18" r="3" />
          <path d="M18 9a9 9 0 0 1-9 9" />
        </svg>
      </button>
      {open && (
        <div className="pi-branch-popover">
          {branched ? (
            <div className="pi-branch-tree">
              {visibleRoots.map((node, index) => (
                <TreeNodeView
                  key={node.entry.id}
                  node={node}
                  activePathIds={activePathIds}
                  isLast={index === visibleRoots.length - 1}
                  parentLines={[]}
                  onSelect={select}
                />
              ))}
            </div>
          ) : (
            <div className="pi-branch-empty">当前会话还没有分支</div>
          )}
        </div>
      )}
    </div>
  );
}
