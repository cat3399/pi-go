// Host updates are independent of ApplicationClient and the selected remote Core.
export interface HostUpdateOptions { version: string; force: boolean }

export interface HostUpdatePlan {
  id: string;
  current_version: string;
  target_version: string;
  surface: string;
  os: string;
  arch: string;
  action: "none" | "upgrade" | "downgrade" | "reinstall";
  update_available: boolean;
  can_update: boolean;
  reason?: string;
  asset: string;
}

export interface HostUpdateStatus {
  state: "idle" | "updated" | "download_opened";
  message: string;
}

export interface HostUpdater {
  kind: "replace" | "download";
  check(options: HostUpdateOptions): Promise<HostUpdatePlan>;
  apply(id: string): Promise<HostUpdateStatus>;
  status?(): Promise<HostUpdateStatus>;
}
