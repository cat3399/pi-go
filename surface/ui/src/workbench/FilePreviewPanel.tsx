import { useEffect, useState } from "react";
import { Download, FileText, LoaderCircle } from "lucide-react";
import type { FilePreview } from "../contracts";
import { MarkdownFilePreview } from "./MarkdownFilePreview";
import { SidePanel } from "./SidePanel";

interface FilePreviewPanelProps {
  path: string;
  previewFile(path: string): Promise<FilePreview>;
  downloadFile(path: string): Promise<void>;
  mobile: boolean;
  anchorsEnabled: boolean;
  onAnchorsAvailableChange(available: boolean): void;
  onClose(): void;
}

type TextDisplayMode = "preview" | "source";

function normalizedPath(value: string): string {
  return value.replace(/\\/g, "/").replace(/\/+$/, "");
}

export function FilePreviewPanel({ path, previewFile, downloadFile, mobile, anchorsEnabled, onAnchorsAvailableChange, onClose }: FilePreviewPanelProps) {
  const [preview, setPreview] = useState<FilePreview | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [downloading, setDownloading] = useState(false);
  const [downloadError, setDownloadError] = useState("");
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
  const download = async () => {
    if (downloading) return;
    setDownloading(true);
    setDownloadError("");
    try { await downloadFile(path); }
    catch (error) { setDownloadError(error instanceof Error ? error.message : String(error)); }
    finally { setDownloading(false); }
  };

  return (
    <SidePanel
      label={`预览 ${name}`}
      title={name}
      tooltip={path}
      icon={<FileText size={14} strokeWidth={1.8} />}
      loading={loading}
      closeLabel="关闭文件预览"
      onClose={onClose}
      actions={<>
        {markdown && (
          <button
            className="pi-file-preview-mode"
            type="button"
            onClick={() => setTextMode((mode) => mode === "preview" ? "source" : "preview")}
          >
            {textMode === "preview" ? "查看源代码" : "查看预览"}
          </button>
        )}
        <button className="pi-file-preview-mode" type="button" disabled={downloading} onClick={() => void download()}
          aria-label={downloading ? "正在下载文件" : "下载文件"} title={downloading ? "正在下载…" : "下载文件"}>
          {downloading ? <LoaderCircle className="pi-spin" size={15} /> : <Download size={15} />}
        </button>
      </>}
    >
      <div className="pi-file-preview-body">
        {downloadError && <div className="pi-inline-error" role="alert">下载失败：{downloadError}</div>}
        {!loading && !error && markdown && textMode === "preview" ? (
          <MarkdownFilePreview content={preview.content ?? ""} mobile={mobile} anchorsEnabled={anchorsEnabled}
            onAnchorsAvailableChange={onAnchorsAvailableChange} />
        ) : (
          <div className="pi-file-preview-content">
            {loading ? (
              <div className="pi-file-preview-status" role="status">
                <LoaderCircle size={17} />
                <span>正在读取文件…</span>
              </div>
            ) : error ? (
              <div className="pi-file-preview-status is-error" role="alert">{error}</div>
            ) : preview?.kind === "text" ? (
              <pre className="pi-file-preview-source"><code>{preview.content ?? ""}</code></pre>
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
        )}
      </div>
    </SidePanel>
  );
}
