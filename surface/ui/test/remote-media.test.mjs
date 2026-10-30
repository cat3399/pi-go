import assert from "node:assert/strict";
import test from "node:test";
import { RemoteApplicationClient } from "../src/http-client.ts";
import { SessionImageCache } from "../src/workbench/session-images.ts";

const block = () => ({ type: "image", mimeType: "image/png", imageRef: { generation: "g", entryId: "u", blockIndex: 1 } });

test("native historical images use the authenticated resource request, without a direct network URL", async () => {
  const requests = [];
  const transport = {
    mediaMode: "embedded",
    async request(endpoint, path, token) {
      requests.push({ endpoint, path, token });
      const data = path.includes("/resources/image")
        ? { mimeType: "image/png", data: "YWJj" }
        : { items: [{ message: { role: "user", content: [block()] } }] };
      return { status: 200, body: JSON.stringify({ token: "resource", data }) };
    },
  };
  const client = new RemoteApplicationClient("http://core.test", transport);
  const history = await client.readResource("session", "history");
  const image = history.data.items[0].message.content[0];
  assert.equal(image.url, undefined);
  const cache = new SessionImageCache((id, query, known) => client.readResource(id, "image", query, known));
  cache.selectSession("session", "g");
  assert.equal(cache.source(image), "");
  assert.equal(await cache.load(image), "data:image/png;base64,YWJj");
  assert.equal(requests.length, 2);
  assert.equal(requests[1].token, "", "native storage supplies the credential, not JavaScript");
  const query = JSON.parse(new URL(requests[1].path, requests[1].endpoint).searchParams.get("query"));
  assert.deepEqual(query, { entryId: "u", blockIndex: 1, generation: "g" });
});

test("native workspace media uses embedded previews while browser media keeps its streaming URL", async () => {
  const paths = [];
  const transport = {
    mediaMode: "embedded",
    async request(endpoint, path) {
      paths.push(path);
      return { status: 200, body: JSON.stringify({ kind: "image", sourceUrl: "data:image/png;base64,YWJj" }) };
    },
  };
  const native = new RemoteApplicationClient("http://core.test", transport);
  assert.equal((await native.previewFile("/work/photo.png")).sourceUrl, "data:image/png;base64,YWJj");
  assert.equal(paths[0], "/api/v1/files/work/photo.png?type=embedded-preview");
  const browser = new RemoteApplicationClient("https://core.test", {
    async request() { return { status: 200, body: JSON.stringify({ mime: "image/png", size: 3 }) }; },
  });
  assert.equal((await browser.previewFile("/work/photo.png")).sourceUrl, "https://core.test/api/v1/files/work/photo.png?type=read");
});
