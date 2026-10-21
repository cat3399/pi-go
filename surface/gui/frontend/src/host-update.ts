import { Call } from "@wailsio/runtime";
import type { HostUpdater, HostUpdatePlan, HostUpdateStatus } from "@cat3399/pi-workbench";

interface UpdateResult { updated: boolean; target_version: string }
function status(result: UpdateResult): HostUpdateStatus {
  return result.updated
    ? { state: "updated", message: `已更新到 ${result.target_version}。请退出并重新打开应用。` }
    : { state: "idle", message: "" };
}

export const guiUpdater: HostUpdater = {
  kind: "replace",
  check: (options) => Call.ByName("main.UpdateBridge.Check", options) as Promise<HostUpdatePlan>,
  apply: async (id) => status(await Call.ByName("main.UpdateBridge.Apply", id) as UpdateResult),
  status: async () => status(await Call.ByName("main.UpdateBridge.Status") as UpdateResult),
};
