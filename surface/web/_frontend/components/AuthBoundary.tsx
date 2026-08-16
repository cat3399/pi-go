"use client";

import { FormEvent, ReactNode, useCallback, useEffect, useRef, useState } from "react";
import styles from "./AuthBoundary.module.css";

type AuthStatus = {
  authRequired: boolean;
  authenticated: boolean;
  error?: string;
  retryAfterMs?: number;
};

async function sha256Hex(value: string): Promise<string> {
  if (!window.crypto?.subtle) throw new Error("This browser cannot hash the access password");
  const digest = await window.crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  return Array.from(new Uint8Array(digest))
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
}

async function readAuthStatus(): Promise<AuthStatus> {
  const response = await fetch("/api/v1/auth/status", {
    cache: "no-store",
    credentials: "same-origin",
    headers: { Accept: "application/json" },
  });
  const body = await response.json().catch(() => null) as AuthStatus | null;
  if (!response.ok || !body) throw new Error(body?.error || `HTTP ${response.status}`);
  return body;
}

export function AuthBoundary({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<"checking" | "login" | "ready" | "error">("checking");
  const [error, setError] = useState("");
  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const passwordRef = useRef<HTMLInputElement>(null);

  const check = useCallback(async () => {
    setStatus("checking");
    setError("");
    try {
      const auth = await readAuthStatus();
      if (!auth.authRequired || auth.authenticated) {
        setStatus("ready");
      } else {
        setStatus("login");
      }
    } catch (statusError) {
      setError(statusError instanceof Error ? statusError.message : String(statusError));
      setStatus("error");
    }
  }, []);

  useEffect(() => {
    void check();
  }, [check]);
  useEffect(() => {
    if (status === "login") passwordRef.current?.focus();
  }, [status]);

  const login = async (event: FormEvent) => {
    event.preventDefault();
    if (!password || submitting) return;
    setSubmitting(true);
    setError("");
    try {
      const response = await fetch("/api/v1/auth/login", {
        method: "POST",
        cache: "no-store",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify({ passwordHash: await sha256Hex(password) }),
      });
      const body = await response.json().catch(() => null) as AuthStatus | null;
      if (!response.ok || !body?.authenticated) {
        throw new Error(body?.error || `HTTP ${response.status}`);
      }
      setPassword("");
      setStatus("ready");
    } catch (loginError) {
      setError(loginError instanceof Error ? loginError.message : String(loginError));
      setStatus("login");
    } finally {
      setSubmitting(false);
    }
  };

  if (status === "ready") return children;

  return (
    <main className={styles.entry} aria-live="polite">
      <section className={styles.panel}>
        <h1>pi</h1>
        {status === "checking" && <p className={styles.status}>正在检查访问权限…</p>}
        {status === "error" && (
          <>
            <p className={styles.status}>无法检查访问权限。</p>
            <p className={styles.error}>{error}</p>
            <button className={styles.button} type="button" onClick={() => void check()}>重试</button>
          </>
        )}
        {status === "login" && (
          <>
            <p className={styles.status}>请输入访问密码。</p>
            <form onSubmit={login} autoComplete="on">
              <label htmlFor="web-access-password">访问密码</label>
              <input
                ref={passwordRef}
                id="web-access-password"
                name="password"
                type="password"
                autoComplete="current-password"
                placeholder="请输入密码"
                value={password}
                onChange={(event) => setPassword(event.target.value)}
              />
              <button className={styles.button} type="submit" disabled={!password || submitting}>
                {submitting ? "正在连接…" : "进入"}
              </button>
              <div className={styles.error}>{error}</div>
            </form>
          </>
        )}
      </section>
    </main>
  );
}
