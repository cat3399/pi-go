import { PiWorkbench, RemoteApplicationClient } from "@cat3399/pi-workbench";
import type { ApplicationClient } from "@cat3399/pi-workbench";
import { WailsRemoteTransport } from "./wails-remote-transport";

const version = "0.1.0-dev";

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
  return (
    <PiWorkbench
      localAvailable={false}
      version={version}
      hostKind="mobile"
      createRemoteClient={createRemoteClient}
      onEdgeGesturesEnabledChange={setAndroidEdgeGesturesEnabled}
    />
  );
}
