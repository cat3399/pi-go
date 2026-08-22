const gestureOwnedSelector = [
  "[data-swipe-ignore]",
  "input[type='range']",
  "[role='slider']",
].join(", ");

export function blocksEdgeGestureStart(target: EventTarget | null): boolean {
  return target instanceof Element && target.closest(gestureOwnedSelector) !== null;
}

const textEditorSelector = [
  "input:not([type])",
  "input[type='text']",
  "input[type='search']",
  "input[type='email']",
  "input[type='url']",
  "input[type='tel']",
  "textarea",
  "[contenteditable]:not([contenteditable='false'])",
  "[role='textbox']",
].join(", ");

export function isTextSelectionInteraction(
  target: EventTarget | null,
  clientX: number,
  clientY: number,
): boolean {
  if (target instanceof Element && target.closest(textEditorSelector)) return true;
  const selection = window.getSelection();
  if (!selection || selection.isCollapsed) return false;

  // A selection elsewhere on the page must not disable both edge gestures.
  // Only leave the pointer to the browser when it is on the selected text (or
  // one of the nearby native drag handles).
  const margin = 12;
  for (let index = 0; index < selection.rangeCount; index += 1) {
    const range = selection.getRangeAt(index);
    for (const rect of range.getClientRects()) {
      if (
        clientX >= rect.left - margin
        && clientX <= rect.right + margin
        && clientY >= rect.top - margin
        && clientY <= rect.bottom + margin
      ) return true;
    }
  }
  return false;
}
