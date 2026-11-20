import type { ApplicationClient, DiscoveredModel, ModelDefinition, ModelPresetRequest, ModelProviderConfig } from "../contracts";
import { mappingEnablesLevel, thinkingLevels, type ThinkingLevel } from "./model-thinking.ts";

// Only authored fields belong in model. Keep suggestions and per-level edits
// separate until save, so changing a draft ID or route cannot carry stale defaults.
export interface ModelDraftRow {
  model: ModelDefinition;
  match?: { key: string; preset: DiscoveredModel };
  levels?: Partial<Record<ThinkingLevel, boolean>>;
}

// Listing a service selects an identity, not capabilities. Older cores may
// include inferred fields in the list response; do not import them on selection.
export function modelFromService({ id, name }: DiscoveredModel): ModelDefinition {
  return name ? { id, name } : { id };
}

export function modelPresetRequest(providerId: string, config: ModelProviderConfig, model: ModelDefinition, first?: ModelDefinition): ModelPresetRequest {
  return { providerId: providerId.trim(), models: [{ id: model.id.trim(), api: model.api || config.api || first?.api || "",
    baseUrl: (model.baseUrl || config.baseUrl || first?.baseUrl || "").trim() }] };
}

// Only the explicit fill action reads the catalog. Saving reads the draft, not the service.
export async function matchModelPreset(client: Pick<ApplicationClient, "lookupModelPresets">, request: ModelPresetRequest) {
  const values = await client.lookupModelPresets(request);
  const preset = values[0];
  if (!preset || values.length !== 1 || preset.id !== request.models[0]?.id) throw new Error("内置模型匹配结果与请求不符");
  return { key: JSON.stringify(request), preset };
}

export function currentPreset(row: ModelDraftRow, request: ModelPresetRequest) {
  return row.match && row.match.key === JSON.stringify(request) ? row.match.preset : undefined;
}

export function resolveModelDraft(row: ModelDraftRow, preset?: DiscoveredModel): ModelDefinition {
  const model = preset ? fillModelPreset(preset, row.model) : row.model;
  if (!row.levels) return model;
  let map = model.thinkingLevelMap;
  for (const level of thinkingLevels) {
    const chosen = row.levels[level];
    if (chosen === undefined || chosen === mappingEnablesLevel(map, level)) continue;
    const effort = preset?.thinkingLevelMap?.[level] ?? (level === "off" ? "none" : level);
    map = { ...map, [level]: chosen ? effort : null };
  }
  return map === model.thinkingLevelMap ? model : { ...model, thinkingLevelMap: map };
}

// Matching never changes routing, billing, auth or an explicit field (including false/null mappings).
function fillModelPreset(source: DiscoveredModel, existing: ModelDefinition): ModelDefinition {
  const fields = ["name", "reasoning", "thinkingLevelMap", "input", "contextWindow", "maxTokens"] as const;
  const additions: Record<string, unknown> = {};
  for (const field of fields) {
    if (source[field] !== undefined && existing[field] === undefined) additions[field] = source[field];
  }
  return Object.keys(additions).length ? { ...existing, ...additions } : existing;
}
