import {
  useCallback,
  useEffect,
  useState,
  type CSSProperties,
} from "react";
import {
  ChevronRight,
  File,
  Folder,
  FolderOpen,
  RefreshCw,
} from "lucide-react";
import type { FileEntry, FileList } from "../contracts";
import { IconAction } from "../primitives/InlineActions";
import { InlineConfirmation } from "../primitives/InlineConfirmation";
import { FileContextMenu, useFileContextMenu } from "./FileContextMenu";

interface FileTreeProps {
  cwd: string;
  refreshKey?: number;
  activePreviewPath: string;
  listFiles(path: string): Promise<FileList>;
  deleteFile(path: string): Promise<void>;
  onMention(value: string): void;
  onPreview(path: string): void;
  onDeleted(path: string): void;
}

interface FileNode extends FileEntry {
  path: string;
}

interface FileDeleteControl {
  targetPath: string;
  working: boolean;
  error: string;
  request(node: FileNode): void;
  confirm(): void;
  cancel(): void;
}

function normalizedPath(value: string): string {
  return value.replace(/\\/g, "/").replace(/\/+$/, "");
}

function joinPath(parent: string, name: string): string {
  const separator = parent.includes("\\") && !parent.includes("/") ? "\\" : "/";
  return `${parent.replace(/[\\/]+$/, "")}${separator}${name}`;
}

function relativePath(path: string, cwd: string): string {
  const target = normalizedPath(path);
  const root = normalizedPath(cwd);
  return target === root ? "" : target.startsWith(`${root}/`) ? target.slice(root.length + 1) : target;
}

function atMention(path: string, cwd: string, isDir: boolean): string {
  const relative = relativePath(path, cwd);
  const value = isDir ? `${relative}/` : relative;
  return value.includes(" ") ? `@"${value}" ` : `@${value} `;
}

function fileDeleteErrorMessage(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error);
  const normalized = message.toLowerCase();
  if (normalized.includes("directory is not empty")) return "文件夹不是空的，未删除。";
  if (normalized.includes("workspace root")) return "工作区根目录不能删除。";
  if (normalized.includes("not found") || normalized.includes("no such file")) return "文件或文件夹已不存在。";
  if (normalized.includes("access denied") || normalized.includes("permission denied")) return "没有权限删除这个文件或文件夹。";
  return message;
}

function toNodes(parent: string, entries: FileEntry[]): FileNode[] {
  return entries.map((entry) => ({ ...entry, path: joinPath(parent, entry.name) }));
}

function TreeNode(props: {
  node: FileNode;
  depth: number;
  cwd: string;
  activePreviewPath: string;
  selectedPath: string;
  onSelect(path: string): void;
  listFiles(path: string): Promise<FileList>;
  deleteControl: FileDeleteControl;
  onMention(value: string): void;
  onPreview(path: string): void;
}) {
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [children, setChildren] = useState<FileNode[]>([]);
  const [error, setError] = useState("");
  const selected = props.selectedPath === props.node.path || props.activePreviewPath === props.node.path;
  const deleteOpen = props.deleteControl.targetPath === props.node.path;
  const menu = useFileContextMenu((action) => {
    props.onSelect(props.node.path);
    if (action === "preview") props.onPreview(props.node.path);
    if (action === "mention") props.onMention(atMention(props.node.path, props.cwd, props.node.isDir));
    if (action === "delete") props.deleteControl.request(props.node);
  });

  const loadChildren = useCallback(async () => {
    if (!props.node.isDir || loading) return;
    setLoading(true);
    setError("");
    try {
      const result = await props.listFiles(props.node.path);
      setChildren(toNodes(result.path || props.node.path, result.entries ?? []));
      setLoaded(true);
    } catch (loadError) {
      setError(loadError instanceof Error ? loadError.message : String(loadError));
    } finally {
      setLoading(false);
    }
  }, [loading, props.listFiles, props.node.isDir, props.node.path]);

  const activate = () => {
    menu.dismiss();
    props.onSelect(props.node.path);
    if (!props.node.isDir) return;
    const next = !open;
    setOpen(next);
    if (next && !loaded) void loadChildren();
  };

  const Icon = props.node.isDir ? (open ? FolderOpen : Folder) : File;

  return (
    <div
      className="pi-file-node"
      role="treeitem"
      aria-expanded={props.node.isDir ? open : undefined}
      aria-selected={selected}
    >
      <div
        className={`pi-file-row ${selected ? "is-selected" : ""} ${menu.open ? "is-menu-open" : ""} ${deleteOpen ? "is-confirming-delete" : ""}`}
        style={{ "--pi-file-indent": `${8 + props.depth * 14}px` } as CSSProperties}
      >
        {deleteOpen ? (
          <InlineConfirmation
            className="pi-file-delete-confirm"
            message={props.deleteControl.error || "确定删除？"}
            working={props.deleteControl.working}
            onConfirm={props.deleteControl.confirm}
            onCancel={props.deleteControl.cancel}
          />
        ) : (
          <>
            <button
              ref={menu.anchorRef}
              className="pi-file-row-main"
              type="button"
              title={props.node.path}
              aria-haspopup="menu"
              aria-expanded={menu.open}
              aria-keyshortcuts="Shift+F10"
              onPointerDown={menu.onPointerDown}
              onContextMenu={menu.onContextMenu}
              onKeyDown={menu.onTriggerKeyDown}
              onClickCapture={menu.onClickCapture}
              onClick={activate}
            >
              {props.node.isDir ? (
                <ChevronRight className={`pi-file-chevron ${open ? "is-open" : ""}`} size={12} />
              ) : (
                <span className="pi-file-chevron" />
              )}
              <Icon size={14} strokeWidth={1.7} />
              <span>{props.node.name}</span>
              {loading && <RefreshCw className="pi-file-loading" size={11} aria-label="正在读取" />}
            </button>
            <FileContextMenu name={props.node.name} isDir={props.node.isDir} menu={menu} />
          </>
        )}
      </div>
      {error && (
        <button className="pi-file-inline-error" type="button" onClick={() => void loadChildren()}>
          读取失败，点此重试
        </button>
      )}
      {props.node.isDir && open && (
        <div role="group">
          {loaded && children.length === 0 && <div className="pi-file-empty" style={{ paddingLeft: 34 + props.depth * 14 }}>空文件夹</div>}
          {children.map((child) => (
            <TreeNode
              key={child.path}
              {...props}
              node={child}
              depth={props.depth + 1}
            />
          ))}
        </div>
      )}
    </div>
  );
}

