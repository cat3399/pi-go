import { KeyboardEvent, MouseEvent, useEffect, useMemo, useRef, useState } from "react";
import { Ellipsis, FileText, Folder, FolderOpen, MessageSquare, Pencil, Plus, Settings, SquarePen, Trash2, X } from "lucide-react";
import type { FileList, ProjectInfo, SessionInfo } from "../contracts";
import { AnchoredPopover } from "../primitives/AnchoredPopover";
import { IconAction, InlineActions } from "../primitives/InlineActions";
import { InlineConfirmation } from "../primitives/InlineConfirmation";
import { OverlayScrollbar } from "../primitives/OverlayScrollbar";
import { FileTree } from "./FileTree";

interface SidebarProps {
  open: boolean;
  section: "sessions" | "files";
  projects: ProjectInfo[];
  sessions: SessionInfo[];
  runningSessionIds: string[];
  activeSessionId: string | null;
  activePreviewPath: string;
  workingDirectory: string;
  fileRefreshKey: number;
  listFiles(path: string): Promise<FileList>;
  deleteFile(path: string): Promise<void>;
  onMentionFile(value: string): void;
  onPreviewFile(path: string): void;
  onFileDeleted(path: string): void;
  onSectionChange(section: "sessions" | "files"): void;
  onClose(): void;
  onAddProject(): void;
  onRemoveProject(path: string): Promise<void>;
  onNewSession(cwd?: string): void;
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

function projectKey(path: string): string {
  return path.replace(/\\/g, "/").replace(/\/+$/, "");
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
      <InlineConfirmation
        className="pi-session-item pi-session-confirm"
        message={error || "删除这个会话？"}
        working={working}
        onConfirm={(event) => void remove(event)}
        onCancel={() => {
          setConfirmDelete(false);
          setError("");
        }}
      />
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
      aria-current={active ? "true" : undefined}
      title={sessionTitle(session)}
      onClick={onSelect}
      onKeyDown={(event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          onSelect();
        }
      }}
    >
      <span>{sessionTitle(session)}</span>
      {running && <span className="pi-session-running-dot" aria-label="运行中" />}
      <InlineActions className="pi-sidebar-row-actions pi-session-actions">
        <IconAction label="重命名会话" onClick={startRename}>
          <Pencil size={15} />
        </IconAction>
        <IconAction
          label="删除会话"
          onClick={(event) => {
            event.stopPropagation();
            setConfirmDelete(true);
          }}
        >
          <Trash2 size={15} />
        </IconAction>
      </InlineActions>
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
  onNewSession(cwd: string): void;
  onRemoveProject(path: string): Promise<void>;
  onSelect(sessionId: string): void;
  onRename(sessionId: string, name: string): Promise<void>;
  onDelete(sessionId: string): Promise<void>;
}) {
  const containsActive = props.project.sessions.some((session) => session.id === props.activeSessionId);
  const [open, setOpen] = useState(containsActive);
  const [menuOpen, setMenuOpen] = useState(false);
  const [removing, setRemoving] = useState(false);
  const [removeError, setRemoveError] = useState("");
  const menuButtonRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (containsActive) setOpen(true);
  }, [containsActive]);

  const removeProject = async () => {
    setRemoving(true);
    setRemoveError("");
    try {
      await props.onRemoveProject(props.project.root);
    } catch (error) {
      setRemoveError(error instanceof Error ? error.message : String(error));
      setRemoving(false);
    }
  };

  return (
    <div className={`pi-project ${open ? "is-open" : ""}`}>
      <div className="pi-project-row">
        <button
          className="pi-project-toggle"
          type="button"
          title={props.project.root}
          aria-expanded={open}
          onClick={() => setOpen((value) => !value)}
        >
          {open ? <FolderOpen size={16} /> : <Folder size={16} />}
          <span>{props.project.name}</span>
        </button>
        <InlineActions className="pi-sidebar-row-actions pi-project-actions" visible={menuOpen}>
          <IconAction
            className="pi-project-new-session"
            label={`在 ${props.project.name} 中新建对话`}
            onClick={() => props.onNewSession(props.project.root)}
          >
            <SquarePen size={15} />
          </IconAction>
          <IconAction
            ref={menuButtonRef}
            className="pi-project-more"
            label={`${props.project.name} 项目菜单`}
            aria-haspopup="menu"
            aria-expanded={menuOpen}
            title={removeError || undefined}
            onClick={(event) => {
              event.stopPropagation();
              setMenuOpen((value) => !value);
            }}
          >
            <Ellipsis size={16} />
          </IconAction>
          <AnchoredPopover
            anchorRef={menuButtonRef}
            open={menuOpen}
            className="pi-project-menu"
            role="menu"
            minWidth={132}
            onDismiss={() => setMenuOpen(false)}
          >
            <button
              type="button"
              role="menuitem"
              disabled={removing}
              title="仅从项目列表移除，不会删除项目文件或历史会话"
              onClick={(event) => {
                event.stopPropagation();
                void removeProject();
              }}
            >
              <Trash2 size={14} />
              <span>{removing ? "正在删除…" : "删除项目"}</span>
            </button>
          </AnchoredPopover>
        </InlineActions>
      </div>
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
  const sidebarRef = useRef<HTMLElement>(null);
  const sessionListRef = useRef<HTMLElement>(null);
  const selectSection = (section: "sessions" | "files", focus = false) => {
    props.onSectionChange(section);
    if (focus) {
      requestAnimationFrame(() => document.getElementById(`pi-sidebar-${section}-tab`)?.focus());
    }
  };
  const onTabKeyDown = (event: KeyboardEvent, section: "sessions" | "files") => {
    let next: "sessions" | "files" | null = null;
    if (event.key === "ArrowLeft" || event.key === "ArrowRight") {
      next = section === "sessions" ? "files" : "sessions";
    } else if (event.key === "Home") {
      next = "sessions";
    } else if (event.key === "End") {
      next = "files";
    }
    if (!next) return;
    event.preventDefault();
    selectSection(next, true);
  };
  const projects = useMemo(() => {
    const groups = new Map<string, SessionInfo[]>();
    for (const session of props.sessions) {
      const root = session.projectRoot || session.cwd;
      const key = projectKey(root);
      const values = groups.get(key) ?? [];
      values.push(session);
      groups.set(key, values);
    }
    return (props.projects ?? [])
      .map((project): ProjectGroup => {
        const root = project.path;
        const sessions = groups.get(projectKey(root)) ?? [];
        const ordered = [...sessions].sort((left, right) => right.modified.localeCompare(left.modified));
        return {
          root,
          name: pathName(root),
          modified: project.modified,
          sessions: ordered,
        };
      })
      .sort((left, right) => right.modified.localeCompare(left.modified));
  }, [props.projects, props.sessions]);
  const runningSessionIds = useMemo(() => new Set(props.runningSessionIds), [props.runningSessionIds]);

  useEffect(() => {
    if (!props.open) return;
    const closeOutside = (event: PointerEvent) => {
      if (!sidebarRef.current?.closest(".pi-workbench.is-mobile") && window.innerWidth >= 800) return;
      const target = event.target instanceof Element ? event.target : null;
      if (target && (sidebarRef.current?.contains(target) || target.closest(".pi-sidebar-toggle"))) return;
      props.onClose();
    };
    document.addEventListener("pointerdown", closeOutside, true);
    return () => document.removeEventListener("pointerdown", closeOutside, true);
  }, [props.open, props.onClose]);

  return (
    <>
      <button
        className={`pi-sidebar-backdrop ${props.open ? "is-open" : ""}`}
        type="button"
        tabIndex={props.open ? 0 : -1}
        aria-label="关闭侧栏"
        onPointerDown={props.onClose}
        onClick={props.onClose}
      />
      <aside
        ref={sidebarRef}
        id="pi-workbench-sidebar"
        className={`pi-sidebar ${props.open ? "is-open" : ""}`}
        aria-label="会话"
        aria-hidden={!props.open}
        inert={!props.open}
      >
        <div className="pi-sidebar-titlebar" />
        <header className="pi-sidebar-header">
          <span className="pi-wordmark">pi</span>
          <button className="pi-sidebar-mobile-close" type="button" aria-label="关闭侧栏" onClick={props.onClose}>
            <X size={18} />
          </button>
        </header>
        <div className="pi-sidebar-primary">
          <button className="pi-new-session" type="button" onClick={() => props.onNewSession()}>
            <SquarePen size={16} />
            <span>新会话</span>
          </button>
          <div className="pi-sidebar-tabs" role="tablist" aria-label="侧栏内容">
            <button
              id="pi-sidebar-sessions-tab"
              type="button"
              role="tab"
              aria-selected={props.section === "sessions"}
              aria-controls="pi-sidebar-panel"
              tabIndex={props.section === "sessions" ? 0 : -1}
              className={props.section === "sessions" ? "is-active" : ""}
              onClick={() => selectSection("sessions")}
              onKeyDown={(event) => onTabKeyDown(event, "sessions")}
            >
              <MessageSquare size={13} />
              会话
            </button>
            <button
              id="pi-sidebar-files-tab"
              type="button"
              role="tab"
              aria-selected={props.section === "files"}
              aria-controls="pi-sidebar-panel"
              tabIndex={props.section === "files" ? 0 : -1}
              className={props.section === "files" ? "is-active" : ""}
              onClick={() => selectSection("files")}
              onKeyDown={(event) => onTabKeyDown(event, "files")}
            >
              <FileText size={13} />
              文件
            </button>
          </div>
        </div>
        <div
          id="pi-sidebar-panel"
          className="pi-session-scroll pi-overlay-scroll-host"
          role="tabpanel"
          aria-labelledby={`pi-sidebar-${props.section}-tab`}
        >
          {props.section === "sessions" ? (
            <>
              <nav ref={sessionListRef} className="pi-session-list pi-overlay-scroll-viewport">
                <div className="pi-sidebar-section-heading">
                  <span>项目</span>
                  <IconAction label="添加项目" onClick={props.onAddProject}>
                    <Plus size={16} />
                  </IconAction>
                </div>
                {projects.map((project) => (
                  <ProjectSessions
                    key={project.root}
                    project={project}
                    activeSessionId={props.activeSessionId}
                    runningSessionIds={runningSessionIds}
                    onNewSession={props.onNewSession}
                    onRemoveProject={props.onRemoveProject}
                    onSelect={props.onSelect}
                    onRename={props.onRename}
                    onDelete={props.onDelete}
                  />
                ))}
              </nav>
              <OverlayScrollbar viewportRef={sessionListRef} />
            </>
          ) : (
            <FileTree
              cwd={props.workingDirectory}
              refreshKey={props.fileRefreshKey}
              activePreviewPath={props.activePreviewPath}
              listFiles={props.listFiles}
              deleteFile={props.deleteFile}
              onMention={props.onMentionFile}
              onPreview={props.onPreviewFile}
              onDeleted={props.onFileDeleted}
            />
          )}
        </div>
        <button className="pi-sidebar-settings" type="button" onClick={props.onOpenSettings}>
          <Settings size={16} />
          <span>设置</span>
        </button>
      </aside>
    </>
  );
}
