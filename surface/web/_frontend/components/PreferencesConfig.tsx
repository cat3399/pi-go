"use client";

import { useEffect } from "react";
import type { StreamingInputBehavior } from "@cat3399/pi-workbench";
import { useI18n } from "@/hooks/useI18n";
import { useIsMobile } from "@/hooks/useIsMobile";

interface PreferencesConfigProps {
  streamingInputBehavior: StreamingInputBehavior;
  onStreamingInputBehaviorChange(value: StreamingInputBehavior): void;
  onClose(): void;
}

export function PreferencesConfig({
  streamingInputBehavior,
  onStreamingInputBehaviorChange,
  onClose,
}: PreferencesConfigProps) {
  const { t } = useI18n();
  const isMobile = useIsMobile();

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [onClose]);

  const choices: Array<{
    value: StreamingInputBehavior;
    label: string;
    description: string;
    icon: React.ReactNode;
  }> = [
    {
      value: "steer",
      label: t("settings.steer"),
      description: t("settings.steerDescription"),
      icon: (
        <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M9 18 15 12 9 6" />
          <path d="M15 12H3" />
        </svg>
      ),
    },
    {
      value: "follow_up",
      label: t("settings.followUp"),
      description: t("settings.followUpDescription"),
      icon: (
        <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M12 3v12" />
          <path d="m7 10 5 5 5-5" />
          <path d="M5 21h14" />
        </svg>
      ),
    },
  ];

  return (
    <div
      role="presentation"
      style={{
        position: "fixed",
        inset: 0,
        zIndex: 1000,
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        padding: 8,
        background: "rgba(0,0,0,0.35)",
      }}
      onClick={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
    >
      <section
        role="dialog"
        aria-modal="true"
        aria-labelledby="preferences-title"
        style={{
          width: isMobile ? "calc(100vw - 16px)" : 560,
          maxWidth: "calc(100vw - 16px)",
          maxHeight: "calc(100dvh - 16px)",
          overflow: "hidden",
          border: "1px solid var(--border)",
          borderRadius: 10,
          background: "var(--bg)",
          boxShadow: "0 8px 32px rgba(0,0,0,0.18)",
        }}
      >
        <header style={{ display: "flex", alignItems: "center", justifyContent: "space-between", padding: "12px 18px", borderBottom: "1px solid var(--border)" }}>
          <strong id="preferences-title" style={{ color: "var(--text)", fontSize: 15 }}>{t("common.settings")}</strong>
          <button
            type="button"
            aria-label={t("settings.close")}
            onClick={onClose}
            style={{ padding: "2px 6px", border: 0, background: "none", color: "var(--text-muted)", cursor: "pointer", fontSize: 20, lineHeight: 1 }}
          >
            ×
          </button>
        </header>
        <div style={{ maxHeight: "calc(100dvh - 72px)", padding: isMobile ? 18 : 24, overflowY: "auto" }}>
          <h2 style={{ margin: 0, color: "var(--text)", fontSize: 15 }}>{t("settings.runningInputTitle")}</h2>
          <p style={{ margin: "6px 0 16px", color: "var(--text-muted)", fontSize: 12, lineHeight: 1.6 }}>{t("settings.runningInputDescription")}</p>
          <div role="radiogroup" aria-label={t("settings.runningInputTitle")} style={{ overflow: "hidden", border: "1px solid var(--border)", borderRadius: 10 }}>
            {choices.map((choice, index) => {
              const selected = streamingInputBehavior === choice.value;
              return (
                <button
                  key={choice.value}
                  type="button"
                  role="radio"
                  aria-checked={selected}
                  onClick={() => onStreamingInputBehaviorChange(choice.value)}
                  style={{
                    display: "grid",
                    gridTemplateColumns: "28px minmax(0, 1fr) 12px",
                    alignItems: "center",
                    gap: 12,
                    width: "100%",
                    minHeight: 72,
                    padding: "12px 14px",
                    border: 0,
                    borderTop: index === 0 ? 0 : "1px solid var(--border)",
                    background: selected ? "var(--bg-selected)" : "transparent",
                    color: "var(--text)",
                    textAlign: "left",
                    cursor: "pointer",
                  }}
                >
                  <span style={{ display: "grid", placeItems: "center", width: 28, height: 28, border: "1px solid var(--border)", borderRadius: 8, color: "var(--text-muted)" }}>{choice.icon}</span>
                  <span>
                    <strong style={{ display: "block", fontSize: 13 }}>{choice.label}</strong>
                    <small style={{ display: "block", marginTop: 3, color: "var(--text-muted)", fontSize: 12, lineHeight: 1.5 }}>{choice.description}</small>
                  </span>
                  <span aria-hidden="true" style={{ width: 10, height: 10, border: `1px solid ${selected ? "var(--text)" : "var(--border)"}`, borderRadius: "50%", background: selected ? "var(--text)" : "transparent" }} />
                </button>
              );
            })}
          </div>
          <p style={{ margin: "12px 2px 0", color: "var(--text-muted)", fontSize: 12, lineHeight: 1.6 }}>{t("settings.singleActionHint")}</p>
        </div>
      </section>
    </div>
  );
}
