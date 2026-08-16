import { MouseEvent, useEffect, useMemo, useRef, useState } from "react";
import { Check, ChevronDown, ChevronRight, Folder, Pencil, Settings, SquarePen, Trash2, X } from "lucide-react";
import type { SessionInfo } from "../contracts";

interface SidebarProps {
  open: boolean;
  sessions: SessionInfo[];
  runningSessionIds: string[];
  activeSessionId: string | null;
  onClose(): void;
  onNewSession(): void;
  onSelect(sessionId: string): void;
  onRename(sessionId: string, name: string): Promise<void>;
  onDelete(sessionId: string): Promise<void>;
  onOpenSettings(): void;
}

function sessionTitle(session: SessionInfo): string {
  const title = session.name?.trim() || session.firstMessage?.trim();
  if (title && title !== "(no messages)") return title;
  const parts = session.cwd.split(/[\\/]/).filter(Boolean);
  return parts[parts.length - 1] || "新会话";
}

function pathName(path: string): string {
  const parts = path.split(/[\\/]/).filter(Boolean);
  return parts[parts.length - 1] || path || "项目";
}

function SessionRow({
  session,
  active,
  onSelect,
  onRename,
  onDelete,
  running,
}: {
  session: SessionInfo;
  active: boolean;
  onSelect(): void;
  onRename(name: string): Promise<void>;
  onDelete(): Promise<void>;
  running: boolean;
}) {
  const [hovered, setHovered] = useState(false);
  const [renaming, setRenaming] = useState(false);
  const [renameValue, setRenameValue] = useState("");
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [working, setWorking] = useState(false);
  const [error, setError] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);

  const startRename = (event: MouseEvent) => {
    event.stopPropagation();
    setError("");
    setRenameValue(session.name?.trim() || session.firstMessage?.trim() || "");
    setRenaming(true);
    requestAnimationFrame(() => inputRef.current?.select());
  };

  const commitRename = async () => {
    const name = renameValue.trim();
    if (!name) {
      setRenaming(false);
      return;
    }
    setWorking(true);
    setError("");
    try {
      await onRename(name);
      setRenaming(false);
    } catch (renameError) {
      setError(renameError instanceof Error ? renameError.message : String(renameError));
    } finally {
      setWorking(false);
    }
  };

  const remove = async (event: MouseEvent) => {
    event.stopPropagation();
    setWorking(true);
    setError("");
    try {
      await onDelete();
    } catch (deleteError) {
      setError(deleteError instanceof Error ? deleteError.message : String(deleteError));
      setConfirmDelete(true);
    } finally {
      setWorking(false);
    }
  };

  if (confirmDelete) {
    return (
      <div className="pi-session-item pi-session-confirm" title={error || undefined}>
        <span>{error || "删除这个会话？"}</span>
        <button type="button" disabled={working} aria-label="确认删除" onClick={(event) => void remove(event)}>
          <Check size={14} />
        </button>
        <button
          type="button"
          aria-label="取消删除"
          onClick={(event) => {
            event.stopPropagation();
            setConfirmDelete(false);
            setError("");
          }}
        >
          <X size={14} />
        </button>
      </div>
    );
  }

  if (renaming) {
    return (
      <div className="pi-session-item pi-session-renaming">
        <input
          ref={inputRef}
          value={renameValue}
          disabled={working}
          aria-label="会话名称"
          onChange={(event) => setRenameValue(event.target.value)}
          onBlur={() => {
            if (!working) void commitRename();
          }}
          onKeyDown={(event) => {
            if (event.key === "Enter") void commitRename();
            if (event.key === "Escape") setRenaming(false);
          }}
        />
      </div>
    );
  }

  return (
    <div
      className={`pi-session-item ${active ? "is-active" : ""}`}
      role="button"
      tabIndex={0}
      title={sessionTitle(session)}
      onClick={onSelect}
      onKeyDown={(event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          onSelect();
        }
      }}
      onMouseEnter={() => setHovered(true)}
      onMouseLeave={() => setHovered(false)}
    >
      <span>{sessionTitle(session)}</span>
      {running && !hovered && <span className="pi-session-running-dot" aria-label="运行中" />}
      {hovered && (
        <span className="pi-session-actions">
          <button type="button" aria-label="重命名会话" onClick={startRename}>
            <Pencil size={13} />
          </button>
          <button
            type="button"
            aria-label="删除会话"
            onClick={(event) => {
              event.stopPropagation();
              setConfirmDelete(true);
            }}
          >
            <Trash2 size={13} />
          </button>
        </span>
      )}
    </div>
  );
}

interface ProjectGroup {
  root: string;
  name: string;
  modified: string;
  sessions: SessionInfo[];
}

