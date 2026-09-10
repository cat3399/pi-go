import { useEffect, useState } from "react";
import { FileText, LoaderCircle } from "lucide-react";
import type { FilePreview } from "../contracts";
import { MarkdownBody } from "../content/MarkdownBody";
import { SidePanel } from "./SidePanel";

interface FilePreviewPanelProps {
  path: string;
  previewFile(path: string): Promise<FilePreview>;
  onClose(): void;
}

type TextDisplayMode = "preview" | "source";

function normalizedPath(value: string): string {
  return value.replace(/\\/g, "/").replace(/\/+$/, "");
}

export function FilePreviewPanel({ path, previewFile, onClose }: FilePreviewPanelProps) {
  const [preview, setPreview] = useState<FilePreview | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [textMode, setTextMode] = useState<TextDisplayMode>("source");

  useEffect(() => {
    let cancelled = false;
    setPreview(null);
    setLoading(true);
    setError("");
    setTextMode("source");
    void previewFile(path)
      .then((result) => {
        if (cancelled) return;
        setPreview(result);
        if (result.kind === "text" && result.language === "markdown") {
          setTextMode("preview");
        }
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
  }, [path, previewFile]);

  const name = preview?.name || normalizedPath(path).split("/").pop() || path;
  const markdown = preview?.kind === "text" && preview.language === "markdown";

  return (
    <SidePanel
      label={`预览 ${name}`}
      title={name}
      tooltip={path}
      icon={<FileText size={14} strokeWidth={1.8} />}
      loading={loading}
      closeLabel="关闭文件预览"
      onClose={onClose}
      actions={markdown && (
          <button
            className="pi-file-preview-mode"
            type="button"
            onClick={() => setTextMode((mode) => mode === "preview" ? "source" : "preview")}
          >
            {textMode === "preview" ? "查看源代码" : "查看预览"}
          </button>
      )}
    >
      <div className="pi-file-preview-content">
        {loading ? (
          <div className="pi-file-preview-status" role="status">
            <LoaderCircle size={17} />
            <span>正在读取文件…</span>
          </div>
        ) : error ? (
          <div className="pi-file-preview-status is-error" role="alert">{error}</div>
        ) : preview?.kind === "text" ? (
          markdown && textMode === "preview" ? (
            <div className="pi-file-preview-markdown">
              <MarkdownBody>{preview.content ?? ""}</MarkdownBody>
            </div>
          ) : (
            <pre className="pi-file-preview-source"><code>{preview.content ?? ""}</code></pre>
          )
        ) : preview?.kind === "image" && preview.sourceUrl ? (
          <div className="pi-file-preview-media is-image">
            <img src={preview.sourceUrl} alt={preview.name} />
          </div>
        ) : preview?.kind === "audio" && preview.sourceUrl ? (
          <div className="pi-file-preview-media is-audio">
            <audio controls preload="metadata" src={preview.sourceUrl}>
              当前环境无法播放这个音频文件。
            </audio>
          </div>
        ) : preview?.kind === "pdf" && preview.sourceUrl ? (
          <iframe className="pi-file-preview-frame" src={preview.sourceUrl} title={`预览 ${preview.name}`} />
        ) : preview?.kind === "docx" && preview.content ? (
          <iframe
            className="pi-file-preview-frame"
            srcDoc={preview.content}
            sandbox=""
            title={`预览 ${preview.name}`}
          />
        ) : (
          <div className="pi-file-preview-status is-error" role="alert">无法预览这个文件</div>
        )}
      </div>
    </SidePanel>
  );
}
