import { type MouseEvent, useCallback, useRef, useState } from "react";

export type InputModality = "keyboard" | "pointer";

/**
 * Tracks the interaction that should own focus inside a composite control.
 * Pointer-opened menus keep focus stationary; keyboard-opened menus may move it.
 */
export function useInputModality(initial: InputModality = "pointer") {
  const [modality, setModality] = useState<InputModality>(initial);
  const modalityRef = useRef<InputModality>(initial);
  const updateModality = useCallback((next: InputModality) => {
    if (modalityRef.current === next) return;
    modalityRef.current = next;
    setModality(next);
  }, []);
  const markKeyboard = useCallback(() => {
    updateModality("keyboard");
  }, [updateModality]);
  const markPointer = useCallback(() => {
    updateModality("pointer");
  }, [updateModality]);
  const suppressPointerFocus = useCallback((event: MouseEvent<HTMLElement>) => {
    if (modalityRef.current === "pointer") event.preventDefault();
  }, []);

  return { modality, modalityRef, markKeyboard, markPointer, suppressPointerFocus };
}
