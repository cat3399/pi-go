export type StreamingInputBehavior = "steer" | "follow_up";

export const DEFAULT_STREAMING_INPUT_BEHAVIOR: StreamingInputBehavior = "steer";

const STORAGE_KEY = "pi.streaming.input-behavior";

export function readStreamingInputBehavior(): StreamingInputBehavior {
  if (typeof window === "undefined") return DEFAULT_STREAMING_INPUT_BEHAVIOR;
  try {
    const value = window.localStorage.getItem(STORAGE_KEY);
    return value === "follow_up" ? "follow_up" : DEFAULT_STREAMING_INPUT_BEHAVIOR;
  } catch {
    return DEFAULT_STREAMING_INPUT_BEHAVIOR;
  }
}

export function writeStreamingInputBehavior(value: StreamingInputBehavior): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(STORAGE_KEY, value);
  } catch {
    // Persistence is optional; the current surface still updates immediately.
  }
}
