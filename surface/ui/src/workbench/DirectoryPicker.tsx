import { FormEvent, useCallback, useEffect, useState } from "react";
import { createPortal } from "react-dom";
import { ChevronUp, Folder, HardDrive, X } from "lucide-react";
import type { DirectoryView } from "../contracts";

interface DirectoryPickerProps {
  initialPath: string;
  load(path?: string): Promise<DirectoryView>;
  onCancel(): void;
  onSelect(path: string): Promise<void>;
}

function isWindowsDriveRoot(directory: string): boolean {
  return /^[a-zA-Z]:[\\/]?$/.test(directory);
}

export function DirectoryPicker({
  initialPath,
  load,
  onCancel,
  onSelect,
}: DirectoryPickerProps) {
  const [currentPath, setCurrentPath] = useState("");
  const [parentPath, setParentPath] = useState<string | null>(null);
  const [pathInput, setPathInput] = useState(initialPath);
  const [directories, setDirectories] = useState<DirectoryView["directories"]>([]);
  const [drives, setDrives] = useState<DirectoryView["drives"]>();
  const [loading, setLoading] = useState(true);
  const [selecting, setSelecting] = useState(false);
  const [error, setError] = useState("");

  const navigate = useCallback(async (path?: string) => {
    setLoading(true);
    setError("");
    try {
      const value = await load(path);
      const resolved = value.path || path || "/";
      setCurrentPath(resolved);
      setParentPath(value.parentPath ?? null);
      setPathInput(resolved);
      setDirectories(value.directories ?? []);
      setDrives(value.drives);
    } catch (loadError) {
      setError(loadError instanceof Error ? loadError.message : String(loadError));
    } finally {
      setLoading(false);
    }
  }, [load]);

  useEffect(() => {
    void navigate(initialPath || undefined);
  }, [initialPath, navigate]);

  useEffect(() => {
    const keydown = (event: KeyboardEvent) => {
      if (event.key === "Escape" && !selecting) onCancel();
    };
    document.addEventListener("keydown", keydown);
    return () => document.removeEventListener("keydown", keydown);
  }, [onCancel, selecting]);

  const submitPath = (event: FormEvent) => {
    event.preventDefault();
    const candidate = pathInput.trim();
    if (candidate) void navigate(candidate);
  };

  const choose = async () => {
    if (!currentPath || pathInput.trim() !== currentPath) return;
    setSelecting(true);
    setError("");
    try {
      await onSelect(currentPath);
    } catch (selectError) {
      setError(selectError instanceof Error ? selectError.message : String(selectError));
      setSelecting(false);
    }
  };

  const canNavigateUp = Boolean(parentPath) || isWindowsDriveRoot(currentPath);
  const canSelect = Boolean(currentPath) && pathInput.trim() === currentPath && !selecting;

  return createPortal(
    <div
      className="pi-directory-backdrop"
      role="dialog"
      aria-modal="true"
      aria-label="选择工作目录"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget && !selecting) onCancel();
      }}
    >
      <section className="pi-directory-picker">
        <header>
          <h2>选择工作目录</h2>
          <button type="button" aria-label="关闭" disabled={selecting} onClick={onCancel}>
            <X size={17} />
          </button>
        </header>
        <form onSubmit={submitPath}>
          <button
            type="button"
            aria-label="上一级目录"
            disabled={loading || !canNavigateUp}
            onClick={() => void navigate(parentPath ?? undefined)}
          >
            <ChevronUp size={17} />
          </button>
          <input
            value={pathInput}
            aria-label="目录路径"
            placeholder="/path/to/project or ~/project"
            autoFocus
            autoComplete="off"
            spellCheck={false}
            onChange={(event) => {
              setPathInput(event.target.value);
              setError("");
            }}
          />
          <button type="submit" disabled={loading || !pathInput.trim()}>前往</button>
        </form>
        <div className="pi-directory-list">
          {loading ? (
            <p>正在读取目录…</p>
          ) : drives ? (
            drives.map((drive) => (
              <button type="button" key={drive.path} title={drive.path} onClick={() => void navigate(drive.path)}>
                <HardDrive size={15} />
                <span>{drive.name}</span>
              </button>
            ))
          ) : directories.length > 0 ? (
            directories.map((directory) => (
              <button
                type="button"
                key={directory.path}
                title={directory.path}
                onClick={() => void navigate(directory.path)}
              >
                <Folder size={15} />
                <span>{directory.name}</span>
              </button>
            ))
          ) : (
            <p>没有子目录</p>
          )}
          {error && <p className="pi-directory-error">{error}</p>}
        </div>
        <footer>
          <button type="button" disabled={selecting} onClick={onCancel}>取消</button>
          <button type="button" disabled={!canSelect} onClick={() => void choose()}>
            {selecting ? "正在选择…" : "选择此文件夹"}
          </button>
        </footer>
      </section>
    </div>,
    document.body,
  );
}
