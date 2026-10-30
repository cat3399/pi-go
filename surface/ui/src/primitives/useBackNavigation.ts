import { useEffect, useRef } from "react";
import { backNavigation } from "../back-navigation";

export function useBackNavigation(enabled: boolean, onBack: () => void, priority: number): void {
  const callback = useRef(onBack);
  callback.current = onBack;
  useEffect(() => {
    if (!enabled) return;
    return backNavigation.register(() => callback.current(), priority);
  }, [enabled, priority]);
}
