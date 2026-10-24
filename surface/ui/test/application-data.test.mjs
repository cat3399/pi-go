import assert from "node:assert/strict";
import test from "node:test";
import { ResourceCache } from "../src/workbench/resource-cache.ts";
import { ApplicationDataStore } from "../src/workbench/application-data.ts";

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

const tick = () => new Promise((resolve) => setImmediate(resolve));
async function until(check) {
  for (let i = 0; i < 100; i++) { if (check()) return; await new Promise((resolve) => setTimeout(resolve, 5)); }
  assert.fail("condition was not reached");
}

test("identical reads singleflight, and invalidation rejects an in-flight old response", async () => {
  const cache = new ResourceCache();
  const old = deferred(), fresh = deferred();
  let count = 0;
  const load = () => (++count === 1 ? old.promise : fresh.promise);
  const one = cache.read("history", load), two = cache.read("history", load);
  await tick();
  assert.equal(count, 1);
  cache.invalidate("history");
  const three = cache.read("history", load);
  old.resolve({ token: "old", data: ["old"] });
  await tick();
  assert.equal(count, 2);
  assert.equal(cache.peek("history"), undefined);
  fresh.resolve({ token: "new", data: ["new"] });
  assert.deepEqual((await Promise.all([one, two, three])).map((result) => result.data), [["new"], ["new"], ["new"]]);
  assert.deepEqual((await cache.read("history", load)).data, ["new"]);
  assert.equal(count, 2, "validated cached range needs no read");
});

test("failed requests retain successful data and byte eviction does not evict pinned working data", async () => {
  const cache = new ResourceCache(3000);
  cache.put("one", { text: "a".repeat(500) });
  const unpin = cache.pin("one");
  cache.put("two", { text: "b".repeat(500) });
  cache.put("three", { text: "c".repeat(500) });
  assert.ok(cache.peek("one"));
  assert.equal(cache.peek("two"), undefined);
  assert.ok(cache.retainedBytes <= cache.budget);
  cache.invalidate("one");
  await assert.rejects(cache.read("one", () => Promise.reject(new Error("offline"))), /offline/);
  assert.equal(cache.peek("one").text.length, 500);
  assert.equal(cache.isFresh("one"), false);
  unpin();
});

