import type { ModelDefinition, ModelsView, SelectedModel } from "../contracts";

export const thinkingLevels = ["off", "minimal", "low", "medium", "high", "xhigh", "max"] as const;
export type ThinkingLevel = typeof thinkingLevels[number];
export const isThinkingLevel = (value: string): value is ThinkingLevel => (thinkingLevels as readonly string[]).includes(value);

export function mappingEnablesLevel(map: ModelDefinition["thinkingLevelMap"], level: ThinkingLevel): boolean {
  const mapped = map?.[level];
  return mapped !== null && (mapped !== undefined || (level !== "xhigh" && level !== "max"));
}

// Mirror the Core capability clamp for unsaved drafts: prefer the next higher
// supported level, then the highest lower one. No choice exists for an empty set.
function closestLevel(choices: ThinkingLevel[], preferred = "medium"): ThinkingLevel | undefined {
  const index = isThinkingLevel(preferred) ? thinkingLevels.indexOf(preferred) : -1;
  return choices.find((level) => thinkingLevels.indexOf(level) >= index) ?? choices.at(-1);
}

export function editorThinking(model: ModelDefinition) {
  const choices = thinkingLevels.filter((level) => model.reasoning ? mappingEnablesLevel(model.thinkingLevelMap, level) : level === "off");
  return { choices, level: closestLevel(choices, model.defaultThinkingLevel) };
}

export function modelThinkingChoices(models: ModelsView | null, model: SelectedModel | null) {
  return (model && models?.thinkingLevels[`${model.provider}:${model.modelId}`] || []).filter(isThinkingLevel);
}

export function sessionDraftThinking(models: ModelsView | null, model: SelectedModel | null, override: string) {
  const choices = modelThinkingChoices(models, model);
  if (!models || !model) return { level: "", requested: undefined };
  if (isThinkingLevel(override) && choices.includes(override)) return { level: override, requested: override };
  const item = models.modelList.find((m) => m.provider === model.provider && m.id === model.modelId);
  const pin = models.thinkingLevelPins[`${model.provider}/${model.modelId}`];
  const level = item?.defaultThinkingLevel ?? closestLevel(choices, pin) ?? "off";
  // Leave unpinned defaults to the Core at creation time. Older remote cores
  // don't project the resolved default: request the concrete level shown there.
  return { level, requested: pin || !item?.defaultThinkingLevel ? level : undefined };
}
