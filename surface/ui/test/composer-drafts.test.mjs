import assert from "node:assert/strict";
import test from "node:test";
import { ComposerDraftStore, composerDraftKey } from "../src/workbench/composer-drafts.ts";

class MemoryStorage {
  values = new Map();
  get length() { return this.values.size; }
  key(index) { return [...this.values.keys()][index] ?? null; }
  getItem(key) { return this.values.get(key) ?? null; }
  setItem(key, value) { this.values.set(key, value); }
  removeItem(key) { this.values.delete(key); }
}

function setup(storage = new MemoryStorage(), namespace = "server") {
  const values = new Map(), writes = [];
  const client = {
    offline: false,
    async readDraftText(key) {
      if (this.offline) throw new Error("offline");
      return values.get(key) ?? { revision: "", text: "" };
    },
    async writeDraftText(key, update) {
      if (this.offline) throw new Error("offline");
      writes.push({ key, ...update });
      const current = await this.readDraftText(key);
      if (current.revision !== update.baseRevision && current.revision !== update.revision) return { saved: false, draft: current };
      const draft = { revision: update.revision, text: update.text };
      values.set(key, draft);
      return { saved: true, draft };
    },
  };
  const makeStore = (ns = namespace) => new ComposerDraftStore(client, ns, { storage, delay: 30, maxDelay: 60 });
  return { storage, client, values, writes, makeStore, store: makeStore() };
}

test("session, scratch and source identities isolate drafts; offline reload preserves attachments", async () => {
  const env = setup();
  env.client.offline = true;
  const key = composerDraftKey("one", "/project");
  const one = env.store.open(key);
  const content = { text: "first", images: [{ id: "local-image", mimeType: "image/png", durable: true }], files: [{ name: "file.txt", path: "/project/file.txt" }] };
  one.update(content);
  env.store.open(composerDraftKey("two", "/project")).update({ text: "second" });
  env.store.open(composerDraftKey(null, "/project")).update({ text: "scratch" });
  const restored = env.makeStore().open(key);
  await restored.refresh();
  assert.deepEqual(restored.getSnapshot().content, content);
  assert.equal(env.makeStore("other-server").open(key).getSnapshot().content.text, "");
  assert.equal(env.store.open(key).getSnapshot().content.text, "first");
});

test("typing is coalesced and only plain text crosses the draft API", async () => {
  const env = setup(), draft = env.store.open("session:one");
  await draft.refresh();
  for (const text of ["a", "ab", "abc"]) draft.update({ text });
  draft.update({ images: [{ id: "local", mimeType: "image/png", durable: true }], files: [{ path: "/file", name: "file" }] });
  assert.equal(env.writes.length, 0);
  await draft.flush();
  assert.equal(env.writes.length, 1);
  assert.deepEqual(Object.keys(env.writes[0]).sort(), ["baseRevision", "key", "revision", "text"]);
  assert.equal(env.writes[0].text, "abc");
});

test("local drafts take precedence over cloud changes, including a locally cleared draft", async () => {
  const env = setup(), draft = env.store.open("session:one");
  draft.update({ text: "local priority" });
  await draft.flush();
  env.values.set("session:one", { revision: "remote-newer", text: "cloud text" });
  const reopened = env.makeStore().open("session:one");
  await reopened.refresh();
  assert.equal(reopened.getSnapshot().content.text, "local priority");
  await reopened.flush();
  assert.equal(env.values.get("session:one").text, "local priority");
  reopened.update({ text: "" });
  await reopened.flush();
  env.values.set("session:one", { revision: "remote-again", text: "cloud text" });
  const cleared = env.makeStore().open("session:one");
  await cleared.refresh();
  assert.equal(cleared.getSnapshot().content.text, "");
  await cleared.flush();
  assert.equal(env.values.get("session:one").text, "");
});

test("cloud text fills only a missing local draft", async () => {
  const env = setup();
  const draft = env.store.open("session:one");
  await draft.refresh();
  assert.equal(env.storage.length, 0, "an empty initial lookup must not create a local draft");
  env.values.set("session:one", { revision: "cloud", text: "restore from cloud" });
  await draft.refresh(true);
  assert.equal(draft.getSnapshot().content.text, "restore from cloud");
  assert.equal(env.makeStore().open("session:one").getSnapshot().content.text, "restore from cloud");
});

