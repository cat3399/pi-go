import type { AgentMessage, MessageContentBlock, TokenUsageInfo } from "../contracts";

function validTokenCount(value: unknown): value is number {
  return typeof value === "number" && Number.isFinite(value) && value >= 0;
}

function assistantUsage(message: AgentMessage | null | undefined): TokenUsageInfo | null {
  if (
    message?.role !== "assistant"
    || message.stopReason === "aborted"
    || message.stopReason === "error"
  ) return null;

  const usage = message.usage;
  if (
    !usage
    || !validTokenCount(usage.input)
    || !validTokenCount(usage.output)
    || !validTokenCount(usage.cacheRead)
    || !validTokenCount(usage.cacheWrite)
  ) return null;

  const componentTotal = usage.input + usage.output + usage.cacheRead + usage.cacheWrite;
  const reportedTotal = validTokenCount(usage.totalTokens) ? usage.totalTokens : componentTotal;
  return reportedTotal > 0 || componentTotal > 0 ? usage : null;
}

export function latestAssistantUsage(
  messages: AgentMessage[],
  streamingMessage?: AgentMessage | null,
): TokenUsageInfo | null {
  const streamingUsage = assistantUsage(streamingMessage);
  if (streamingUsage) return streamingUsage;
  for (let index = messages.length - 1; index >= 0; index--) {
    const usage = assistantUsage(messages[index]);
    if (usage) return usage;
  }
  return null;
}

export function messageText(message: AgentMessage | null | undefined): string {
  if (!message) return "";
  if (message.role === "bashExecution" && typeof message.output === "string") return message.output;
  if (typeof message.content === "string") return message.content;
  if (!Array.isArray(message.content)) return "";
  return message.content
    .filter((block): block is MessageContentBlock => block?.type === "text")
    .map((block) => typeof block.text === "string" ? block.text : "")
    .filter(Boolean)
    .join("\n");
}

export function messageToolNames(message: AgentMessage): string[] {
  if (!Array.isArray(message.content)) return [];
  return message.content
    .filter((block) => block?.type === "toolCall")
    .map((block) => {
      if (typeof block.toolName === "string") return block.toolName;
      if (typeof block.name === "string") return block.name;
      return "";
    })
    .filter(Boolean);
}

export function visibleMessage(message: AgentMessage): boolean {
  if (message.role === "toolResult") return false;
  if (message.role === "custom") return message.display !== false;
  return message.role === "user" || message.role === "assistant" || message.role === "bashExecution";
}

export function sameUserText(message: AgentMessage | undefined, text: string): boolean {
  return message?.role === "user" && messageText(message) === text;
}
