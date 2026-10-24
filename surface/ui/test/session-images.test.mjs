import assert from "node:assert/strict";
import test from "node:test";
import { SessionImageCache } from "../src/workbench/session-images.ts";
import { ResourceCache } from "../src/workbench/resource-cache.ts";

const inline = (data = "YWJj") => ({ type: "image", mimeType: "image/png", data });
const reference = (entryId = "user-1", blockIndex = 1, generation = "generation-1") => ({
  type: "image", mimeType: "image/png", byteSize: 3,
  imageRef: { generation, entryId, blockIndex }, url: `https://example.invalid/${generation}/${entryId}/${blockIndex}`,
});
const message = (image, extra = {}) => ({ role: "user", timestamp: 123, content: [{ type: "text", text: "look" }, image], ...extra });

test("cold historical images stay deferred, then retain their loaded reference across session switches", () => {
  const cache = new SessionImageCache();
  const image = reference();
  cache.selectSession("session-1", "generation-1");
  assert.equal(cache.availableSource(image), null);
  assert.equal(cache.source(image), image.url);
  cache.rememberLoaded(image, image.url);
  cache.selectSession("session-2", "generation-1");
  assert.equal(cache.availableSource(image), null);
  cache.selectSession("session-1", "generation-1");
  assert.equal(cache.availableSource(image), image.url);
  assert.equal(cache.availableSource(reference("user-1", 1, "generation-2")), null);
});

test("only an exact core receipt associates a live image with an entry", () => {
  const cache = new SessionImageCache();
  cache.selectSession("session-1", "generation-1");
  cache.rememberMessage("session-1", "agent/1", message(inline()));
  cache.rememberMessage("session-1", "agent/2", message(inline("ZGVm")));
  assert.equal(cache.availableSource(reference()), null, "matching text and timestamps do not establish identity");
  cache.associate("session-1", "agent/2", "user-1", "generation-1");
  assert.equal(cache.availableSource(reference()), "data:image/png;base64,ZGVm");
  cache.associate("session-1", "agent/1", "different-entry", "generation-1");
  assert.equal(cache.availableSource(reference("different-entry")), "data:image/png;base64,YWJj");
});

test("images in one message retain exact block positions, including legacy inline bytes", () => {
  const cache = new SessionImageCache();
  cache.selectSession("session-1", "generation-1");
  const value = message(inline());
  value.content.push({ type: "image", source: { type: "base64", media_type: "image/png", data: "Z2hp" } });
  cache.rememberMessage("session-1", "agent/1", value);
  cache.associate("session-1", "agent/1", "user-1", "generation-1");
  assert.equal(cache.availableSource(reference("user-1", 1)), "data:image/png;base64,YWJj");
  assert.equal(cache.availableSource(reference("user-1", 2)), "data:image/png;base64,Z2hp");
  assert.equal(cache.availableSource(reference("user-1", 0)), null);
});

test("IPC images load lazily with their session and source generation, and concurrent clicks merge", async () => {
  const calls = [];
  const cache = new SessionImageCache(async (sessionId, query) => {
    calls.push({ sessionId, query });
    return { token: "image", data: { sessionId, ...query, mimeType: "image/png", data: "YWJj" } };
  });
  cache.selectSession("session-1", "generation-1");
  const image = reference();
  delete image.url;
  assert.equal(cache.source(image), "");
  assert.equal(calls.length, 0);
  const results = await Promise.all([cache.load(image), cache.load(image)]);
  assert.deepEqual(results, ["data:image/png;base64,YWJj", "data:image/png;base64,YWJj"]);
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0], { sessionId: "session-1", query: { entryId: "user-1", blockIndex: 1, generation: "generation-1" } });
});

test("media retained bytes share the bounded resource cache", () => {
  const resources = new ResourceCache(2000);
  const cache = new SessionImageCache(undefined, resources);
  cache.selectSession("session-1", "generation-1");
  for (let i = 0; i < 12; i++) cache.rememberLoaded(reference(`entry-${i}`), "data:image/png;base64," + "a".repeat(400));
  assert.ok(resources.retainedBytes <= resources.budget);
  assert.equal(cache.availableSource(reference("entry-0")), null);
  assert.ok(cache.availableSource(reference("entry-11")));
});
