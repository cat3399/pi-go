import assert from "node:assert/strict";
import test from "node:test";
import { ReadMarkers } from "../src/workbench/read-markers.ts";

test("read markers survive refresh, isolate connections, and merge reads across windows", () => {
  const values = new Map();
  const storage = { getItem: (key) => values.get(key) ?? null, setItem: (key, value) => values.set(key, value) };
  const first = { id: "one", lastAssistantMessageId: "a1", messageCount: 2 };
  const second = { ...first, lastAssistantMessageId: "a2", messageCount: 4 };
  const tab = new ReadMarkers("server-a", storage);
  tab.initialize(first);
  assert.equal(tab.unread(first), false);
  assert.equal(tab.unread(second), true);
  const reopened = new ReadMarkers("server-a", storage);
  assert.equal(reopened.unread(second), true);
  assert.equal(reopened.markRead(second), true);
  assert.equal(tab.markRead(first), false, "an old window cannot move a read cursor backwards");
  tab.refresh();
  assert.equal(tab.unread(second), false);
  const other = new ReadMarkers("server-b", storage);
  other.initialize(first);
  assert.equal(other.unread(second), true);
  other.initialize({ ...first, id: "new-session" }, true);
  assert.equal(other.unread({ ...first, id: "new-session" }), true);
});