function ProjectSessions(props: {
  project: ProjectGroup;
  activeSessionId: string | null;
  runningSessionIds: Set<string>;
  onSelect(sessionId: string): void;
  onRename(sessionId: string, name: string): Promise<void>;
  onDelete(sessionId: string): Promise<void>;
}) {
  const containsActive = props.project.sessions.some((session) => session.id === props.activeSessionId);
  const [open, setOpen] = useState(containsActive);

  useEffect(() => {
    if (containsActive) setOpen(true);
  }, [containsActive]);

  return (
    <div className={`pi-project ${open ? "is-open" : ""}`}>
      <button className="pi-project-row" type="button" title={props.project.root} onClick={() => setOpen((value) => !value)}>
        <Folder size={16} />
        <span>{props.project.name}</span>
        {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
      </button>
      {open && (
        <div className="pi-project-sessions">
          {props.project.sessions.map((session) => (
            <SessionRow
              key={session.id}
              session={session}
              active={session.id === props.activeSessionId}
              running={props.runningSessionIds.has(session.id)}
              onSelect={() => props.onSelect(session.id)}
              onRename={(name) => props.onRename(session.id, name)}
              onDelete={() => props.onDelete(session.id)}
            />
          ))}
        </div>
      )}
    </div>
  );
}

export function Sidebar(props: SidebarProps) {
  const [profileOpen, setProfileOpen] = useState(false);
  const profileRef = useRef<HTMLDivElement>(null);
  const projects = useMemo(() => {
    const groups = new Map<string, SessionInfo[]>();
    for (const session of props.sessions) {
      const root = session.projectRoot || session.cwd || "未归类";
      const values = groups.get(root) ?? [];
      values.push(session);
      groups.set(root, values);
    }
    return [...groups.entries()]
      .map(([root, sessions]): ProjectGroup => {
        const ordered = [...sessions].sort((left, right) => right.modified.localeCompare(left.modified));
        return {
          root,
          name: pathName(root),
          modified: ordered[0]?.modified ?? "",
          sessions: ordered,
        };
      })
      .sort((left, right) => right.modified.localeCompare(left.modified));
  }, [props.sessions]);
  const runningSessionIds = useMemo(() => new Set(props.runningSessionIds), [props.runningSessionIds]);

  useEffect(() => {
    if (!profileOpen) return;
    const close = (event: globalThis.MouseEvent) => {
      if (!profileRef.current?.contains(event.target as Node)) setProfileOpen(false);
    };
    document.addEventListener("mousedown", close);
    return () => document.removeEventListener("mousedown", close);
  }, [profileOpen]);

  return (
    <>
      <button
        className={`pi-sidebar-backdrop ${props.open ? "is-open" : ""}`}
        type="button"
        tabIndex={props.open ? 0 : -1}
        aria-label="关闭侧栏"
        onClick={props.onClose}
      />
      <aside id="pi-workbench-sidebar" className={`pi-sidebar ${props.open ? "is-open" : ""}`} aria-label="会话">
        <div className="pi-sidebar-titlebar" />
        <header className="pi-sidebar-header">
          <span className="pi-wordmark">pi</span>
          <button className="pi-sidebar-mobile-close" type="button" aria-label="关闭侧栏" onClick={props.onClose}>
            <X size={18} />
          </button>
        </header>
        <button className="pi-new-session" type="button" onClick={props.onNewSession}>
          <SquarePen size={16} />
          <span>新会话</span>
        </button>
        <nav className="pi-session-list">
          {projects.length > 0 && <div className="pi-sidebar-section-title">项目</div>}
          {projects.map((project) => (
            <ProjectSessions
              key={project.root}
              project={project}
              activeSessionId={props.activeSessionId}
              runningSessionIds={runningSessionIds}
              onSelect={props.onSelect}
              onRename={props.onRename}
              onDelete={props.onDelete}
            />
          ))}
        </nav>
        <div className="pi-sidebar-profile" ref={profileRef}>
          {profileOpen && (
            <div className="pi-profile-menu">
              <div className="pi-profile-menu-header">
                <span className="pi-sidebar-avatar">π</span>
                <strong>pi</strong>
              </div>
              <button
                type="button"
                onClick={() => {
                  setProfileOpen(false);
                  props.onOpenSettings();
                }}
              >
                <Settings size={16} />
                <span>设置</span>
              </button>
            </div>
          )}
          <button className="pi-sidebar-settings" type="button" onClick={() => setProfileOpen((value) => !value)}>
            <span className="pi-sidebar-avatar">π</span>
            <span className="pi-sidebar-identity"><strong>pi</strong></span>
          </button>
        </div>
      </aside>
    </>
  );
}