test("successful send clears only the submitted version; failure and reload retain newer input", async () => {
  const env = setup(), draft = env.store.open("session:one");
  draft.update({ text: "send this", images: [{ id: "first", mimeType: "image/png", durable: true }] });
  const sent = draft.beginSend();
  draft.update({ text: "next answer", files: [{ path: "/next", name: "next" }] });
  draft.finishSend(sent.id, true);
  assert.deepEqual(draft.getSnapshot().content, { text: "next answer", images: [], files: [{ path: "/next", name: "next" }] });
  const failed = draft.beginSend();
  draft.update({ text: "keep typing" });
  draft.finishSend(failed.id, false);
  assert.equal(draft.getSnapshot().content.text, "next answer\n\nkeep typing");
  const interrupted = draft.beginSend();
  draft.update({ text: "after request" });
  const recovered = env.makeStore().open("session:one");
  assert.equal(recovered.getSnapshot().content.text, "next answer\n\nkeep typing\n\nafter request");
  draft.finishSend(interrupted.id, false);
  await draft.flush();
});

test("new-session migration preserves the in-flight send and follows later input", async () => {
  const env = setup(), draft = env.store.open("new:/project");
  draft.update({ text: "first request" });
  await draft.flush();
  const sent = draft.beginSend();
  draft.update({ text: "prepared reply" });
  env.store.move("new:/project", "session:created");
  assert.equal(env.store.open("session:created"), draft);
  const nextScratch = env.store.open("new:/project");
  await nextScratch.refresh();
  assert.equal(nextScratch.getSnapshot().content.text, "");
  draft.finishSend(sent.id, true);
  await new Promise((resolve) => setImmediate(resolve));
  await draft.flush();
  await nextScratch.flush();
  assert.equal(env.values.get("session:created").text, "prepared reply");
  assert.equal(env.values.get("new:/project").text, "");
  assert.equal(env.makeStore().open("new:/project").getSnapshot().content.text, "");
});

test("concurrent remote edits resolve silently while preserving local input and the other text", async () => {
  const env = setup(), one = env.store.open("session:one"), two = env.makeStore("other-device").open("session:one");
  await Promise.all([one.refresh(), two.refresh()]);
  one.update({ text: "first device" });
  two.update({ text: "second device" });
  await one.flush();
  await two.flush();
  assert.equal(env.values.get("session:one").text, "first device");
  assert.equal(two.getSnapshot().content.text, "second device");
  await two.flush();
  assert.equal(env.values.get("session:one").text, "second device");
  const copy = two.getSnapshot().recoveries.find((copy) => copy.content.text === "first device");
  assert.ok(copy);
});

test("a stale local tab backs up the overwritten version and reports storage failures", async () => {
  const env = setup();
  env.client.offline = true;
  const one = env.store.open("session:one"), otherStore = env.makeStore(), two = otherStore.open("session:one");
  one.update({ text: "window one" });
  two.update({ text: "window two" });
  assert.ok(two.getSnapshot().recoveries.some((copy) => copy.content.text === "window one"));
  const write = env.storage.setItem;
  env.storage.setItem = () => { throw new Error("quota"); };
  two.update({ text: "quota must not erase this" });
  assert.equal(two.getSnapshot().content.text, "quota must not erase this");
  assert.ok(two.getSnapshot().localError);
  env.storage.setItem = write;
});

test("edits made while a sync request is pending remain dirty until their own acknowledgement", async () => {
  const env = setup(), draft = env.store.open("session:one");
  await draft.refresh();
  let resolve;
  const original = env.client.writeDraftText.bind(env.client);
  env.client.writeDraftText = (key, update) => new Promise((done) => { resolve = async () => done(await original(key, update)); });
  draft.update({ text: "older" });
  const inFlight = draft.flush();
  draft.update({ text: "newer" });
  await resolve();
  await inFlight;
  assert.equal(draft.getSnapshot().content.text, "newer");
  assert.equal(draft.getSnapshot().textChanged, true);
  env.client.writeDraftText = original;
  await draft.flush();
  assert.equal(env.values.get("session:one").text, "newer");
  assert.equal(draft.getSnapshot().textChanged, false);
});

test("a delayed remote read cannot roll back an acknowledged local write", async () => {
  const env = setup(), draft = env.store.open("session:one");
  draft.update({ text: "old" });
  await draft.flush();
  const old = env.values.get("session:one");
  const read = env.client.readDraftText.bind(env.client);
  let resolve;
  env.client.readDraftText = () => new Promise((done) => { resolve = done; });
  const staleRead = draft.refresh(true);
  env.client.readDraftText = read;
  draft.update({ text: "new" });
  await draft.flush();
  resolve(old);
  await staleRead;
  assert.equal(draft.getSnapshot().content.text, "new");
  assert.equal(draft.getSnapshot().textChanged, false);
});

test("overwriting a different window's pending send retains its submitted text in recovery", async () => {
  const env = setup();
  env.client.offline = true;
  const one = env.store.open("session:one"), two = env.makeStore().open("session:one");
  one.update({ text: "waiting for acknowledgement" });
  const pending = one.beginSend();
  two.update({ text: "second window" });
  assert.ok(two.getSnapshot().recoveries.some((copy) => copy.content.text === "waiting for acknowledgement"));
  one.finishSend(pending.id, false);
});
