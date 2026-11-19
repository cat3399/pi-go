import assert from "node:assert/strict";
import test from "node:test";
import { pastedAttachments } from "../src/workbench/composer-paste.ts";

const image = new File(["png"], "screenshot.png", { type: "image/png" });
const file = new File(["notes"], "notes.txt", { type: "text/plain" });
const item = (file) => ({ kind: "file", getAsFile: () => file });

test("clipboard text, paths and HTML without file objects stay native", () => {
  assert.equal(pastedAttachments({ files: [], items: [{ kind: "string" }, item(null)] }), null);
});

test("mixed clipboard keeps plain text and routes each file once", () => {
  assert.deepEqual(pastedAttachments({ files: [file, image], items: [item(file), item(image)],
    getData: (type) => type === "text/plain" ? "caption" : "" }),
  { images: [image], uploads: [file], hasText: true });
});

test("screenshot exposed only as an item is still attachable", () => {
  assert.deepEqual(pastedAttachments({ files: [], items: [item(image)], getData: () => "" }),
    { images: [image], uploads: [], hasText: false });
});
