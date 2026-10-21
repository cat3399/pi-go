import { useEffect, useState } from "react";
import type { HostUpdater, HostUpdatePlan, HostUpdateStatus } from "../host-update";

export function HostUpdatePanel({ updater }: { updater: HostUpdater }) {
  const downloadOnly = updater.kind === "download";
  const [version, setVersion] = useState("");
  const [force, setForce] = useState(false);
  const [plan, setPlan] = useState<HostUpdatePlan | null>(null);
  const [status, setStatus] = useState<HostUpdateStatus | null>(null);
  const [busy, setBusy] = useState<"check" | "apply" | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!updater.status) return;
    let active = true;
    void updater.status().then((value) => {
      if (active) setStatus(value);
    }).catch((err: unknown) => {
      if (active) setError(err instanceof Error ? err.message : String(err));
    });
    return () => { active = false; };
  }, [updater]);

  async function check() {
    setBusy("check"); setError(""); setPlan(null); setStatus(null);
    try { setPlan(await updater.check({ version: version.trim(), force })); }
    catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setBusy(null); }
  }

  async function apply() {
    if (!plan) return;
    setBusy("apply"); setError("");
    try { setStatus(await updater.apply(plan.id)); setPlan(null); }
    catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setBusy(null); }
  }

  const disabled = !!busy || status?.state === "updated";
  const action = downloadOnly ? "下载 APK" : plan?.action === "downgrade" ? "降级到" : plan?.action === "reinstall" ? "重新安装" : "更新到";
  return (
    <section className="pi-settings-content-section">
      <h2>应用更新</h2>
      <div className="pi-settings-card pi-host-update">
        {!downloadOnly && <details>
          <summary>指定版本或重新安装</summary>
          <label>目标版本
            <input value={version} placeholder="留空选择最新稳定版" disabled={disabled}
              onChange={(event) => { setVersion(event.target.value); setPlan(null); }} />
          </label>
          <label className="pi-host-update-force">
            <input type="checkbox" checked={force} disabled={disabled}
              onChange={(event) => { setForce(event.target.checked); setPlan(null); }} />
            重新安装相同版本
          </label>
        </details>}
        <div className="pi-host-update-actions">
          <button type="button" className="pi-secondary-button" disabled={disabled} onClick={() => void check()}>
            {busy === "check" ? "正在检查…" : "检查更新"}
          </button>
          {plan?.can_update && (downloadOnly || plan.update_available) && (
            <button type="button" className="pi-secondary-button" disabled={disabled} onClick={() => void apply()}>
              {busy === "apply" ? downloadOnly ? "正在打开下载…" : "正在下载并校验…" : `${action} ${plan.target_version}`}
            </button>
          )}
        </div>
        <div aria-live="polite">
          {plan && <p>当前版本 {plan.current_version} · 目标版本 {plan.target_version}</p>}
          {plan?.reason && <p className="pi-settings-error">{plan.reason}</p>}
          {!downloadOnly && plan?.can_update && !plan.update_available && <p>当前已是此平台的最新稳定版本，或比它更新。</p>}
          {status?.message && <p>{status.message}</p>}
          {error && <p className="pi-settings-error" role="alert">{error}</p>}
        </div>
      </div>
    </section>
  );
}
