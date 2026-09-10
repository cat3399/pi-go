import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { FileText, LoaderCircle, Plus, RotateCw, Square, SquareTerminal } from "lucide-react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import type { ApplicationClient } from "../contracts";
import { SelectMenu } from "../primitives/SelectMenu";
import { SidePanel } from "./SidePanel";

interface TerminalInfo {
  id: string;
  title: string;
  cwd: string;
  state: "running" | "exited";
  exitCode?: number;
  error?: string;
  logPath: string;
}

interface TerminalResult {
  terminal?: TerminalInfo;
  terminals?: TerminalInfo[];
  data?: string;
  start: number;
  cursor: number;
  truncated: boolean;
  hasMore: boolean;
}

interface TerminalPanelProps {
  client: ApplicationClient;
  sessionId: string;
  selectedId: string | null;
  readOnly: boolean;
  onSelect(id: string): void;
  onClose(): void;
  onPreviewFile(path: string): void;
}

function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export function TerminalPanel(props: TerminalPanelProps) {
  const { client, sessionId, selectedId, readOnly, onSelect } = props;
  const [terminals, setTerminals] = useState<TerminalInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const selected = terminals.find((terminal) => terminal.id === selectedId);

  useEffect(() => {
    generation.current += 1;
    return () => { generation.current += 1; };
  }, [client, sessionId]);

  const dispatch = useCallback((command: Record<string, unknown>) => (
    client.dispatch<TerminalResult>(sessionId, { type: "terminal", ...command })
  ), [client, sessionId]);

  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    const refresh = async () => {
      try {
        const result = await dispatch({ action: "list" });
        if (cancelled) return;
        const list = result.terminals ?? [];
        setTerminals(list);
        if (!selectedId && list.length > 0) {
          const active = list.filter((terminal) => terminal.state === "running").at(-1) ?? list.at(-1);
          if (active) onSelect(active.id);
        }
      } catch (error) {
        if (!cancelled) setError(errorText(error));
      } finally {
        if (!cancelled) {
          setLoading(false);
          timer = setTimeout(() => void refresh(), 2500);
        }
      }
    };
    void refresh();
    return () => { cancelled = true; clearTimeout(timer); };
  }, [dispatch, onSelect, selectedId]);

  const create = async () => {
    if (readOnly) return;
    const currentGeneration = generation.current;
    setPending(true);
    setError("");
    try {
      const result = await dispatch({ action: "open", waitMs: 0 });
      if (currentGeneration !== generation.current) return;
      if (result.terminal) {
        setTerminals((current) => [...current.filter((terminal) => terminal.id !== result.terminal!.id), result.terminal!]);
        onSelect(result.terminal.id);
      }
    } catch (error) {
      setError(errorText(error));
    } finally {
      setPending(false);
    }
  };

  const stop = async () => {
    if (!selectedId || readOnly) return;
    const currentGeneration = generation.current;
    setPending(true);
    setError("");
    try {
      const result = await dispatch({ action: "close", terminalId: selectedId });
      if (currentGeneration !== generation.current) return;
      if (result.terminal) updateTerminal(result.terminal);
    } catch (error) {
      setError(errorText(error));
    } finally {
      setPending(false);
    }
  };

  const updateTerminal = useCallback((info: TerminalInfo) => {
    setTerminals((current) => current.some((terminal) => terminal.id === info.id)
      ? current.map((terminal) => terminal.id === info.id ? info : terminal)
      : [...current, info]);
  }, []);

  return (
    <SidePanel
      label="终端"
      title={terminals.length > 1 ? (
        <SelectMenu
          ariaLabel="切换终端"
          value={selectedId ?? ""}
          placeholder="终端"
          options={terminals.map((terminal, index) => ({
            value: terminal.id,
            label: `${index + 1} · ${terminal.title}${terminal.state === "exited" ? " · 已退出" : ""}`,
          }))}
          onChange={onSelect}
        />
      ) : selected?.title ?? terminals[0]?.title ?? "终端"}
      tooltip={selected?.id}
      icon={<SquareTerminal size={14} strokeWidth={1.8} />}
      closeLabel="关闭终端面板"
      onClose={props.onClose}
      actions={(
        <div className="pi-terminal-actions">
          <button className="pi-icon-button" type="button" aria-label="新建终端" title="新建终端" disabled={pending || readOnly} onClick={() => void create()}><Plus size={15} /></button>
          <button className="pi-icon-button" type="button" aria-label="停止终端" title="停止终端" disabled={pending || readOnly || selected?.state !== "running"} onClick={() => void stop()}><Square size={13} /></button>
        </div>
      )}
    >
      <div className="pi-terminal-panel">
        {error && <div className="pi-terminal-error" role="alert">{error}</div>}
        {selectedId ? (
          <TerminalViewport key={selectedId} id={selectedId} readOnly={readOnly} dispatch={dispatch} onInfo={updateTerminal} onPreviewFile={props.onPreviewFile} />
        ) : (
          <div className="pi-terminal-empty">
            {loading ? <LoaderCircle className="pi-spin" size={22} /> : <><SquareTerminal size={30} strokeWidth={1.2} /><button type="button" className="pi-secondary-button" disabled={pending || readOnly} onClick={() => void create()}><Plus size={14} />新建终端</button></>}
          </div>
        )}
      </div>
    </SidePanel>
  );
}

