import assert from "node:assert/strict";
import test from "node:test";
import { captureTranscriptPosition, restoreTranscriptPosition, transcriptPositionTarget } from "../src/workbench/transcript-position.ts";

// Deterministic layout: scroll changes viewport coordinates, while a prepend
// changes document coordinates. The process and answer share a durable ID.
function transcript() {
  let scrollTop = 500;
  const elements = [];
  const writes = [];
  const viewport = {
    clientHeight: 600,
    scrollHeight: 1800,
    get scrollTop() { return scrollTop; },
    set scrollTop(value) {
      scrollTop = Math.max(0, Math.min(value, this.scrollHeight - this.clientHeight));
      writes.push(scrollTop);
    },
    getBoundingClientRect: () => ({ top: 50, bottom: 650 }),
    querySelectorAll(selector) {
      return elements.filter((element) => selector.includes("[data-entry-id]") && element.dataset.entryId
        || selector.includes("[data-run-summary]") && element.dataset.runSummary);
    },
  };
  const add = (dataset, top, height) => {
    const element = {
      dataset, top, height,
      getBoundingClientRect() { return { top: 50 + this.top - scrollTop, bottom: 50 + this.top + this.height - scrollTop }; },
    };
    elements.push(element);
    return element;
  };
  const summary = add({ runSummary: "user" }, 50, 24);
  const process = add({ entryId: "assistant", messagePart: "process" }, 100, 30);
  const answer = add({ entryId: "assistant", messagePart: "message" }, 154, 1400);
  return { viewport, elements, writes, summary, process, answer };
}

test("a completed answer stays still across rerenders with its process expanded", () => {
  const { viewport, writes, answer } = transcript();
  for (let render = 0; render < 5; render++) {
    const position = captureTranscriptPosition(viewport, false);
    assert.equal(transcriptPositionTarget(viewport, position), answer);
    assert.equal(restoreTranscriptPosition(viewport, position), true);
    assert.equal(viewport.scrollTop, 500, "restoring the answer must not use its process prefix");
  }
  assert.deepEqual(writes, [], "unchanged layout must not interrupt native scrolling");
});

test("resizing content above a split answer compensates only for the actual height change", () => {
  const { viewport, process, answer } = transcript();
  const position = captureTranscriptPosition(viewport, false);
  const before = answer.getBoundingClientRect().top;
  process.height += 90;
  answer.top += 90;
  assert.equal(restoreTranscriptPosition(viewport, position), true);
  assert.equal(viewport.scrollTop, 590);
  assert.equal(answer.getBoundingClientRect().top, before);
});

test("paging before the viewport preserves the answer offset through prepends and trims", () => {
  const { viewport, elements, answer } = transcript();
  for (const shift of [300, -240]) {
    const position = captureTranscriptPosition(viewport, false);
    const before = answer.getBoundingClientRect().top;
    for (const element of elements) element.top += shift;
    viewport.scrollHeight += shift;
    assert.equal(restoreTranscriptPosition(viewport, position), true);
    assert.equal(answer.getBoundingClientRect().top, before);
  }
  assert.equal(viewport.scrollTop, 560);
});

test("reading the process restores that fragment, and never substitutes the final answer", () => {
  const { viewport, elements, process, answer } = transcript();
  viewport.scrollTop = 100;
  const position = captureTranscriptPosition(viewport, false);
  assert.equal(transcriptPositionTarget(viewport, position), process);
  assert.equal(restoreTranscriptPosition(viewport, position), true);
  elements.splice(elements.indexOf(process), 1);
  assert.equal(restoreTranscriptPosition(viewport, position), false);
  assert.ok(elements.includes(answer));
  assert.equal(viewport.scrollTop, 100);
});

test("a collapsed process does not change the identity of the saved answer", () => {
  const { viewport, elements, process, answer } = transcript();
  const position = captureTranscriptPosition(viewport, false);
  const before = answer.getBoundingClientRect().top;
  elements.splice(elements.indexOf(process), 1);
  answer.top -= 54;
  assert.equal(restoreTranscriptPosition(viewport, position), true);
  assert.equal(answer.getBoundingClientRect().top, before);
});

test("run summaries and navigation to entries without a captured fragment remain addressable", () => {
  const { viewport, summary, process } = transcript();
  viewport.scrollTop = 50;
  const position = captureTranscriptPosition(viewport, false);
  assert.equal(transcriptPositionTarget(viewport, position), summary);
  summary.top += 20;
  assert.equal(restoreTranscriptPosition(viewport, position), true);
  assert.equal(viewport.scrollTop, 70);
  assert.equal(transcriptPositionTarget(viewport, { entryId: "assistant", offset: 24, bottom: false }), process);
});

test("following the bottom still tracks appended content without redundant scroll writes", () => {
  const { viewport, writes } = transcript();
  viewport.scrollTop = viewport.scrollHeight;
  writes.length = 0;
  const position = captureTranscriptPosition(viewport, true);
  assert.equal(restoreTranscriptPosition(viewport, position), true);
  assert.deepEqual(writes, []);
  viewport.scrollHeight += 100;
  assert.equal(restoreTranscriptPosition(viewport, position), true);
  assert.equal(viewport.scrollTop, 1300);
});
