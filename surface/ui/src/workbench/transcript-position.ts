import type { ScrollPosition } from "./application-data";

export function captureTranscriptPosition(transcript: HTMLElement, bottom: boolean): ScrollPosition {
  const bounds = transcript.getBoundingClientRect();
  const element = [...transcript.querySelectorAll<HTMLElement>("[data-entry-id], [data-run-summary]")].find((item) => {
    const box = item.getBoundingClientRect();
    return box.bottom > bounds.top + 8 && box.top < bounds.bottom;
  });
  const part = element?.dataset.messagePart;
  return {
    entryId: element?.dataset.entryId ?? element?.dataset.runSummary ?? "",
    target: element?.dataset.runSummary ? "process" : undefined,
    messagePart: part === "process" || part === "message" ? part : undefined,
    offset: (element?.getBoundingClientRect().top ?? bounds.top) - bounds.top,
    bottom,
  };
}

export function transcriptPositionTarget(transcript: HTMLElement, position: ScrollPosition): HTMLElement | undefined {
  if (position.target === "process") {
    return [...transcript.querySelectorAll<HTMLElement>("[data-run-summary]")]
      .find((element) => element.dataset.runSummary === position.entryId);
  }
  // One durable assistant entry can appear as both a process prefix and its
  // final answer. Restoring by entry ID alone would jump to the earlier copy.
  return [...transcript.querySelectorAll<HTMLElement>("[data-entry-id]")]
    .find((element) => element.dataset.entryId === position.entryId
      && (!position.messagePart || element.dataset.messagePart === position.messagePart));
}

export function restoreTranscriptPosition(transcript: HTMLElement, position: ScrollPosition): boolean {
  if (position.bottom) {
    if (transcript.scrollHeight - transcript.clientHeight - transcript.scrollTop > 0.5) transcript.scrollTop = transcript.scrollHeight;
    return true;
  }
  const element = transcriptPositionTarget(transcript, position);
  if (!element) return false;
  const delta = element.getBoundingClientRect().top - transcript.getBoundingClientRect().top - position.offset;
  if (Math.abs(delta) > 0.5) transcript.scrollTop += delta;
  return true;
}
