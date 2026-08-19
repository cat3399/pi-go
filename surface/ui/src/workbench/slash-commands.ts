import type { SlashCommandInfo, SlashCommandSource } from "../contracts";

export type SlashCommandPaletteItem = SlashCommandInfo | {
  name: string;
  description: string;
  argumentHint?: string;
  source: "builtin";
};

export type SlashCommandPaletteSource = SlashCommandSource | "builtin" | "argument";

export interface SlashCommandArgumentItem {
  name: string;
  description: string;
  source: "argument";
}

export interface SlashPaletteQuery {
  command: string | null;
  query: string;
  prefix: string;
  argumentMode: boolean;
}

// GUI-adapted TUI built-ins. Keep this list limited to commands with a real
// cross-surface execution path; dynamic commands come from get_commands.
export const BUILTIN_SLASH_COMMANDS: SlashCommandPaletteItem[] = [
  { name: "help", description: "显示可用命令", source: "builtin" },
  { name: "new", description: "开始新会话", source: "builtin" },
  { name: "resume", description: "切换到已有会话", argumentHint: "[会话]", source: "builtin" },
  { name: "tree", description: "浏览当前会话树", source: "builtin" },
  { name: "fork", description: "从较早的用户消息分支", source: "builtin" },
  { name: "clone", description: "从当前位置克隆会话", source: "builtin" },
  { name: "model", description: "选择或切换模型", argumentHint: "[提供商/模型]", source: "builtin" },
  { name: "thinking", description: "选择推理等级", argumentHint: "[等级]", source: "builtin" },
  { name: "settings", description: "打开 GUI 设置", source: "builtin" },
  { name: "compact", description: "压缩上下文，可选附加说明", argumentHint: "[说明]", source: "builtin" },
  { name: "abort", description: "中止当前操作", source: "builtin" },
  { name: "clear-queue", description: "清空插入与稍后消息", source: "builtin" },
  { name: "reload", description: "重新加载扩展、技能、提示词和工具", source: "builtin" },
  { name: "name", description: "设置会话显示名称", argumentHint: "<名称>", source: "builtin" },
  { name: "stats", description: "显示消息、Token 和费用统计", source: "builtin" },
  { name: "copy", description: "复制最后一条助手消息", source: "builtin" },
  { name: "tools", description: "配置当前可用工具", argumentHint: "[预设]", source: "builtin" },
];

export const SLASH_SOURCE_LABELS: Record<SlashCommandPaletteSource, string> = {
  builtin: "内置",
  extension: "扩展",
  prompt: "提示词",
  skill: "技能",
  argument: "选项",
};

const sourceOrder: Record<SlashCommandPaletteSource, number> = {
  builtin: 0,
  extension: 1,
  prompt: 2,
  skill: 3,
  argument: 0,
};

export function slashQuery(value: string): SlashPaletteQuery | null {
  if (!value.startsWith("/") || /[\r\n]/.test(value)) return null;
  const space = value.search(/\s/);
  if (space < 0) {
    return { command: null, query: value.slice(1).toLocaleLowerCase(), prefix: "", argumentMode: false };
  }
  const command = value.slice(1, space).trim().toLocaleLowerCase();
  if (!command) return null;
  let argumentStart = space;
  while (argumentStart < value.length && /\s/.test(value[argumentStart] ?? "")) argumentStart += 1;
  return {
    command,
    query: value.slice(argumentStart).trim().toLocaleLowerCase(),
    prefix: value.slice(0, argumentStart),
    argumentMode: true,
  };
}

function matchRank(command: SlashCommandPaletteItem | SlashCommandArgumentItem, query: string): number {
  const name = command.name.toLocaleLowerCase();
  const description = command.description?.toLocaleLowerCase() ?? "";
  if (name === query) return 0;
  if (name.startsWith(query)) return 1;
  if (name.includes(query)) return 2;
  if (description.includes(query)) return 3;
  return 4;
}

export function matchSlashCommands(
  dynamic: Array<SlashCommandInfo | SlashCommandArgumentItem>,
  query: string,
  includeBuiltins: boolean,
): Array<SlashCommandPaletteItem | SlashCommandArgumentItem> {
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
