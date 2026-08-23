"use client";

import { useEffect, useState } from "react";
import { PiWorkbench } from "@cat3399/pi-workbench";

const version = process.env.NEXT_PUBLIC_PI_VERSION ?? "go-dev";

export default function WebWorkbench() {
  const [endpoint, setEndpoint] = useState("");

  useEffect(() => {
    setEndpoint(window.location.origin);
  }, []);

  if (!endpoint) {
    return (
      <main className="pi-entry" aria-live="polite">
        <section className="pi-entry-panel">
          <h1>pi</h1>
          <p className="pi-entry-status">正在启动 Web 工作区…</p>
        </section>
      </main>
    );
  }

  return (
    <PiWorkbench
      localAvailable={false}
      defaultRemoteEndpoint={endpoint}
      version={version}
      hostKind="web"
    />
  );
}