export function FileTree({
  cwd,
  refreshKey: externalRefreshKey = 0,
  activePreviewPath,
  listFiles,
  deleteFile,
  onMention,
  onPreview,
  onDeleted,
}: FileTreeProps) {
  const [roots, setRoots] = useState<FileNode[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [selectedPath, setSelectedPath] = useState("");
  const [refreshKey, setRefreshKey] = useState(0);
  const [deleteTarget, setDeleteTarget] = useState<FileNode | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState("");

  useEffect(() => {
    setDeleteTarget(null);
    setDeleteError("");
    setDeleting(false);
  }, [cwd]);

  const requestDelete = useCallback((node: FileNode) => {
    if (deleting) return;
    setDeleteTarget(node);
    setDeleteError("");
  }, [deleting]);

  const cancelDelete = useCallback(() => {
    setDeleteTarget(null);
    setDeleteError("");
  }, []);

  const confirmDelete = useCallback(async () => {
    if (!deleteTarget || deleting) return;
    setDeleting(true);
    setDeleteError("");
    try {
      await deleteFile(deleteTarget.path);
      const deletedPath = deleteTarget.path;
      setDeleteTarget(null);
      setSelectedPath((current) => current === deletedPath ? "" : current);
      onDeleted(deletedPath);
    } catch (deleteFailure) {
      setDeleteError(fileDeleteErrorMessage(deleteFailure));
    } finally {
      setDeleting(false);
    }
  }, [deleteFile, deleteTarget, deleting, onDeleted]);

  const deleteControl: FileDeleteControl = {
    targetPath: deleteTarget?.path ?? "",
    working: deleting,
    error: deleteError,
    request: requestDelete,
    confirm: () => void confirmDelete(),
    cancel: cancelDelete,
  };

  useEffect(() => {
    setSelectedPath("");
    if (!cwd) {
      setRoots([]);
      setError("");
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError("");
    void listFiles(cwd)
      .then((result) => {
        if (!cancelled) setRoots(toNodes(result.path || cwd, result.entries ?? []));
      })
      .catch((loadError) => {
        if (!cancelled) setError(loadError instanceof Error ? loadError.message : String(loadError));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [cwd, externalRefreshKey, listFiles, refreshKey]);

  return (
    <section className="pi-file-tree" aria-label="文件树">
      <header className="pi-file-tree-header">
        <span title={cwd}>{cwd ? normalizedPath(cwd).split("/").pop() || cwd : "未选择文件夹"}</span>
        <IconAction
          label="刷新文件树"
          disabled={!cwd || loading || deleting}
          onClick={() => {
            cancelDelete();
            setRefreshKey((value) => value + 1);
          }}
        >
          <RefreshCw className={loading ? "pi-file-loading" : ""} size={13} />
        </IconAction>
      </header>
      {error ? (
        <div className="pi-file-tree-error" role="alert">
          <span>{error}</span>
          <button type="button" onClick={() => setRefreshKey((value) => value + 1)}>重试</button>
        </div>
      ) : loading && roots.length === 0 ? (
        <div className="pi-file-tree-status" role="status"><RefreshCw className="pi-file-loading" size={13} /> 正在读取文件…</div>
      ) : roots.length === 0 ? (
        <div className="pi-file-tree-status">这个文件夹是空的</div>
      ) : (
        <div className="pi-file-tree-content" role="tree">
          {roots.map((node) => (
            <TreeNode
              key={`${externalRefreshKey}:${refreshKey}:${node.path}`}
              node={node}
              depth={0}
              cwd={cwd}
              activePreviewPath={activePreviewPath}
              selectedPath={selectedPath}
              onSelect={setSelectedPath}
              listFiles={listFiles}
              deleteControl={deleteControl}
              onMention={onMention}
              onPreview={onPreview}
            />
          ))}
        </div>
      )}
    </section>
  );
}
