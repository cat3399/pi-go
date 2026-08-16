import type { AgentMessage, MessageContentBlock } from "../contracts";

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
