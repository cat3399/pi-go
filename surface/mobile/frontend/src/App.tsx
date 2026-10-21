import { PiWorkbench, RemoteApplicationClient } from "@cat3399/pi-workbench";
import type { ApplicationClient } from "@cat3399/pi-workbench";
import { useEffect, useState } from "react";
import { WailsRemoteTransport } from "./wails-remote-transport";
import { mobileUpdater, readMobileHostInfo } from "./host-update";
import type { MobileHostInfo } from "./host-update";

function createRemoteClient(endpoint: string): ApplicationClient {
  return new RemoteApplicationClient(endpoint, new WailsRemoteTransport());
}

function setAndroidEdgeGesturesEnabled(enabled: boolean): void {
  const bridge = (window as Window & {
    wails?: { setEdgeGesturesEnabled?(value: boolean): void };
  }).wails;
  bridge?.setEdgeGesturesEnabled?.(enabled);
}

export default function App() {
  const [host, setHost] = useState<MobileHostInfo | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    void readMobileHostInfo().then(setHost).catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)));
  }, []);
  if (!host) return <main className="pi-entry"><p role="status">{error || "正在读取应用信息…"}</p></main>;
  return (
    <PiWorkbench
      localAvailable={false}
      version={host.version}
      hostUpdater={mobileUpdater}
      hostKind="mobile"
      createRemoteClient={createRemoteClient}
      onEdgeGesturesEnabledChange={setAndroidEdgeGesturesEnabled}
    />
  );
}
