import { Suspense } from "react";
import { AppShell } from "@/components/AppShell";
import { I18nProvider } from "@/hooks/useI18n";
import { AuthBoundary } from "@/components/AuthBoundary";

export default function Home() {
  return (
    <Suspense>
      <AuthBoundary>
        <I18nProvider>
          <AppShell />
        </I18nProvider>
      </AuthBoundary>
    </Suspense>
  );
}
