import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { zstdDecompressSync } from "node:zlib";

type WireScenario = {
  name: string;
  api: string;
  model: Record<string, unknown>;
  providerOptions?: Record<string, unknown>;
};

type Corpus = {
  upstreamCommit: string;
  common: {
    systemPrompt: string;
    userPrompt: string;
    sessionId: string;
    tool: Record<string, unknown>;
    options: Record<string, unknown>;
  };
  scenarios: WireScenario[];
};

const here = dirname(fileURLToPath(import.meta.url));
const corpus = JSON.parse(readFileSync(join(here, "upstream_provider_wire_corpus.json"), "utf8")) as Corpus;
const upstreamRoot = resolve(process.env.PI_UPSTREAM_ROOT ?? join(here, "../../../../pi"));

function moduleURL(path: string): string {
  return pathToFileURL(path).href;
}

function codexToken(): string {
  const header = Buffer.from(JSON.stringify({ alg: "none" }), "utf8").toString("base64url");
  const payload = Buffer.from(
    JSON.stringify({ "https://api.openai.com/auth": { chatgpt_account_id: "acct-wire" } }),
    "utf8",
  ).toString("base64url");
  return `${header}.${payload}.signature`;
}

function apiKeyFor(api: string): string {
  if (api === "openai-codex-responses") return codexToken();
  if (api === "anthropic-messages") return "oracle-anthropic-key";
  return "oracle-openai-key";
}