function fakeClient() {
  let sequence = 0, epoch = "EpochA", generation = "SourceA", log = 1, state = 5, payloadVersion = 0;
  let streaming = { id: "AgentA/2", message: { role: "assistant", content: [{ type: "text", text: "partial" }] } };
  const calls = [], subscriptions = [];
  const info = { id: "one", cwd: "/project", projectRoot: "/project", path: "/one.jsonl", created: "2026-01-01T00:00:00Z", modified: "2026-01-02T00:00:00Z", messageCount: 1, firstMessage: "question" };
  const version = () => ({ generation, log, view: log });
  const record = (index) => ({ node: { id: `n${index}`, parentId: index > 1 ? `n${index - 1}` : null, type: "message", role: index % 4 === 1 ? "user" : "assistant", timestamp: "2026-01-01T00:00:00Z", preview: `entry ${index}`, hasChildren: false, rawBytes: 100, contentToken: `entry-${index}-${payloadVersion}` }, message: { role: index % 4 === 1 ? "user" : "assistant", content: `entry ${index}` } });
  const live = () => ({ sessionId: "one", active: !client.inactive, version: { generation: "AgentA", state, configuration: 1, queue: 1 },
    activityVersion: { generation: "ActivityA", revision: 3 }, bashVersion: { generation: "ActivityA", revision: 1 },
    state: { cwd: "/project", phase: "provider", isStreaming: true, queuedMessages: { steering: [], followUp: [] } },
    history: { ...version(), leafId: `n${log}` }, streaming, pending: [], toolsRunning: [] });
  const client = {
    kind: "remote", endpoint: "https://fixture.invalid", calls, subscriptions,
    delayLive: null,
    snapshot() { throw new Error("whole application snapshot is forbidden in paged reads"); },
    sessionView() { throw new Error("whole session projection is forbidden in paged reads"); },
    async readApplicationResource(kind, query, known) {
      calls.push({ kind, query, known });
      const data = kind === "overview" ? { agentDir: "/agent", defaultCwd: "/project", projects: [{ path: "/project", modified: info.modified, sessionCount: 1 }], sessionCount: 1, runningSessionIds: ["one"] }
        : { version: "DirectoryA", sessions: [info], total: 1, start: 0 };
      return { token: JSON.stringify(data), data: structuredClone(data), cursor: `${epoch}:${sequence}` };
    },
    async readResource(id, kind, query = {}, known) {
      calls.push({ id, kind, query, known });
      let data;
      if (client.deleted) throw Object.assign(new Error("session not found"), { status: 404 });
      if (kind === "entry") data = { sessionId: id, generation, entryId: query.entryId, contentToken: record(Number(query.entryId.slice(1))).node.contentToken, message: record(Number(query.entryId.slice(1))).message };
      if (kind === "metadata") data = { sessionId: id, version: version(), info, filePath: info.path, leafId: `n${log}` };
      if (kind === "live") { data = live(); if (client.delayLive) { const wait = client.delayLive; client.delayLive = null; await wait.promise; } }
      if (kind === "history") {
        const cursor = query.cursor?.split(":");
        const at = cursor ? { generation, log: Number(cursor[1]), view: Number(cursor[1]) } : query.at ?? version();
        const after = query.afterId ? Number(query.afterId.slice(1)) : 0;
        const total = Number((query.leafId ?? `n${at.log}`).slice(1));
        const newer = cursor?.[0] === "newer" || query.direction === "newer";
        const position = query.aroundId ? Number(query.aroundId.slice(1)) : cursor ? Number(cursor[2]) : newer ? after : total;
        const start = newer ? position : Math.max(after, position - (query.limit ?? 64));
        const end = newer ? Math.min(total, position + (query.limit ?? 64)) : position;
        data = { sessionId: id, version: { generation: at.generation, log: at.log, view: at.view }, anchorId: `n${total}`, items: Array.from({ length: end - start }, (_, index) => record(start + index + 1)), start, end, total, connected: true, ...(start ? { older: `older:${at.log}:${start}` } : {}), ...(end < total ? { newer: `newer:${at.log}:${end}` } : {}) };
      }
      if (kind === "nodes") {
        const encoded = query.cursor ? JSON.parse(query.cursor) : null;
        const at = encoded?.at ?? query.at ?? version();
        const total = Number((query.leafId ?? `n${at.log}`).slice(1));
        const after = query.afterId ? Number(query.afterId.slice(1)) : 0;
        const connected = after <= total;
        const floor = connected ? after : 0;
        const matching = Array.from({ length: total - floor }, (_, index) => floor + index + 1)
          .filter((index) => query.entryType !== "compaction" && (!query.role || record(index).node.role === query.role));
        const offset = encoded?.offset ?? 0, end = Math.min(matching.length, offset + (query.limit ?? 80));
        data = { sessionId: id, version: at, connected,
          items: matching.slice(offset, end).map((index) => ({ ...record(index).node, position: index - 1 })),
          ...(end < matching.length ? { next: JSON.stringify({ at, offset: end }) } : {}) };
      }
      if (!data) throw new Error(`unexpected resource ${kind}`);
      return { token: JSON.stringify(data), data: structuredClone(data) };
    },
    subscribe(after, observer, sessions) {
      const subscription = { after, observer, sessions, closed: false, ready: Promise.resolve(), close() { this.closed = true; } };
      subscriptions.push(subscription);
      return subscription;
    },
    emit(event, id = "one") {
      const envelope = { sessionId: id, sequence: ++sequence, cursor: `${epoch}:${sequence}`, event };
      const subscription = subscriptions.at(-1);
      if (subscription && !subscription.closed) subscription.observer.onEvent(envelope);
    },
    advance() { log = 2; state = 8; streaming = null; },
    restart() { epoch = "EpochB"; sequence = 1; generation = "SourceB"; state = 1; streaming = null; },
    setLog(value) { log = value; },
    changeEntry() { payloadVersion++; },
  };
  return client;
}

