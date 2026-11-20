import assert from "node:assert/strict";
import test from "node:test";
import { currentPreset, matchModelPreset, modelFromService, modelPresetRequest, resolveModelDraft } from "../src/workbench/model-discovery.ts";
import { editorThinking, modelThinkingChoices, sessionDraftThinking } from "../src/workbench/model-thinking.ts";

const preset = {
  id: "openai/sample", name: "Builtin label", reasoning: true,
  thinkingLevelMap: { minimal: null, xhigh: "strong" }, input: ["text", "image"], contextWindow: 90000, maxTokens: 7000,
};
const config = { api: "openai-completions", baseUrl: "https://gateway.test", apiKey: "not-a-lookup-hint" };

test("typed and service-selected drafts import nothing until the user clicks fill", async () => {
  for (const model of [{ id: preset.id }, modelFromService({ ...preset, name: "Service label" })]) {
    assert.deepEqual(Object.keys(model).sort(), model.name ? ["id", "name"] : ["id"]);
    const row = { model: { ...model, maxTokens: 123 } };
    const request = modelPresetRequest("gateway", config, row.model);
    assert.deepEqual(request, { providerId: "gateway", models: [{ id: preset.id, api: config.api, baseUrl: config.baseUrl }] });
    const readDraft = () => resolveModelDraft(row, currentPreset(row, request));
    assert.deepEqual(readDraft(), row.model); // Also the values available to Save.
    let calls = 0;
    row.match = await matchModelPreset({ async lookupModelPresets() { calls++; return [{ ...preset, api: "builtin", cost: {}, headers: {} }]; } }, request);
    assert.deepEqual(readDraft(), { ...preset, ...row.model });
    assert.equal(calls, 1);
    assert.equal(currentPreset(row, modelPresetRequest("other", config, model)), undefined);
    assert.equal(currentPreset(row, modelPresetRequest("gateway", config, { ...model, id: "different" })), undefined);
  }
});

test("failed or ambiguous manual matches leave a saveable draft", async () => {
  const row = { model: { id: "unknown", reasoning: false } };
  const request = modelPresetRequest("gateway", config, row.model);
  await assert.rejects(matchModelPreset({ async lookupModelPresets() { return [{ id: "another" }]; } }, request), /匹配结果/);
  assert.equal(resolveModelDraft(row), row.model);
  row.match = await matchModelPreset({ async lookupModelPresets() { return [{ id: "unknown" }]; } }, request);
  assert.equal(resolveModelDraft(row, currentPreset(row, request)), row.model);
});

test("the default selector lists only checked chips and never saves a disabled level", () => {
  const row = { model: { id: preset.id, defaultThinkingLevel: "xhigh" } };
  assert.deepEqual(editorThinking(resolveModelDraft(row, preset)), { choices: ["off", "low", "medium", "high", "xhigh"], level: "xhigh" });
  const edited = resolveModelDraft({ ...row, levels: { xhigh: false, off: false, low: false } }, preset);
  assert.deepEqual(editorThinking(edited), { choices: ["medium", "high"], level: "high" });
  assert.deepEqual(editorThinking({ ...edited, reasoning: false }), { choices: ["off"], level: "off" });
  assert.deepEqual(editorThinking(resolveModelDraft({ ...row, levels: { off: false, minimal: false, low: false, medium: false, high: false, xhigh: false, max: false } }, preset)), { choices: [], level: undefined });
  assert.deepEqual(resolveModelDraft({ model: { ...preset, reasoning: false, compat: { keep: true } } }, { ...preset, reasoning: true }), { ...preset, reasoning: false, compat: { keep: true } });
});

test("composer shows the resolved Core default and sends only applicable explicit choices", () => {
  const model = { provider: "gateway", modelId: "a" };
  const item = { provider: "gateway", id: "a", defaultThinkingLevel: "high" };
  const view = { modelList: [item], thinkingLevelPins: {}, thinkingLevels: { "gateway:a": ["low", "high"] } };
  assert.deepEqual(modelThinkingChoices(view, model), ["low", "high"]);
  assert.deepEqual(sessionDraftThinking(view, model, ""), { level: "high", requested: undefined });
  assert.deepEqual(sessionDraftThinking(view, model, "low"), { level: "low", requested: "low" });
  assert.deepEqual(sessionDraftThinking(view, model, "xhigh"), { level: "high", requested: undefined });
  assert.deepEqual(sessionDraftThinking({ ...view, thinkingLevelPins: { "gateway/a": "high" } }, model, ""), { level: "high", requested: "high" });
});
