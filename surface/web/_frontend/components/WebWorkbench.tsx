"use client";

import { useEffect, useMemo, useState } from "react";
import { normalizeRemoteEndpoint, PiWorkbench, type ApplicationClient } from "@cat3399/pi-workbench";

const version = process.env.NEXT_PUBLIC_PI_VERSION ?? "go-dev";
const isDev = process.env.NODE_ENV === "development";

export default function WebWorkbench() {
  const [endpoint, setEndpoint] = useState("");
  const [mockState, setMockState] = useState<{
    enabled: boolean;
    client?: ApplicationClient;
    hostKind: "web" | "desktop" | "mobile";
  }>({ enabled: false, hostKind: "web" });
  const sessionNavigation = useMemo(() => {
    let currentEndpoint = "";
    const read = () => new URL(window.location.href).searchParams.get("session")?.trim() || null;
    const endpoint = () => {
      try { return normalizeRemoteEndpoint(new URL(window.location.href).searchParams.get("node") || window.location.origin); }
      catch { return window.location.origin; }
    };
    const href = (id: string | null, source: string) => {
      const url = new URL(window.location.href);
      if (id) url.searchParams.set("session", id);
      else url.searchParams.delete("session");
      if (source !== window.location.origin) url.searchParams.set("node", source);
      else url.searchParams.delete("node");
      return `${url.pathname}${url.search}${url.hash}`;
    };
    return {
      read, href, endpoint,
      write(id: string | null, source: string) {
        currentEndpoint = source;
        const next = href(id, source);
        if (`${window.location.pathname}${window.location.search}${window.location.hash}` !== next) window.history.pushState(null, "", next);
      },
      subscribe(listener: () => void) {
        currentEndpoint ||= endpoint();
        const onPopState = () => {
          if (endpoint() !== currentEndpoint) window.location.reload();
          else listener();
        };
        window.addEventListener("popstate", onPopState);
        return () => window.removeEventListener("popstate", onPopState);
      },
    };
  }, []);

  useEffect(() => {
    setEndpoint(window.location.origin);
    if (!isDev) return;
    const params = new URLSearchParams(window.location.search);
    if (params.get("mock") !== "1") return;
    const requestedHost = params.get("host");
    const hostKind: "web" | "desktop" | "mobile" =
      requestedHost === "desktop" || requestedHost === "mobile" ? requestedHost : "web";
    let cancelled = false;
    setMockState({ enabled: true, hostKind });
    void import("../dev/mock-client").then(({ createMockApplicationClient }) => {
      if (cancelled) return;
      setMockState({ enabled: true, client: createMockApplicationClient(), hostKind });
    });
    return () => {
      cancelled = true;
    };
  }, []);

  if (!endpoint || (mockState.enabled && !mockState.client)) {
    return (
      <main className="pi-entry" aria-live="polite">
        <section className="pi-entry-panel">
          <h1>pi</h1>
          <p className="pi-entry-status">正在启动 Web 工作区…</p>
        </section>
      </main>
    );
  }

  if (mockState.enabled && mockState.client) {
    return (
      <PiWorkbench
        localClient={mockState.client}
        localAvailable
        version={`${version}-mock`}
        hostKind={mockState.hostKind}
        sessionNavigation={sessionNavigation}
      />
    );
  }

  return (
    <PiWorkbench
      localAvailable={false}
      defaultRemoteEndpoint={endpoint}
      version={version}
      hostKind="web"
      sessionNavigation={sessionNavigation}
    />
  );
}
