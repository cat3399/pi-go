import { normalizeRemoteEndpoint } from "./http-client";

export interface RemoteConnection {
  name: string;
  endpoint: string;
}

export interface RemoteConnections {
  nodes: RemoteConnection[];
  selectedEndpoint: string;
}

const STORAGE_KEY = "pi.remote.connections";

export function readRemoteConnections(): RemoteConnections {
  const empty: RemoteConnections = { nodes: [], selectedEndpoint: "" };
  try {
    const stored = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? "null") as Partial<RemoteConnections> | null;
    if (!Array.isArray(stored?.nodes)) return empty;
    const nodes: RemoteConnection[] = [];
    for (const node of stored.nodes) {
      if (!node || typeof node.endpoint !== "string") continue;
      try {
        const endpoint = normalizeRemoteEndpoint(node.endpoint);
        if (nodes.some((value) => value.endpoint === endpoint)) continue;
        nodes.push({ endpoint, name: typeof node.name === "string" ? node.name.trim() || endpoint : endpoint });
      } catch {
        // One invalid saved address must not prevent opening the connection settings.
      }
    }
    return {
      nodes,
      selectedEndpoint: nodes.find((node) => node.endpoint === stored.selectedEndpoint)?.endpoint ?? "",
    };
  } catch {
    return empty;
  }
}

export function writeRemoteConnections(value: RemoteConnections): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(value));
  } catch {
    // The active connection and the in-memory list still work without storage.
  }
}
