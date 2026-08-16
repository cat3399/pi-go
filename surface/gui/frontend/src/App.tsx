import { useEffect, useMemo, useState } from "react";
import { PiWorkbench } from "@cat3399/pi-workbench";
import type { GUIHostInfo } from "./wails-client";
import { readGUIHostInfo, WailsApplicationClient } from "./wails-client";

export default function App() {
  const localClient = useMemo(() => new WailsApplicationClient(), []);
  const [host, setHost] = useState<GUIHostInfo | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    void readGUIHostInfo()
      .then(setHost)
      .catch((hostError) => {
        setError(hostError instanceof Error ? hostError.message : String(hostError));
      });
    return () => localClient.close();
  }, [localClient]);

  if (!host) {
    return (
      <main className="pi-entry" aria-live="polite">
        <section className="pi-entry-panel">
          <h1>pi</h1>
          <p className={error ? "pi-entry-error" : "pi-entry-status"}>
            {error || "正在启动内嵌 pi-go 核心…"}
          </p>
        </section>
      </main>
    );
  }

  return (
    <PiWorkbench
      localClient={localClient}
      localAvailable={host.localAvailable}
      localError={host.localError}
      defaultRemoteEndpoint={host.defaultRemoteEndpoint}
      version={host.version}
      hostKind="desktop"
    />
  );
}