function TerminalViewport(props: {
  id: string;
  readOnly: boolean;
  dispatch(command: Record<string, unknown>): Promise<TerminalResult>;
  onInfo(info: TerminalInfo): void;
  onPreviewFile(path: string): void;
}) {
  const { id, readOnly, dispatch, onInfo } = props;
  const host = useRef<HTMLDivElement>(null);
  const emulator = useRef<Terminal | null>(null);
  const [info, setInfo] = useState<TerminalInfo | null>(null);
  const [error, setError] = useState("");
  const [truncated, setTruncated] = useState(false);
  const [connection, setConnection] = useState(0);
  const inputDisabled = readOnly || info?.state === "exited";
  const inputDisabledRef = useRef(inputDisabled);

  useLayoutEffect(() => {
    inputDisabledRef.current = inputDisabled;
    if (emulator.current) {
      emulator.current.options.disableStdin = inputDisabled;
      if (inputDisabled) emulator.current.blur();
    }
  }, [inputDisabled]);

  useEffect(() => {
    if (!host.current) return;
    let disposed = false;
    let cursor = 0;
    let inputs = Promise.resolve();
    let resizeTimer: ReturnType<typeof setTimeout>;
    let finishWrite: (() => void) | undefined;
    const colors = getComputedStyle(host.current);
    const terminal = new Terminal({
      fontFamily: '"SFMono-Regular", Consolas, "Liberation Mono", monospace',
      fontSize: 12,
      lineHeight: 1.35,
      cursorBlink: true,
      cursorStyle: "bar",
      disableStdin: inputDisabledRef.current,
      scrollback: 5000,
      theme: {
        background: colors.getPropertyValue("--pi-bg").trim(),
        foreground: colors.getPropertyValue("--pi-text").trim(),
        cursor: colors.getPropertyValue("--pi-text").trim(),
        selectionBackground: "#b8d9ff80",
        black: "#24292f", red: "#c73635", green: "#347d39", yellow: "#946b00",
        blue: "#0969da", magenta: "#8250df", cyan: "#1b7c83", white: "#6e7781",
        brightBlack: "#6e7781", brightRed: "#cf222e", brightGreen: "#1a7f37", brightYellow: "#9a6700",
        brightBlue: "#218bff", brightMagenta: "#a475f9", brightCyan: "#3192aa", brightWhite: "#8c959f",
      },
    });
    const fit = new FitAddon();
    terminal.loadAddon(fit);
    terminal.open(host.current);
    emulator.current = terminal;
    fit.fit();
    if (!inputDisabledRef.current) terminal.focus();
    setError("");
    setTruncated(false);

    const report = (error: unknown) => { if (!disposed) setError(errorText(error)); };
    const resize = () => {
      if (disposed) return;
      fit.fit();
      clearTimeout(resizeTimer);
      resizeTimer = setTimeout(() => {
        void dispatch({ action: "resize", terminalId: id, cols: terminal.cols, rows: terminal.rows }).catch(report);
      }, 100);
    };
    const observer = new ResizeObserver(resize);
    observer.observe(host.current);
    const input = terminal.onData((value) => {
      if (inputDisabledRef.current) return;
      if (new TextEncoder().encode(value).length > 64 * 1024) { setError("输入超过 64 KiB"); return; }
      inputs = inputs.then(async () => {
        if (!disposed && !inputDisabledRef.current) await dispatch({ action: "write", terminalId: id, input: value, waitMs: 0 });
      }).catch(report);
    });

    const read = async () => {
      try {
        while (!disposed) {
          const result = await dispatch({ action: "read", terminalId: id, after: cursor, waitMs: 1000, mode: "follow" });
          if (disposed) break;
          cursor = result.cursor;
          if (result.truncated) setTruncated(true);
          if (result.data) {
            const data = Uint8Array.from(atob(result.data), (character) => character.charCodeAt(0));
            // Await rendering to bound the number of bytes queued in xterm.
            await new Promise<void>((resolve) => {
              finishWrite = resolve;
              terminal.write(data, resolve);
            });
            finishWrite = undefined;
            if (disposed) break;
          }
          if (result.terminal) {
            setInfo(result.terminal);
            onInfo(result.terminal);
            if (result.terminal.state === "exited" && !result.hasMore) break;
          }
        }
      } catch (error) { report(error); }
    };
    void read();
    return () => {
      disposed = true;
      clearTimeout(resizeTimer);
      observer.disconnect();
      input.dispose();
      finishWrite?.();
      emulator.current = null;
      terminal.dispose();
    };
  }, [connection, dispatch, id, onInfo]);

  return (
    <div className="pi-terminal-view">
      <div ref={host} className={`pi-terminal-screen${inputDisabled ? " is-readonly" : ""}`} aria-label="交互终端" />
      <footer className="pi-terminal-status">
        <span className={`pi-terminal-state ${info?.state === "running" ? "is-running" : ""}`} />
        <span>{error || info?.error || (info?.state === "exited" ? `已退出${info.exitCode !== undefined ? ` · ${info.exitCode}` : ""}` : info ? "已连接" : "连接中…")}</span>
        {error && <button className="pi-icon-button" type="button" aria-label="重新连接终端" title="重新连接" onClick={() => setConnection((value) => value + 1)}><RotateCw size={13} /></button>}
        {truncated && info && <button className="pi-icon-button" type="button" aria-label="查看完整终端日志" title="查看完整日志" onClick={() => props.onPreviewFile(info.logPath)}><FileText size={13} /></button>}
      </footer>
    </div>
  );
}
