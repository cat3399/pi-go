import type { SlashCommandInfo, SlashCommandSource } from "../contracts";

export type SlashCommandPaletteItem = SlashCommandInfo | {
  name: string;
  description: string;
  source: "builtin";
};

export type SlashCommandPaletteSource = SlashCommandSource | "builtin";

// Kept in lockstep with the complete WebUI palette. Dynamic commands are
// supplied by the transport-neutral get_commands application command.
export const BUILTIN_SLASH_COMMANDS: SlashCommandPaletteItem[] = [
  { name: "compact", description: "压缩上下文，可选附加说明", source: "builtin" },
  { name: "reload", description: "重新加载扩展、技能、提示词和工具", source: "builtin" },
  { name: "name", description: "设置会话显示名称", source: "builtin" },
  { name: "session", description: "显示会话消息、Token 和费用统计", source: "builtin" },
  { name: "copy", description: "复制最后一条助手消息", source: "builtin" },
];

export const SLASH_SOURCE_LABELS: Record<SlashCommandPaletteSource, string> = {
  builtin: "内置",
  extension: "扩展",
  prompt: "提示词",
  skill: "技能",
};

const sourceOrder: Record<SlashCommandPaletteSource, number> = {
  builtin: 0,
  extension: 1,
  prompt: 2,
  skill: 3,
};

export function slashQuery(value: string): string | null {
  if (!value.startsWith("/") || /[\r\n]/.test(value)) return null;
  const query = value.slice(1);
  return /\s/.test(query) ? null : query.toLocaleLowerCase();
}

function matchRank(command: SlashCommandPaletteItem, query: string): number {
  const name = command.name.toLocaleLowerCase();
  const description = command.description?.toLocaleLowerCase() ?? "";
  if (name === query) return 0;
  if (name.startsWith(query)) return 1;
  if (name.includes(query)) return 2;
  if (description.includes(query)) return 3;
  return 4;
}

export function matchSlashCommands(
  dynamic: SlashCommandInfo[],
  query: string,
  includeBuiltins: boolean,
): SlashCommandPaletteItem[] {
  const seen = new Set<string>();
  return [...(includeBuiltins ? BUILTIN_SLASH_COMMANDS : []), ...dynamic]
    .filter((command) => {
      const key = command.name.toLocaleLowerCase();
      if (!key || seen.has(key)) return false;
      seen.add(key);
      return key.includes(query) || (command.description ?? "").toLocaleLowerCase().includes(query);
    })
    .sort((left, right) => {
      const rank = matchRank(left, query) - matchRank(right, query);
      if (rank !== 0) return rank;
      const source = sourceOrder[left.source] - sourceOrder[right.source];
      if (source !== 0) return source;
      return left.name.localeCompare(right.name);
    });
}