test("cached session revisits read no whole projection and reuse the fixed history range", async () => {
  const client = fakeClient(), store = new ApplicationDataStore(client);
  client.setLog(12000);
  const view = await store.loadSession("one");
  assert.equal(view.window.pages[0].items.length, 128);
  assert.equal(view.window.start, 11872);
  assert.equal(client.calls.find((call) => call.kind === "history").query.maxBytes, 512 * 1024);
  const count = client.calls.length;
  assert.equal(await store.loadSession("one"), view);
  assert.equal(client.calls.length, count);
  const owner = Symbol();
  store.select(owner, "one");
  await store.loadSession("one");
  assert.equal(client.calls.filter((call) => call.kind === "history").length, 1);
  store.select(owner, null);
});

test("snapshot overlap uses core message IDs and receipts; a tail read replaces pending exactly once", async () => {
  const client = fakeClient(), store = new ApplicationDataStore(client);
  await store.start();
  const owner = Symbol();
  store.select(owner, "one");
  const delayed = deferred();
  client.delayLive = delayed;
  const baseline = store.loadSession("one");
  await tick();
  const version = (state) => ({ generation: "AgentA", state, configuration: 1, queue: 1 });
  client.emit({ type: "message_update", readVersion: version(6), messageId: "AgentA/2", message: { role: "assistant", content: "complete" } });
  client.emit({ type: "message_end", readVersion: version(7), messageId: "AgentA/2", message: { role: "assistant", content: "complete" } });
  client.advance();
  client.emit({ type: "message_receipt", readVersion: version(8), messageId: "AgentA/2", generation: "SourceA", entryId: "n2", logVersion: 2, viewVersion: 2, accepted: true, durable: true });
  assert.equal(store.lastAppliedCursor, "EpochA:0", "buffered events must not advance the cache checkpoint");
  delayed.resolve();
  await baseline;
  assert.equal(store.session("one").pending.length, 1);
  assert.equal(store.session("one").pending[0].entryId, "n2");
  assert.equal(store.session("one").live.streaming, null);
  await until(() => store.session("one").window.total === 2);
  assert.equal(store.session("one").pending.length, 0);
  assert.deepEqual(store.session("one").window.pages.flatMap((page) => page.items.map((item) => item.node.id)), ["n1", "n2"]);
  const history = client.calls.filter((call) => call.kind === "history");
  assert.equal(history.length, 2);
  assert.equal(history[1].query.afterId, "n1");
  assert.equal(store.lastAppliedCursor, "EpochA:3");
  store.stop();
});

test("resource versions deduplicate replay while activity has its own revision", async () => {
  const client = fakeClient(), store = new ApplicationDataStore(client);
  await store.start();
  await store.loadSession("one");
  client.emit({ type: "message_update", readVersion: { generation: "AgentA", state: 4, configuration: 1, queue: 1 }, messageId: "AgentA/2", message: { role: "assistant", content: "stale" } });
  assert.equal(store.session("one").live.streaming.message.content[0].text, "partial");
  client.emit({ type: "live_resource", activityVersion: { generation: "ActivityA", revision: 4 }, activity: { phase: "retry_wait", retryWaiting: true, retryAttempt: 2 } });
  assert.equal(store.session("one").live.state.retryWaiting, true);
  client.emit({ type: "auto_retry_start", readVersion: { generation: "AgentA", state: 3, configuration: 1, queue: 1 }, activityVersion: { generation: "ActivityA", revision: 3 }, activity: { phase: "retry_wait", retryWaiting: false } });
  assert.equal(store.session("one").live.state.retryWaiting, true);
  store.stop();
});

test("restart replaces both resource generation and opaque cursor even at a lower sequence", async () => {
  const client = fakeClient(), store = new ApplicationDataStore(client);
  await store.start();
  const owner = Symbol();
  store.select(owner, "one");
  await store.loadSession("one");
  for (let i = 0; i < 8; i++) client.emit({ type: "unrelated" });
  assert.equal(store.lastAppliedCursor, "EpochA:8");
  client.restart();
  client.subscriptions.at(-1).observer.onReset(1, "expired", "EpochB:1");
  await until(() => store.session("one").metadata.version.generation === "SourceB");
  assert.equal(store.session("one").window.version.generation, "SourceB");
  assert.equal(store.lastAppliedCursor, "EpochB:1");
  assert.deepEqual(client.subscriptions.at(-1).sessions, ["one"]);
  store.stop();
});

