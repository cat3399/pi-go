import assert from "node:assert/strict";
import test from "node:test";
import { SessionImageCache } from "../src/workbench/session-images.ts";

const inline = (data = "YWJj") => ({ type: "image", mimeType: "image/png", data });
const reference = (entryId = "user-1", blockIndex = 1) => ({
  type: "image", mimeType: "image/png", byteSize: 3,
  imageRef: { entryId, blockIndex }, url: `https://example.invalid/${entryId}/${blockIndex}`,
});
const message = (image, extra = {}) => ({
  role: "user", timestamp: 123, content: [{ type: "text", text: "look" }, image], ...extra,
});

test("cold history keeps images deferred until they are loaded", () => {
  const cache = new SessionImageCache();
  cache.selectSession("session-1");
  const image = reference();
  cache.reconcile([], [message(image)]);
  assert.equal(cache.availableSource(image), null);
  assert.equal(cache.source(image), image.url);
});

test("settled history reuses uploaded image bytes instead of returning to a placeholder", () => {
  const cache = new SessionImageCache();
  cache.selectSession("session-1");
  const image = reference();
  const history = [message(image)];
  cache.reconcile([message(inline())], history);
  assert.equal(cache.availableSource(image), "data:image/png;base64,YWJj");
  assert.equal(cache.source(image), "data:image/png;base64,YWJj");
  cache.reconcile(history, [message(reference())]);
  assert.equal(cache.source(reference()), "data:image/png;base64,YWJj");
  assert.equal(image.data, undefined, "history itself remains a reference");
});

test("already loaded historical images remain available after refresh and remount", () => {
  const cache = new SessionImageCache();
  cache.selectSession("session-1");
  const image = reference();
  cache.rememberLoaded(image, image.url);
  cache.reconcile([message(image)], [message(reference())]);
  assert.equal(cache.availableSource(reference()), image.url);
  cache.selectSession("session-1");
  assert.equal(cache.availableSource(reference()), image.url);
});

test("multiple images and tool result images retain their own sources", () => {
  const cache = new SessionImageCache();
  const current = [message(inline()), message(inline("ZGVm"), { role: "toolResult", toolCallId: "read-1" })];
  current[0].content.push(inline("Z2hp"));
  const images = [reference(), reference("user-1", 2), reference("tool-1")];
  const history = [message(images[0]), message(images[2], { role: "toolResult", toolCallId: "read-1" })];
  history[0].content.push(images[1]);
  cache.reconcile(current, history);
  assert.deepEqual(images.map((image) => cache.source(image)), [
    "data:image/png;base64,YWJj", "data:image/png;base64,Z2hp", "data:image/png;base64,ZGVm",
  ]);
});

test("legacy image blocks are reused", () => {
  const cache = new SessionImageCache();
  const legacy = { type: "image", source: { type: "base64", media_type: "image/png", data: "YWJj" } };
  cache.reconcile([message(legacy)], [message(reference())]);
  assert.equal(cache.availableSource(reference()), "data:image/png;base64,YWJj");
});

test("references do not cross branches or sessions", () => {
  const cache = new SessionImageCache();
  cache.selectSession("session-1");
  const original = reference();
  cache.rememberLoaded(original, original.url);
  const other = reference("different-entry");
  cache.reconcile([message(original)], [message(other)]);
  assert.equal(cache.availableSource(other), null);
  cache.selectSession("session-2");
  assert.equal(cache.availableSource(original), null);
});

test("ambiguous live messages and changed content are not matched", () => {
  const cache = new SessionImageCache();
  cache.reconcile([message(inline()), message(inline("ZGVm"))], [message(reference())]);
  assert.equal(cache.availableSource(reference()), null);
  cache.reconcile([message(inline())], [message(reference(), { timestamp: 124 })]);
  assert.equal(cache.availableSource(reference()), null);
  cache.reconcile([message(inline())], [message({ ...reference(), byteSize: 4 })]);
  assert.equal(cache.availableSource(reference()), null);
});
