import { useCallback, useEffect, useState, type CSSProperties } from "react";
import {
  AtSign,
  ChevronRight,
  Eye,
  File,
  Folder,
  FolderOpen,
  RefreshCw,
} from "lucide-react";
import type { FileEntry, FileList } from "../contracts";

interface FileTreeProps {
  cwd: string;
  activePreviewPath: string;
  listFiles(path: string): Promise<FileList>;
  onMention(value: string): void;
  onPreview(path: string): void;
}

interface FileNode extends FileEntry {
  path: string;
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
  onMention(value: string): void;
  onPreview(path: string): void;
}) {
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [children, setChildren] = useState<FileNode[]>([]);
  const [error, setError] = useState("");
  const [hovered, setHovered] = useState(false);
  const selected = props.selectedPath === props.node.path || props.activePreviewPath === props.node.path;

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
    props.onSelect(props.node.path);
    if (!props.node.isDir) return;
    const next = !open;
    setOpen(next);
    if (next && !loaded) void loadChildren();
  };

  const actionsVisible = hovered || selected;
  const Icon = props.node.isDir ? (open ? FolderOpen : Folder) : File;

  return (
    <div
      className="pi-file-node"
      role="treeitem"
      aria-expanded={props.node.isDir ? open : undefined}
      aria-selected={selected}
    >
      <div
        className={`pi-file-row ${selected ? "is-selected" : ""}`}
        style={{ "--pi-file-indent": `${8 + props.depth * 14}px` } as CSSProperties}
        onPointerEnter={(event) => {
          if (event.pointerType === "mouse") setHovered(true);
        }}
        onPointerLeave={(event) => {
          if (event.pointerType === "mouse") setHovered(false);
        }}
      >
        <button
          className="pi-file-row-main"
          type="button"
          title={props.node.path}
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
        <div className={`pi-file-actions ${actionsVisible ? "is-visible" : ""}`}>
          {!props.node.isDir && (
            <button
              type="button"
              title={`预览 ${props.node.name}`}
              aria-label={`预览 ${props.node.name}`}
              onClick={(event) => {
                event.stopPropagation();
                props.onPreview(props.node.path);
              }}
            >
              <Eye size={12} />
              <span>预览</span>
            </button>
          )}
          <button
            type="button"
            title={`在输入框中引用 ${props.node.name}`}
            aria-label={`引用 ${props.node.name}`}
            onClick={(event) => {
              event.stopPropagation();
              props.onMention(atMention(props.node.path, props.cwd, props.node.isDir));
            }}
          >
            <AtSign size={12} />
            <span>引用</span>
          </button>
        </div>
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

export function FileTree({ cwd, activePreviewPath, listFiles, onMention, onPreview }: FileTreeProps) {
  const [roots, setRoots] = useState<FileNode[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [selectedPath, setSelectedPath] = useState("");
  const [refreshKey, setRefreshKey] = useState(0);

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
  }, [cwd, listFiles, refreshKey]);

  return (
    <section className="pi-file-tree" aria-label="文件树">
      <header className="pi-file-tree-header">
        <span title={cwd}>{cwd ? normalizedPath(cwd).split("/").pop() || cwd : "未选择文件夹"}</span>
        <button type="button" aria-label="刷新文件树" disabled={!cwd || loading} onClick={() => setRefreshKey((value) => value + 1)}>
          <RefreshCw className={loading ? "pi-file-loading" : ""} size={13} />
        </button>
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
              key={`${refreshKey}:${node.path}`}
              node={node}
              depth={0}
              cwd={cwd}
              activePreviewPath={activePreviewPath}
              selectedPath={selectedPath}
              onSelect={setSelectedPath}
              listFiles={listFiles}
              onMention={onMention}
              onPreview={onPreview}
            />
          ))}
        </div>
      )}
    </section>
  );
}