test("history render windows stay bounded and old-node lookup skips all intervening pages", async () => {
  const client = fakeClient(), store = new ApplicationDataStore(client);
  client.setLog(12000);
  await store.loadSession("one");
  for (let index = 0; index < 5; index++) await store.pageHistory("one", "older");
  let value = store.session("one").window;
  assert.ok(value.pages.flatMap((page) => page.items).length <= 384);
  assert.equal(value.start, 11232);
  assert.equal(value.end, 11616);
  await store.pageHistory("one", "newer");
  value = store.session("one").window;
  assert.equal(value.end, 11744);
  const previous = client.calls.filter((call) => call.kind === "history").length;
  await store.pageHistory("one", "latest", "n21");
  assert.equal(client.calls.filter((call) => call.kind === "history").length, previous + 1);
  assert.equal(store.session("one").window.end, 21);
  assert.equal(store.session("one").window.following, false);
  await store.pageHistory("one", "latest");
  assert.equal(store.session("one").window.end, 12000);
});

test("small tail updates accumulate into a recent window instead of evicting one page per message", async () => {
  const client = fakeClient(), store = new ApplicationDataStore(client);
  client.setLog(128);
  await store.loadSession("one");
  for (let log = 129; log <= 138; log++) {
    client.setLog(log);
    await store.loadSession("one", true);
  }
  const value = store.session("one").window;
  assert.equal(value.start, 0);
  assert.equal(value.end, 138);
  assert.equal(value.pages.length, 2);
});

test("requests from a stopped connection cannot overwrite its resumed baseline", async () => {
  const client = fakeClient(), store = new ApplicationDataStore(client);
  const wait = deferred();
  client.delayLive = wait;
  const old = store.loadSession("one");
  await tick();
  store.stop();
  client.restart();
  const current = store.loadSession("one", true);
  wait.resolve();
  await Promise.all([old, current]);
  assert.equal(store.session("one").metadata.version.generation, "SourceB");
});


test("failed recovery keeps the applied cursor and automatically retries the baseline", async () => {
  const client = fakeClient(), store = new ApplicationDataStore(client);
  await store.start();
  store.select(Symbol(), "one");
  await store.loadSession("one");
  client.emit({ type: "unrelated" });
  const applied = store.lastAppliedCursor;
  const read = client.readResource.bind(client);
  let fail = true;
  client.readResource = async (...args) => {
    if (fail && args[1] === "live") throw new Error("offline during baseline");
    return read(...args);
  };
  client.restart();
  client.subscriptions.at(-1).observer.onReset(1, "expired", "EpochB:1");
  await until(() => store.lastError.includes("offline"));
  assert.equal(store.lastAppliedCursor, applied);
  assert.ok(client.subscriptions.at(-1).closed);
  fail = false;
  // The retry timer owns reconnecting; no user action or periodic poll is used.
  await new Promise((resolve) => setTimeout(resolve, 550));
  await until(() => store.lastAppliedCursor === "EpochB:1");
  assert.equal(store.session("one").metadata.version.generation, "SourceB");
  assert.equal(client.subscriptions.at(-1).closed, false);
  store.stop();
});

test("recovery holds both events and cursor until the selected baseline completes", async () => {
  const client = fakeClient(), store = new ApplicationDataStore(client);
  await store.start();
  store.select(Symbol(), "one");
  await store.loadSession("one");
  client.emit({ type: "unrelated" });
  const applied = store.lastAppliedCursor, pending = deferred();
  client.restart();
  client.delayLive = pending;
  client.subscriptions.at(-1).observer.onReset(1, "expired", "EpochB:1");
  await until(() => client.subscriptions.at(-1).after === "EpochB:1");
  client.emit({ type: "unrelated" });
  client.subscriptions.at(-1).observer.onConnected("EpochB:2");
  assert.equal(store.lastAppliedCursor, applied);
  pending.resolve();
  await until(() => store.lastAppliedCursor === "EpochB:2");
  assert.equal(store.session("one").metadata.version.generation, "SourceB");
  store.stop();
});

