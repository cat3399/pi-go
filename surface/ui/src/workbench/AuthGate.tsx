import { FormEvent, useEffect, useRef, useState } from "react";
import { Settings } from "lucide-react";

interface AuthGateProps {
  title: string;
  error: string;
  onLogin(password: string): Promise<void>;
  onOpenSettings(): void;
}

export function AuthGate({ title, error, onLogin, onOpenSettings }: AuthGateProps) {
  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => inputRef.current?.focus(), []);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!password || submitting) return;
    setSubmitting(true);
    try {
      await onLogin(password);
      setPassword("");
    } catch {
      // The controller owns the user-facing error state.
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <main className="pi-entry" aria-live="polite">
      <button
        className="pi-floating-settings"
        type="button"
        aria-label="打开设置"
        onClick={onOpenSettings}
      >
        <Settings size={17} strokeWidth={1.8} />
      </button>
      <section className="pi-entry-panel">
        <h1>pi</h1>
        <p className="pi-entry-status">{title}</p>
        <form onSubmit={submit} autoComplete="on">
          <label htmlFor="pi-password">访问密码</label>
          <input
            ref={inputRef}
            id="pi-password"
            name="password"
            type="password"
            autoComplete="current-password"
            placeholder="请输入密码"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
          />
          <button type="submit" disabled={!password || submitting}>
            {submitting ? "正在连接…" : "进入"}
          </button>
          <div className="pi-entry-error">{error}</div>
        </form>
      </section>
    </main>
  );
}
