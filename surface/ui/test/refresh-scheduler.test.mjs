import assert from "node:assert/strict";
import test from "node:test";
import { RefreshScheduler } from "../src/workbench/refresh-scheduler.ts";

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

test("adjacent turn, settled and catalog events share one refresh", { timeout: 1000 }, async () => {
  const done = deferred();
  const scheduler = new RefreshScheduler(async (requests) => done.resolve(requests), 0);
  try {
    const identity = { generation: 1, sessionId: "one" };
    scheduler.request({ ...identity, turn: true });
    scheduler.request({ ...identity, session: true });
    scheduler.request({ ...identity, snapshot: true });
    assert.deepEqual(await done.promise, [{ ...identity, turn: true, session: true, snapshot: true }]);
  } finally {
    scheduler.dispose();
  }
});

test("changes arriving during a read get one subsequent refresh", { timeout: 1000 }, async () => {
  const started = deferred();
  const release = deferred();
  const finished = deferred();
  const batches = [];
  const scheduler = new RefreshScheduler(async (requests) => {
    batches.push(requests);
    if (batches.length === 1) {
      started.resolve();
      await release.promise;
    } else {
      finished.resolve();
    }
  }, 0);
  try {
    const identity = { generation: 1, sessionId: "one" };
    scheduler.request({ ...identity, turn: true });
    await started.promise;
    scheduler.request({ ...identity, session: true });
    scheduler.request({ ...identity, snapshot: true });
    assert.equal(batches.length, 1);
    release.resolve();
    await finished.promise;
    assert.equal(batches.length, 2);
    assert.equal(batches[1][0].session, true);
    assert.equal(batches[1][0].snapshot, true);
  } finally {
    release.resolve();
    scheduler.dispose();
  }
});

test("effect cleanup discards old requests and setup can resume", { timeout: 1000 }, async () => {
  const done = deferred();
  const scheduler = new RefreshScheduler(async (requests) => done.resolve(requests), 0);
  scheduler.request({ generation: 1, sessionId: "old", session: true });
  scheduler.dispose();
  scheduler.start();
  scheduler.request({ generation: 2, sessionId: "new", session: true });
  try {
    const requests = await done.promise;
    assert.equal(requests.length, 1);
    assert.equal(requests[0].generation, 2);
    assert.equal(requests[0].sessionId, "new");
  } finally {
    scheduler.dispose();
  }
});
