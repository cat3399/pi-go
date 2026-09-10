import type { ReactNode } from "react";
import { X } from "lucide-react";

interface SidePanelProps {
  label: string;
  title: ReactNode;
  icon: ReactNode;
  tooltip?: string;
  loading?: boolean;
  actions?: ReactNode;
  closeLabel: string;
  onClose(): void;
  children: ReactNode;
}

export function SidePanel(props: SidePanelProps) {
  return (
    <aside className="pi-side-panel" aria-label={props.label} aria-busy={props.loading}>
      <header className="pi-side-panel-tabs">
        <div className="pi-side-panel-tab" title={props.tooltip}>
          {props.icon}
          <span>{props.title}</span>
          <button type="button" aria-label={props.closeLabel} title="关闭" onClick={props.onClose}>
            <X size={14} />
          </button>
        </div>
        {props.actions}
      </header>
      {props.children}
    </aside>
  );
}
