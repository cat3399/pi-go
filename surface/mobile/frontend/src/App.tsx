import { useEffect } from "react";
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

function useMobileViewportHeight() {
  useEffect(() => {
    const root = document.documentElement;
    const viewport = window.visualViewport;
    const update = () => {
      root.style.setProperty("--pi-mobile-viewport-height", `${Math.round(viewport?.height ?? window.innerHeight)}px`);
    };

    update();
    window.addEventListener("resize", update);
    viewport?.addEventListener("resize", update);
    viewport?.addEventListener("scroll", update);
    return () => {
      window.removeEventListener("resize", update);
      viewport?.removeEventListener("resize", update);
      viewport?.removeEventListener("scroll", update);
      root.style.removeProperty("--pi-mobile-viewport-height");
    };
  }, []);
}

export default function App() {
  useMobileViewportHeight();

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