test("a second reset during recovery renews the baseline before applying a checkpoint", async () => {
  const client = fakeClient(), store = new ApplicationDataStore(client);
  await store.start(); store.select(Symbol(), "one"); await store.loadSession("one");
  client.emit({ type: "unrelated" });
  const applied = store.lastAppliedCursor, pending = deferred();
  client.restart(); client.delayLive = pending;
  client.subscriptions.at(-1).observer.onReset(1, "expired", "EpochB:1");
  await until(() => client.subscriptions.at(-1).after === "EpochB:1");
  const count = client.calls.filter((call) => call.kind === "overview").length;
  client.subscriptions.at(-1).observer.onReset(1, "expired again", "EpochB:1");
  pending.resolve();
  assert.equal(store.lastAppliedCursor, applied);
  await until(() => client.calls.filter((call) => call.kind === "overview").length > count && store.lastAppliedCursor === "EpochB:1");
  store.stop();
});


test("full navigation is metadata-only, appends only its tail, and distant jumps read one body page", async () => {
  const client = fakeClient(), store = new ApplicationDataStore(client);
  client.setLog(12000); store.select(Symbol(), "one");
  await store.loadSession("one");
  await until(() => store.navigation("one")?.items.length === 3000);
  assert.equal(client.calls.filter((call) => call.kind === "history").length, 1);
  const original = store.navigation("one").items.map((item) => item.id);
  const count = client.calls.length;
  client.setLog(12004); await store.loadSession("one", true);
  await until(() => store.navigation("one")?.items.length === 3001);
  assert.deepEqual(store.navigation("one").items.slice(0, 3000).map((item) => item.id), original);
  const nodes = client.calls.slice(count).filter((call) => call.kind === "nodes");
  assert.equal(nodes.length, 2);
  assert.ok(nodes.every((call) => call.query.afterId === "n12000"));
  const bodyReads = client.calls.filter((call) => call.kind === "history").length;
  await store.pageHistory("one", "latest", "n21");
  assert.equal(client.calls.filter((call) => call.kind === "history").length, bodyReads + 1);
  assert.equal(client.calls.filter((call) => call.kind === "history").at(-1).query.aroundId, "n21");
  assert.equal(store.navigation("one").items.length, 3001, "paging never renumbers the full navigation index");
  store.stop();
});


test("runtime admission preserves an edit draft only when the full entry identity matches", async () => {
  for (const changed of [false, true]) {
    const client = fakeClient(), store = new ApplicationDataStore(client);
    store.select(Symbol(), "one"); await store.loadSession("one");
    const oldKey = store.draftKey("one", "SourceA", "n1");
    store.drafts.set(oldKey, { text: "unsent draft", original: "entry 1", contentToken: "entry-1-0" });
    store.editing.set(JSON.stringify(["one", "SourceA"]), "n1");
    client.restart(); if (changed) client.changeEntry();
    await store.loadSession("one", true);
    await tick();
    const newKey = store.draftKey("one", "SourceB", "n1");
    if (changed) {
      assert.equal(store.drafts.get(oldKey).text, "unsent draft");
      assert.equal(store.drafts.has(newKey), false, "a reused entry ID cannot receive another source's draft");
    } else {
      await until(() => store.drafts.has(newKey));
      assert.equal(store.drafts.get(newKey).text, "unsent draft");
      assert.equal(store.editing.get(JSON.stringify(["one", "SourceB"])), "n1");
    }
    store.stop();
  }
});

test("a session deleted during disconnection cannot trap all other synchronization in recovery", async () => {
  const client = fakeClient(), store = new ApplicationDataStore(client);
  await store.start(); store.select(Symbol(), "one"); await store.loadSession("one");
  client.restart(); client.deleted = true;
  client.subscriptions.at(-1).observer.onReset(1, "expired", "EpochB:1");
  await until(() => store.lastAppliedCursor === "EpochB:1");
  assert.equal(store.session("one"), undefined);
  assert.equal(store.isMissing("one"), true);
  client.emit({ type: "unrelated" }, "another");
  assert.equal(store.lastAppliedCursor, "EpochB:2");
  store.stop();
});


