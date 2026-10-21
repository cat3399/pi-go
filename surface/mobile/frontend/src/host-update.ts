import { Call } from "@wailsio/runtime";
import type { HostUpdater, HostUpdatePlan } from "@cat3399/pi-workbench";

export interface MobileHostInfo { version: string; updateError?: string }
export const readMobileHostInfo = () => Call.ByName("main.UpdateBridge.HostInfo") as Promise<MobileHostInfo>;

export const mobileUpdater: HostUpdater = {
  kind: "download",
  check: () => Call.ByName("main.UpdateBridge.Check") as Promise<HostUpdatePlan>,
  apply: async (id) => {
    await Call.ByName("main.UpdateBridge.Download", id);
    return { state: "download_opened", message: "已打开 APK 下载链接。" };
  },
};