function anthropicSSE(): string {
  const events: Array<{ event: string; data: Record<string, unknown> }> = [
    {
      event: "message_start",
      data: {
        type: "message_start",
        message: {
          id: "msg_anthropic_wire",
          usage: {
            input_tokens: 12,
            output_tokens: 0,
            cache_read_input_tokens: 2,
            cache_creation_input_tokens: 1,
            cache_creation: { ephemeral_1h_input_tokens: 1 },
          },
        },
      },
    },
    {
      event: "content_block_start",
      data: {
        type: "content_block_start",
        index: 0,
        content_block: { type: "thinking", thinking: "", signature: "" },
      },
    },
    {
      event: "content_block_delta",
      data: {
        type: "content_block_delta",
        index: 0,
        delta: { type: "thinking_delta", thinking: "plan" },
      },
    },
    {
      event: "content_block_delta",
      data: {
        type: "content_block_delta",
        index: 0,
        delta: { type: "signature_delta", signature: "anthropic-signature" },
      },
    },
    { event: "content_block_stop", data: { type: "content_block_stop", index: 0 } },
    {
      event: "content_block_start",
      data: { type: "content_block_start", index: 1, content_block: { type: "text", text: "" } },
    },
    {
      event: "content_block_delta",
      data: { type: "content_block_delta", index: 1, delta: { type: "text_delta", text: "answer " } },
    },
    { event: "content_block_stop", data: { type: "content_block_stop", index: 1 } },
    {
      event: "content_block_start",
      data: {
        type: "content_block_start",
        index: 2,
        content_block: { type: "tool_use", id: "call_1", name: "lookup", input: {} },
      },
    },
    {
      event: "content_block_delta",
      data: {
        type: "content_block_delta",
        index: 2,
        delta: { type: "input_json_delta", partial_json: "{\"query\":\"pi-go\"}" },
      },
    },
    { event: "content_block_stop", data: { type: "content_block_stop", index: 2 } },
    {
      event: "message_delta",
      data: {
        type: "message_delta",
        delta: { stop_reason: "tool_use" },
        usage: {
          input_tokens: 12,
          output_tokens: 8,
          cache_read_input_tokens: 2,
          cache_creation_input_tokens: 1,
          cache_creation: { ephemeral_1h_input_tokens: 1 },
          output_tokens_details: { thinking_tokens: 3 },
        },
      },
    },
    { event: "message_stop", data: { type: "message_stop" } },
  ];
  return events.map(({ event, data }) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`).join("");
}

function completionsSSE(): string {
  const chunks: Record<string, unknown>[] = [
    {
      id: "chatcmpl-wire",
      model: "gpt-wire-chat-actual",
      choices: [{ index: 0, delta: { reasoning_content: "plan" }, finish_reason: null }],
    },
    {
      id: "chatcmpl-wire",
      model: "gpt-wire-chat-actual",
      choices: [{ index: 0, delta: { content: "answer " }, finish_reason: null }],
    },
    {
      id: "chatcmpl-wire",
      model: "gpt-wire-chat-actual",
      choices: [
        {
          index: 0,
          delta: {
            reasoning_details: [{ type: "reasoning.encrypted", id: "call_1", data: "completion-signature" }],
          },
          finish_reason: null,
        },
      ],
    },
    {
      id: "chatcmpl-wire",
      model: "gpt-wire-chat-actual",
      choices: [
        {
          index: 0,
          delta: {
            tool_calls: [
              {
                index: 0,
                id: "call_1",
                type: "function",
                function: { name: "lookup", arguments: "{\"query\":" },
              },
            ],
          },
          finish_reason: null,
        },
      ],
    },
    {
      id: "chatcmpl-wire",
      model: "gpt-wire-chat-actual",
      choices: [
        {
          index: 0,
          delta: { tool_calls: [{ index: 0, function: { arguments: "\"pi-go\"}" } }] },
          finish_reason: null,
        },
      ],
    },
    {
      id: "chatcmpl-wire",
      model: "gpt-wire-chat-actual",
      choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }],
    },
    {
      id: "chatcmpl-wire",
      model: "gpt-wire-chat-actual",
      choices: [],
      usage: {
        prompt_tokens: 20,
        completion_tokens: 8,
        total_tokens: 28,
        prompt_tokens_details: { cached_tokens: 3, cache_write_tokens: 2 },
        completion_tokens_details: { reasoning_tokens: 3 },
      },
    },
  ];
  return `${chunks.map((chunk) => `data: ${JSON.stringify(chunk)}\n\n`).join("")}data: [DONE]\n\n`;
}

function responsesSSE(prefix: "responses" | "codex"): string {
  const responseId = `resp_${prefix}_wire`;
  const reasoningItem = {
    type: "reasoning",
    id: `rs_${prefix}_wire`,
    summary: [{ type: "summary_text", text: "plan" }],
    encrypted_content: `${prefix}-encrypted`,
  };
  const events: Record<string, unknown>[] = [
    { type: "response.created", sequence_number: 0, response: { id: responseId } },
    {
      type: "response.output_item.added",
      sequence_number: 1,
      output_index: 0,
      item: { type: "reasoning", id: reasoningItem.id, summary: [] },
    },
    {
      type: "response.reasoning_summary_text.delta",
      sequence_number: 2,
      output_index: 0,
      summary_index: 0,
      delta: "plan",
    },
    {
      type: "response.output_item.done",
      sequence_number: 3,
      output_index: 0,
      item: reasoningItem,
    },
    {
      type: "response.output_item.added",
      sequence_number: 4,
      output_index: 1,
      item: { type: "message", id: `msg_${prefix}_wire`, role: "assistant", status: "in_progress", content: [] },
    },
    {
      type: "response.output_text.delta",
      sequence_number: 5,
      output_index: 1,
      content_index: 0,
      item_id: `msg_${prefix}_wire`,
      delta: "answer ",
    },
    {
      type: "response.output_item.done",
      sequence_number: 6,
      output_index: 1,
      item: {
        type: "message",
        id: `msg_${prefix}_wire`,
        role: "assistant",
        status: "completed",
        phase: "final_answer",
        content: [{ type: "output_text", text: "answer ", annotations: [] }],
      },
    },
    {
      type: "response.output_item.added",
      sequence_number: 7,
      output_index: 2,
      item: { type: "function_call", id: `fc_${prefix}_wire`, call_id: "call_1", name: "lookup", arguments: "" },
    },
    {
      type: "response.function_call_arguments.delta",
      sequence_number: 8,
      output_index: 2,
      item_id: `fc_${prefix}_wire`,
      delta: "{\"query\":\"pi-go\"}",
    },
    {
      type: "response.output_item.done",
      sequence_number: 9,
      output_index: 2,
      item: {
        type: "function_call",
        id: `fc_${prefix}_wire`,
        call_id: "call_1",
        name: "lookup",
        arguments: "{\"query\":\"pi-go\"}",
      },
    },
    {
      type: "response.completed",
      sequence_number: 10,
      response: {
        id: responseId,
        status: "completed",
        service_tier: prefix === "codex" ? "flex" : "priority",
        output: [reasoningItem],
        usage: {
          input_tokens: 20,
          output_tokens: 8,
          total_tokens: 28,
          input_tokens_details: { cached_tokens: 3, cache_write_tokens: 2 },
          output_tokens_details: { reasoning_tokens: 3 },
        },
      },
    },
  ];
  return `${events.map((event) => `data: ${JSON.stringify(event)}\n\n`).join("")}data: [DONE]\n\n`;
}

function responseBodyFor(api: string): string {
  switch (api) {
    case "anthropic-messages":
      return anthropicSSE();
    case "openai-completions":
      return completionsSSE();
    case "openai-responses":
      return responsesSSE("responses");
    case "openai-codex-responses":
      return responsesSSE("codex");
    default:
      throw new Error(`unsupported provider wire scenario ${api}`);
  }
}

const observedHeaderNames = [
  "accept",
  "content-type",
  "content-encoding",
  "authorization",
  "x-api-key",
  "anthropic-version",
  "anthropic-beta",
  "x-session-affinity",
  "session_id",
  "session-id",
  "x-client-request-id",
  "x-session-id",
  "chatgpt-account-id",
  "openai-beta",
  "originator",
  "x-oracle",
] as const;

function selectedHeaders(headers: Headers): Record<string, string | null> {
  return Object.fromEntries(observedHeaderNames.map((name) => [name, headers.get(name)]));
}

async function requestBytes(request: Request): Promise<Buffer> {
  const encoded = Buffer.from(await request.arrayBuffer());
  return request.headers.get("content-encoding") === "zstd" ? zstdDecompressSync(encoded) : encoded;
}

function normalizeSignature(signature: string | undefined): unknown {
  if (!signature) return null;
  try {
    return JSON.parse(signature);
  } catch {
    return signature;
  }
}

function normalizeContentBlock(block: any): Record<string, unknown> {
  if (block.type === "text") {
    return {
      type: "text",
      text: block.text,
      signature: normalizeSignature(block.textSignature),
    };
  }
  if (block.type === "thinking") {
    return {
      type: "thinking",
      thinking: block.thinking,
      signature: normalizeSignature(block.thinkingSignature),
      redacted: block.redacted ?? false,
    };
  }
  if (block.type === "toolCall") {
    return {
      type: "toolCall",
      id: block.id,
      name: block.name,
      arguments: block.arguments,
      thoughtSignature: normalizeSignature(block.thoughtSignature),
    };
  }
  throw new Error(`unknown content block ${String(block?.type)}`);
}

function normalizeUsage(usage: any): Record<string, unknown> {
  const normalized: Record<string, unknown> = {
    input: usage.input,
    output: usage.output,
    cacheRead: usage.cacheRead,
    cacheWrite: usage.cacheWrite,
    totalTokens: usage.totalTokens,
  };
  if (usage.reasoning !== undefined) normalized.reasoning = usage.reasoning;
  if (usage.cacheWrite1h !== undefined) normalized.cacheWrite1h = usage.cacheWrite1h;
  return normalized;
}

function normalizeMessage(message: any): Record<string, unknown> {
  const normalized: Record<string, unknown> = {
    role: message.role,
    api: message.api,
    provider: message.provider,
    model: message.model,
    content: message.content.map(normalizeContentBlock),
    usage: normalizeUsage(message.usage),
    stopReason: message.stopReason,
  };
  if (message.responseId !== undefined) normalized.responseId = message.responseId;
  if (message.responseModel !== undefined) normalized.responseModel = message.responseModel;
  if (message.rawStopReason !== undefined) normalized.rawStopReason = message.rawStopReason;
  if (message.errorMessage !== undefined) normalized.errorMessage = message.errorMessage;
  return normalized;
}

function normalizeEvent(event: any): Record<string, unknown> {
  const normalized: Record<string, unknown> = { type: event.type };
  if (event.contentIndex !== undefined) normalized.contentIndex = event.contentIndex;
  if (event.delta !== undefined) normalized.delta = event.delta;
  if (event.content !== undefined) normalized.content = event.content;
  if (event.reason !== undefined) normalized.reason = event.reason;
  if (event.type === "toolcall_start") {
    const block = event.partial.content[event.contentIndex];
    normalized.id = block.id;
    normalized.name = block.name;
  }
  if (event.type === "toolcall_end") normalized.toolCall = normalizeContentBlock(event.toolCall);
  if (event.type === "thinking_end") {
    const block = event.partial.content[event.contentIndex];
    normalized.signature = normalizeSignature(block.thinkingSignature);
    normalized.redacted = block.redacted ?? false;
  }
  return normalized;
}

async function runScenario(
  scenario: WireScenario,
  modules: Record<string, { stream: (model: any, context: any, options: any) => any }>,
): Promise<Record<string, unknown>> {
  let wireRequest: Record<string, unknown> | undefined;
  const fetch = async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const request = input instanceof Request ? input.clone() : new Request(input, init);
    const bytes = await requestBytes(request);
    wireRequest = {
      url: request.url,
      method: request.method,
      headers: selectedHeaders(request.headers),
      body: JSON.parse(bytes.toString("utf8")),
    };
    return new Response(responseBodyFor(scenario.api), {
      status: 200,
      headers: { "content-type": "text/event-stream" },
    });
  };
  const context = {
    systemPrompt: corpus.common.systemPrompt,
    messages: [{ role: "user", content: corpus.common.userPrompt, timestamp: 0 }],
    tools: [corpus.common.tool],
  };
  const options = {
    ...corpus.common.options,
    ...scenario.providerOptions,
    apiKey: apiKeyFor(scenario.api),
    sessionId: corpus.common.sessionId,
    fetch,
  };
  const stream = modules[scenario.api].stream(scenario.model, context, options);
  const events: Record<string, unknown>[] = [];
  for await (const event of stream) events.push(normalizeEvent(event));
  const result = await stream.result();
  if (!wireRequest) throw new Error(`${scenario.name} did not issue an HTTP request`);
  return {
    name: scenario.name,
    api: scenario.api,
    input: {
      model: scenario.model,
      systemPrompt: corpus.common.systemPrompt,
      userPrompt: corpus.common.userPrompt,
      sessionId: corpus.common.sessionId,
      tool: corpus.common.tool,
      options: { ...corpus.common.options, ...scenario.providerOptions },
    },
    request: wireRequest,
    events,
    result: normalizeMessage(result),
  };
}

async function main(): Promise<void> {
  const commit = execFileSync("git", ["rev-parse", "HEAD"], { cwd: upstreamRoot, encoding: "utf8" }).trim();
  if (commit !== corpus.upstreamCommit) {
    throw new Error(`upstream commit ${commit}, expected ${corpus.upstreamCommit}`);
  }
  const apiRoot = join(upstreamRoot, "packages/ai/src/api");
  const modules = {
    "anthropic-messages": await import(moduleURL(join(apiRoot, "anthropic-messages.ts"))),
    "openai-completions": await import(moduleURL(join(apiRoot, "openai-completions.ts"))),
    "openai-responses": await import(moduleURL(join(apiRoot, "openai-responses.ts"))),
    "openai-codex-responses": await import(moduleURL(join(apiRoot, "openai-codex-responses.ts"))),
  };
  const scenarios = [];
  for (const scenario of corpus.scenarios) scenarios.push(await runScenario(scenario, modules));
  process.stdout.write(
    `${JSON.stringify(
      {
        upstreamCommit: commit,
        generatedBy: "pinned packages/ai provider adapters with deterministic HTTP/SSE transport",
        generator: {
          nodeVersion: process.version,
          corpus: "upstream_provider_wire_corpus.json",
        },
        scenarios,
      },
      null,
      2,
    )}\n`,
  );
}

void main().catch((error) => {
  process.stderr.write(`${error instanceof Error ? error.stack ?? error.message : String(error)}\n`);
  process.exitCode = 1;
});