test("scrolling past an old snapshot reads the next new page without skipping the append gap", async () => {
  const client = fakeClient(), store = new ApplicationDataStore(client);
  client.setLog(256); await store.loadSession("one");
  await store.pageHistory("one", "older");
  client.setLog(1024); await store.loadSession("one", true);
  assert.equal(store.session("one").window.end, 256);
  await store.pageHistory("one", "newer");
  const window = store.session("one").window;
  assert.equal(window.end, 384);
  assert.equal(window.total, 1024);
  assert.equal(window.pages.at(-1).items[0].node.id, "n257");
  const query = client.calls.filter((call) => call.kind === "history").at(-1).query;
  assert.equal(query.afterId, "n256");
  assert.equal(query.direction, "newer");
  assert.equal(window.following, false);
});


test("the first readonly paint does not download an index that Runtime admission will replace", async () => {
  const client = fakeClient(), store = new ApplicationDataStore(client);
  client.inactive = true; client.setLog(12000); store.select(Symbol(), "one");
  await store.loadSession("one"); await tick();
  assert.equal(client.calls.filter((call) => call.kind === "nodes").length, 0);
  client.inactive = false; client.restart();
  await store.loadSession("one", true); await store.ensureNavigation("one");
  assert.equal(store.navigation("one").items.length, 3000);
  assert.equal(client.calls.filter((call) => call.kind === "nodes").length, 13);
  store.stop();
});

test("the first screen seeds both directory levels and expanding a project reuses its session page", async () => {
  const client = fakeClient(), store = new ApplicationDataStore(client);
  const info = (project, index) => ({ id: `${project}-${index}`, cwd: project, projectRoot: project, path: `${project}/${index}.jsonl`,
    modified: new Date(Date.UTC(2026, 0, 40 - index)).toISOString(), created: "2026-01-01T00:00:00Z", messageCount: 2, firstMessage: `question ${index}` });
  const directory = (project, start, limit) => ({ version: project, total: 35, start,
    sessions: Array.from({ length: Math.min(limit, 35 - start) }, (_, index) => info(project, start + index)),
    ...(start + limit < 35 ? { next: JSON.stringify({ project, start: start + limit }) } : {}) });
  const projectPage = (start) => ({ version: "projects", start, total: 12,
    projects: Array.from({ length: Math.min(8, 12 - start) }, (_, index) => {
      const path = `/project-${start + index}`;
      return { project: { path, modified: "2026-01-01T00:00:00Z", sessionCount: 35 }, sessions: directory(path, 0, 8) };
    }), ...(start === 0 ? { next: "next-projects" } : {}) });
  client.readApplicationResource = async (kind, query) => {
    client.calls.push({ kind, query });
    let data;
    if (kind === "overview") {
      const page = projectPage(0);
      data = { agentDir: "/agent", defaultCwd: "/project-0", projects: page.projects.map((item) => item.project),
        directory: page, sessionCount: 420, runningSessionIds: [] };
    } else if (kind === "projects") data = projectPage(query.cursor ? 8 : 0);
    else if (kind === "directory") {
      const start = query.cursor ? JSON.parse(query.cursor).start : 0;
      data = directory(query.project, start, query.limit);
    }
    return { token: JSON.stringify(data), data, cursor: "EpochA:0" };
  };
  await store.start();
  assert.equal(store.snapshot.projects.length, 8);
  assert.equal(store.snapshot.sessions.length, 64);
  assert.equal(store.hasMoreProjects, true);
  await store.loadDirectory("/project-0");
  assert.deepEqual(client.calls.map((call) => call.kind), ["overview"], "opening a seeded project must not request its first page again");
  await store.loadDirectory("/project-0", "", true);
  assert.equal(store.directory("/project-0").sessions.length, 28);
  assert.equal(client.calls.at(-1).query.limit, 20);
  await store.refreshOverview(true);
  assert.equal(store.directory("/project-0").sessions.length, 28, "head validation must retain manually expanded rows");
  await store.loadProjects();
  assert.equal(store.snapshot.projects.length, 12);
  assert.equal(store.hasMoreProjects, false);
  const count = client.calls.length;
  await store.loadDirectory("/project-10");
  assert.equal(client.calls.length, count, "project continuation must seed its session rows too");
  await store.loadDirectory("/project-0", "", true);
  assert.equal(store.directory("/project-0").sessions.length, 35);
  assert.equal(store.directory("/project-0").next, undefined);
  store.stop();
});
