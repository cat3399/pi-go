import { FormEvent, useEffect, useRef, useState } from "react";
import { ArrowLeft, Clock3, CornerDownRight, Info, MessageSquareText, Radio, Settings } from "lucide-react";
import type { ClientKind } from "../contracts";
import { useDialogFocus } from "../primitives/useDialogFocus";
import type { StreamingInputBehavior } from "../streaming-input-behavior";

interface SettingsDrawerProps {
  open: boolean;
  kind: ClientKind;
  endpoint: string;
  version: string;
  localAvailable: boolean;
  localError?: string;
  hostKind: "desktop" | "web" | "mobile";
  streamingInputBehavior: StreamingInputBehavior;
  onClose(): void;
  onUseLocal(): void;
  onUseRemote(endpoint: string): void;
  onStreamingInputBehaviorChange(value: StreamingInputBehavior): void;
}

export function SettingsDrawer(props: SettingsDrawerProps) {
  const [endpoint, setEndpoint] = useState(props.endpoint);
  const [error, setError] = useState("");
  const [page, setPage] = useState<"connection" | "input" | "about">("connection");
  const dialogRef = useRef<HTMLElement>(null);
  const backRef = useRef<HTMLButtonElement>(null);

  useDialogFocus(props.open, dialogRef, backRef);

  useEffect(() => setEndpoint(props.endpoint), [props.endpoint]);
  useEffect(() => {
    if (!props.open) setError("");
  }, [props.open]);

  const connectRemote = (event: FormEvent) => {
    event.preventDefault();
    try {
      props.onUseRemote(endpoint);
      setError("");
    } catch (connectError) {
      setError(connectError instanceof Error ? connectError.message : String(connectError));
    }
  };

  if (!props.open) return null;

  return (
    <section ref={dialogRef} className="pi-settings-page" role="dialog" aria-modal="true" aria-label="设置" tabIndex={-1}>
      <aside className="pi-settings-navigation">
        <button ref={backRef} className="pi-settings-back" type="button" onClick={props.onClose}>
          <ArrowLeft size={17} />
          <span>返回应用</span>
        </button>
        <div className="pi-settings-nav-group">
          <div className="pi-settings-nav-label">pi</div>
          <button
            type="button"
            className={page === "connection" ? "is-active" : ""}
            onClick={() => setPage("connection")}
          >
            <Radio size={17} />
            <span>连接</span>
          </button>
          <button
            type="button"
            className={page === "input" ? "is-active" : ""}
            onClick={() => setPage("input")}
          >
            <MessageSquareText size={17} />
            <span>输入</span>
          </button>
          <button
            type="button"
            className={page === "about" ? "is-active" : ""}
            onClick={() => setPage("about")}
          >
            <Info size={17} />
            <span>关于</span>
          </button>
        </div>
      </aside>

      <main className="pi-settings-content">
        {page === "connection" ? (
          <>
            <h1>连接</h1>
            <section className="pi-settings-content-section">
              <h2>Agent Core</h2>
              <div className="pi-settings-card">
                {props.hostKind !== "mobile" && (
                  <button
                    className={`pi-connection-row ${props.kind === "local" ? "is-selected" : ""}`}
                    type="button"
                    disabled={!props.localAvailable}
                    onClick={props.onUseLocal}
                  >
                    <span className="pi-settings-item-icon"><Settings size={17} /></span>
                    <span>
                      <strong>此设备</strong>
                      <small>使用桌面端内嵌的完整 pi-go Agent Core</small>
                    </span>
                    <span className="pi-selection-dot" aria-hidden="true" />
                  </button>
                )}
                {props.hostKind !== "mobile" && props.localError && <p className="pi-settings-error">{props.localError}</p>}
                <form className={`pi-remote-form ${props.hostKind === "mobile" ? "is-remote-only" : ""}`} onSubmit={connectRemote}>
                  <div>
                    <label htmlFor="pi-remote-endpoint">{props.hostKind === "mobile" ? "Agent Core 地址" : "远程桌面端"}</label>
                    <p>{props.hostKind === "mobile" ? "连接运行 pi-go Web API 的设备。" : "连接另一台 pi-go 桌面端，与 WebUI 使用同一套远程协议。"}</p>
                  </div>
                  <div className="pi-remote-controls">
                    <input
                      id="pi-remote-endpoint"
                      type="url"
                      inputMode="url"
                      placeholder={props.hostKind === "mobile" ? "https://pi.example.com" : "http://192.168.1.10:30141"}
                      value={endpoint}
                      onChange={(event) => setEndpoint(event.target.value)}
                    />
                    <button type="submit">连接</button>
                  </div>
                  {error && <p className="pi-settings-error">{error}</p>}
                </form>
              </div>
            </section>
          </>
        ) : page === "input" ? (
          <>
            <h1>输入</h1>
            <section className="pi-settings-content-section">
              <h2>任务运行时发送</h2>
              <div className="pi-settings-card" role="radiogroup" aria-label="任务运行时的消息处理方式">
                <button
                  className={`pi-settings-choice-row ${props.streamingInputBehavior === "steer" ? "is-selected" : ""}`}
                  type="button"
                  role="radio"
                  aria-checked={props.streamingInputBehavior === "steer"}
                  onClick={() => props.onStreamingInputBehaviorChange("steer")}
                >
                  <span className="pi-settings-item-icon"><CornerDownRight size={17} /></span>
                  <span>
                    <strong>插入当前运行</strong>
                    <small>在当前任务下一次模型调用前送入这条消息。</small>
                  </span>
                  <span className="pi-selection-dot" aria-hidden="true" />
                </button>
                <button
                  className={`pi-settings-choice-row ${props.streamingInputBehavior === "follow_up" ? "is-selected" : ""}`}
                  type="button"
                  role="radio"
                  aria-checked={props.streamingInputBehavior === "follow_up"}
                  onClick={() => props.onStreamingInputBehaviorChange("follow_up")}
                >
                  <span className="pi-settings-item-icon"><Clock3 size={17} /></span>
                  <span>
                    <strong>排到下一轮</strong>
                    <small>当前任务结束后，自动把这条消息作为下一轮输入。</small>
                  </span>
                  <span className="pi-selection-dot" aria-hidden="true" />
                </button>
              </div>
              <p className="pi-settings-footnote">任务运行时，输入框为空显示停止按钮；有文字时，同一位置切换为发送按钮。</p>
            </section>
          </>
        ) : (
          <>
            <h1>关于</h1>
            <section className="pi-settings-content-section">
              <h2>pi</h2>
              <div className="pi-settings-card">
                <div className="pi-settings-about-row">
                  <span>
                    <strong>{props.hostKind === "mobile" ? "pi Mobile" : "pi GUI"}</strong>
                    <small>{props.hostKind === "mobile" ? "连接远程 pi-go Agent Core 的移动端" : "内嵌完整 pi-go Agent Core 的独立桌面端"}</small>
                  </span>
                  <code>{props.version}</code>
                </div>
              </div>
            </section>
          </>
        )}
      </main>
    </section>
  );
}
