import type {
  AgentMessage,
  ApplicationClient,
  ApplicationOverview,
  ApplicationReadRequest,
  ApplicationResources,
  ApplicationSnapshot,
  AuthState,
  CreateSessionRequest,
  CreateSessionResult,
  DirectoryView,
  DraftText,
  DraftTextUpdate,
  DraftTextWriteResult,
  EntryNode,
  EventObserver,
  EventSubscription,
  FileEntry,
  FileList,
  FilePreview,
  HistoryItem,
  HistoryPage,
  HistoryRun,
  LiveSnapshot,
  ModelDiscovery,
  ModelProviderInfo,
  ModelSyncResult,
  ModelsConfig,
  ModelsView,
  NodesPage,
  ProjectDirectoryPage,
  ProjectInfo,
  ProviderModelInfo,
  ReadResourceRequest,
  ResourceResponse,
  RunsPage,
  SessionDirectoryPage,
  SessionInfo,
  SessionMetadata,
  SessionResources,
  SessionStatsInfo,
  SessionView,
  SlashCommandInfo,
  UploadResult,
  UploadTargetInspection,
} from "@cat3399/pi-workbench";

interface MockEntry {
  id: string;
  parentId: string | null;
  type: string;
  role?: string;
  timestamp: string;
  message: AgentMessage;
  preview: string;
}

interface MockTurnDef {
  id: string;
  entries: MockEntry[];
  resultEntryId?: string;
  stopReason?: string;
}

interface MockSessionData {
  info: SessionInfo;
  generation: string;
  model: { provider: string; modelId: string; name: string };
  thinkingLevel: string;
  turns: MockTurnDef[];
  live?: Partial<LiveSnapshot>;
  contextUsage?: { percent: number | null; contextWindow: number; tokens: number | null };
}

const SAMPLE_SVG_DATA_URI =
  "data:image/svg+xml;base64," +
  btoa(
    `<svg xmlns="http://www.w3.org/2000/svg" width="640" height="360" viewBox="0 0 640 360">
      <defs>
        <linearGradient id="bg" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0%" stop-color="#f7f7f6"/>
          <stop offset="100%" stop-color="#ecebe8"/>
        </linearGradient>
      </defs>
      <rect width="640" height="360" rx="20" fill="url(#bg)"/>
      <rect x="40" y="44" width="160" height="272" rx="14" fill="#ffffff" stroke="#e4e4e7"/>
      <rect x="58" y="70" width="96" height="10" rx="5" fill="#d4d4d8"/>
      <rect x="58" y="96" width="124" height="10" rx="5" fill="#e4e4e7"/>
      <rect x="58" y="122" width="110" height="10" rx="5" fill="#e4e4e7"/>
      <rect x="224" y="44" width="376" height="184" rx="16" fill="#ffffff" stroke="#e4e4e7"/>
      <rect x="252" y="76" width="210" height="12" rx="6" fill="#27272a" opacity="0.82"/>
      <rect x="252" y="106" width="308" height="10" rx="5" fill="#a1a1aa" opacity="0.55"/>
      <rect x="252" y="128" width="276" height="10" rx="5" fill="#a1a1aa" opacity="0.45"/>
      <rect x="252" y="150" width="240" height="10" rx="5" fill="#a1a1aa" opacity="0.35"/>
      <rect x="224" y="248" width="376" height="68" rx="22" fill="#ffffff" stroke="#d4d4d8"/>
      <circle cx="566" cy="282" r="16" fill="#18181b"/>
    </svg>`,
  );

const PROJECTS: ProjectInfo[] = [
  {
    path: "/Users/dev/Documents/code/pi-package/pi-go",
    modified: "2026-11-21T07:10:00Z",
    sessionCount: 4,
  },
  {
    path: "/Users/dev/Documents/code/enterprise-platform-monorepo-service-gateway-v2",
    modified: "2026-11-21T06:20:00Z",
    sessionCount: 2,
  },
  {
    path: "/Users/dev/Documents/code/design-system-tokens",
    modified: "2026-11-20T18:30:00Z",
    sessionCount: 1,
  },
  {
    path: "/Users/dev/Documents/code/empty-sandbox-workspace",
    modified: "2026-11-19T12:00:00Z",
    sessionCount: 0,
  },
];

function entryTextPreview(message: AgentMessage): string {
  if (typeof message.content === "string") return message.content.slice(0, 120);
  if (Array.isArray(message.content)) {
    const firstText = message.content.find((block) => block.type === "text" && typeof block.text === "string");
    if (firstText?.text) return firstText.text.slice(0, 120);
  }
  return message.role ?? "entry";
}

function makeEntry(
  id: string,
  parentId: string | null,
  timestamp: string,
  message: AgentMessage,
  type = "message",
): MockEntry {
  return {
    id,
    parentId,
    type,
    role: message.role,
    timestamp,
    message,
    preview: entryTextPreview(message),
  };
}

function buildMockSessions(): Map<string, MockSessionData> {
  const sessions = new Map<string, MockSessionData>();

  // 1. 复杂过程流（思考 + 多文件读取 + 搜索 + Diff + 正常命令 + 失败命令 + 修复复测）
  const p1 = "/Users/dev/Documents/code/pi-package/pi-go";
  const processTurn1: MockEntry[] = [
    makeEntry("p1-e1", null, "2026-11-21T06:40:00Z", {
      role: "user",
      content: "帮我排查一下 session 重放时的工具结果丢失问题，顺便把相关的单元测试跑一遍。",
      timestamp: Date.parse("2026-11-21T06:40:00Z"),
    }),
    makeEntry("p1-e2", "p1-e1", "2026-11-21T06:40:04Z", {
      role: "assistant",
      timestamp: Date.parse("2026-11-21T06:40:04Z"),
      content: [
        {
          type: "thinking",
          thinking:
            "梳理 session 重放链路，先定位 toolResult 索引与过滤逻辑\n" +
            "先检查 `internal/session/manager.go` 和 `internal/session/projection.go` 中对 `toolResult` 条目的读取逻辑：\n" +
            "1. 确认 branch 切换后 `entryIds` 与 `toolCallId` 映射是否保持一致；\n" +
            "2. 检查 compaction 之后的截断边界，是否误把最后一组工具结果裁掉了。",
        },
        {
          type: "toolCall",
          id: "tc-read-1",
          name: "read",
          arguments: { path: "internal/session/manager.go", offset: 1, limit: 120 },
        },
        {
          type: "toolCall",
          id: "tc-read-2",
          name: "read",
          arguments: { path: "internal/session/projection.go", offset: 40, limit: 140 },
        },
        {
          type: "toolCall",
          id: "tc-read-3",
          name: "read",
          arguments: { path: "internal/session/replay_test.go", offset: 1, limit: 90 },
        },
      ],
    }),
    makeEntry("p1-e3", "p1-e2", "2026-11-21T06:40:06Z", {
      role: "toolResult",
      toolCallId: "tc-read-1",
      toolName: "read",
      content: [{ type: "text", text: "package session\n\n// Manager owns branch tree, active leaf and compaction boundaries.\ntype Manager struct {\n\tstore Store\n\tleafID string\n}" }],
    }),
    makeEntry("p1-e4", "p1-e3", "2026-11-21T06:40:07Z", {
      role: "toolResult",
      toolCallId: "tc-read-2",
      toolName: "read",
      content: [{ type: "text", text: "func ProjectHistory(entries []Entry, leafID string) []Entry {\n\t// Filter orphan tool results when compacting.\n\treturn compactOrphans(entries)\n}" }],
    }),
    makeEntry("p1-e5", "p1-e4", "2026-11-21T06:40:08Z", {
      role: "toolResult",
      toolCallId: "tc-read-3",
      toolName: "read",
      content: [{ type: "text", text: "func TestReplayPreservesToolResults(t *testing.T) {\n\t// Existing replay test only covers single-tool turns.\n}" }],
    }),
    makeEntry("p1-e6", "p1-e5", "2026-11-21T06:40:10Z", {
      role: "assistant",
      timestamp: Date.parse("2026-11-21T06:40:10Z"),
      content: [
        {
          type: "toolCall",
          id: "tc-grep-1",
          name: "grep",
          arguments: { pattern: "compactOrphans", path: "internal/session" },
        },
      ],
    }),
    makeEntry("p1-e7", "p1-e6", "2026-11-21T06:40:11Z", {
      role: "toolResult",
      toolCallId: "tc-grep-1",
      toolName: "grep",
      content: [{ type: "text", text: "internal/session/projection.go:88: func compactOrphans(entries []Entry) []Entry {" }],
    }),
    makeEntry("p1-e8", "p1-e7", "2026-11-21T06:40:15Z", {
      role: "assistant",
      timestamp: Date.parse("2026-11-21T06:40:15Z"),
      content: [
        {
          type: "toolCall",
          id: "tc-edit-1",
          name: "edit",
          arguments: {
            path: "internal/session/projection.go",
            edits: [
              {
                oldText: "if entry.Role == \"toolResult\" && !seenCalls[entry.ToolCallID] {\n\tcontinue\n}",
                newText: "if entry.Role == \"toolResult\" && !seenCalls[entry.ToolCallID] && !anchoredCalls[entry.ToolCallID] {\n\tcontinue\n}",
              },
            ],
          },
        },
        {
          type: "toolCall",
          id: "tc-bash-1",
          name: "bash",
          arguments: { command: "go test -race ./internal/session/..." },
        },
      ],
    }),
    makeEntry("p1-e9", "p1-e8", "2026-11-21T06:40:17Z", {
      role: "toolResult",
      toolCallId: "tc-edit-1",
      toolName: "edit",
      content: [{ type: "text", text: "Updated internal/session/projection.go" }],
      details: {
        patch: [
          "@@ -86,11 +86,15 @@ func compactOrphans(entries []Entry) []Entry {",
          " \tseenCalls := make(map[string]bool)",
          "+\tanchoredCalls := collectAnchoredToolCalls(entries)",
          " \tout := make([]Entry, 0, len(entries))",
          " \tfor _, entry := range entries {",
          "-\t\tif entry.Role == \"toolResult\" && !seenCalls[entry.ToolCallID] {",
          "-\t\t\tcontinue",
          "-\t\t}",
          "+\t\tif entry.Role == \"toolResult\" {",
          "+\t\t\tif !seenCalls[entry.ToolCallID] && !anchoredCalls[entry.ToolCallID] {",
          "+\t\t\t\tcontinue",
          "+\t\t\t}",
          "+\t\t}",
          " \t\tout = append(out, entry)",
          " \t}",
        ].join("\n"),
      },
    }),
    makeEntry("p1-e10", "p1-e9", "2026-11-21T06:40:22Z", {
      role: "toolResult",
      toolCallId: "tc-bash-1",
      toolName: "bash",
      content: [
        {
          type: "text",
          text: "ok  \tgithub.com/cat3399/pi-go/internal/session\t0.418s\nPASS\n",
        },
      ],
    }),
    makeEntry("p1-e11", "p1-e10", "2026-11-21T06:40:26Z", {
      role: "assistant",
      timestamp: Date.parse("2026-11-21T06:40:26Z"),
      content: [
        {
          type: "text",
          text:
            "问题定位清楚了，并已经修复通过测试。\n\n" +
            "### 根因\n" +
            "在 `internal/session/projection.go` 的 `compactOrphans` 中，重放跨越 compaction 边界时，只记录了当前窗口内新出现的 `seenCalls`，没有把锚点之前的活跃调用放进白名单，导致紧跟在压缩点之后的 `toolResult` 被当成孤儿条目静默丢弃。\n\n" +
            "### 处理内容\n" +
            "- 补齐了 `collectAnchoredToolCalls`，在投影阶段保留锚点关联的工具调用 ID；\n" +
            "- 执行 `go test -race ./internal/session/...`，原有及新增用例全部通过。",
        },
      ],
      stopReason: "stop",
    }),
  ];

  const processTurn2: MockEntry[] = [
    makeEntry("p1-e12", "p1-e11", "2026-11-21T06:45:00Z", {
      role: "user",
      content: "顺手跑一下全量 `internal/application` 和 `surface/web` 的测试，确认没有回归。",
      timestamp: Date.parse("2026-11-21T06:45:00Z"),
    }),
    makeEntry("p1-e13", "p1-e12", "2026-11-21T06:45:04Z", {
      role: "assistant",
      timestamp: Date.parse("2026-11-21T06:45:04Z"),
      content: [
        {
          type: "toolCall",
          id: "tc-bash-fail",
          name: "bash",
          arguments: {
            command:
              "go test -v -race -count=1 -run 'TestSessionRunHistoryProjection|TestCompactionAnchorPreservation' ./internal/application/... ./internal/session/... ./surface/web/...",
          },
        },
      ],
    }),
    makeEntry("p1-e14", "p1-e13", "2026-11-21T06:45:11Z", {
      role: "toolResult",
      toolCallId: "tc-bash-fail",
      toolName: "bash",
      isError: true,
      content: [
        {
          type: "text",
          text:
            "--- FAIL: TestSessionRunHistoryProjection (0.04s)\n" +
            "    history_test.go:214: expected 3 tool results in run window, got 2\n" +
            "    history_test.go:218: missing toolCallId=tc_8f31a in compacted anchor\n" +
            "FAIL\tgithub.com/cat3399/pi-go/internal/application\t0.612s\n" +
            "exit status 1",
        },
      ],
    }),
    makeEntry("p1-e15", "p1-e14", "2026-11-21T06:45:16Z", {
      role: "assistant",
      timestamp: Date.parse("2026-11-21T06:45:16Z"),
      content: [
        {
          type: "thinking",
          thinking:
            "应用层的 run 投影测试失败了，说明应用层缓存没有把锚点 ID 传给投影器\n" +
            "检查 `internal/application/history.go:142`，这里构建窗口切片时把 `anchorEntry` 裁掉了。",
        },
        {
          type: "toolCall",
          id: "tc-edit-2",
          name: "edit",
          arguments: { path: "internal/application/history.go" },
        },
        {
          type: "toolCall",
          id: "tc-bash-pass2",
          name: "bash",
          arguments: { command: "go test -race ./internal/application/... ./surface/web/..." },
        },
      ],
    }),
    makeEntry("p1-e16", "p1-e15", "2026-11-21T06:45:19Z", {
      role: "toolResult",
      toolCallId: "tc-edit-2",
      toolName: "edit",
      content: [{ type: "text", text: "Updated internal/application/history.go" }],
      details: {
        patch: [
          "@@ -139,7 +139,8 @@ func buildRunWindow(entries []session.Entry, run RunRange) []session.Entry {",
          " \tif run.Start > 0 && entries[run.Start-1].Type == session.EntryCompaction {",
          "-\t\treturn entries[run.Start:run.End]",
          "+\t\treturn session.ProjectWithAnchor(entries[run.Start-1], entries[run.Start:run.End])",
          " \t}",
          " \treturn entries[run.Start:run.End]",
          " }",
        ].join("\n"),
      },
    }),
    makeEntry("p1-e17", "p1-e16", "2026-11-21T06:45:25Z", {
      role: "toolResult",
      toolCallId: "tc-bash-pass2",
      toolName: "bash",
      content: [
        {
          type: "text",
          text:
            "ok  \tgithub.com/cat3399/pi-go/internal/application\t0.594s\n" +
            "ok  \tgithub.com/cat3399/pi-go/surface/web\t0.241s\n",
        },
      ],
    }),
    makeEntry("p1-e18", "p1-e17", "2026-11-21T06:45:29Z", {
      role: "assistant",
      timestamp: Date.parse("2026-11-21T06:45:29Z"),
      content: [
        {
          type: "text",
          text:
            "第一遍跑 `internal/application` 时确实暴露出一条边界回归：`TestSessionRunHistoryProjection` 在 run 起点紧邻 compaction 节点时缺少锚点上下文。\n\n" +
            "已在 `internal/application/history.go` 改为调用 `session.ProjectWithAnchor`，随后复测 `internal/application` 与 `surface/web` 全部通过（含 `-race`）。",
        },
      ],
      stopReason: "stop",
    }),
  ];

  sessions.set("mock-process-full", {
    info: {
      id: "mock-process-full",
      path: `${p1}/.pi/sessions/mock-process-full.jsonl`,
      cwd: p1,
      projectRoot: p1,
      name: "排查工具折叠与命令报错",
      created: "2026-11-21T06:40:00Z",
      modified: "2026-11-21T06:45:29Z",
      messageCount: 18,
      firstMessage: "帮我排查一下 session 重放时的工具结果丢失问题",
    },
    generation: "gen-process-1",
    model: { provider: "anthropic", modelId: "claude-opus-4-6", name: "Claude Opus 4.6" },
    thinkingLevel: "medium",
    contextUsage: { percent: 28, contextWindow: 200000, tokens: 56420 },
    turns: [
      { id: "p1-e1", entries: processTurn1, resultEntryId: "p1-e11", stopReason: "stop" },
      { id: "p1-e12", entries: processTurn2, resultEntryId: "p1-e18", stopReason: "stop" },
    ],
  });

  // 2. 深度 Markdown 排版 + 宽表 + 代码块 + 公式 + 图片附件 + 上下文压缩摘要
  const mdTurn1: MockEntry[] = [
    makeEntry("md-e1", null, "2026-11-21T05:10:00Z", {
      role: "user",
      timestamp: Date.parse("2026-11-21T05:10:00Z"),
      content: [
        {
          type: "text",
          text: "这是我画的布局草图，帮我整理一份桌面 / Web / 移动三端的布局对照表，并给出核心状态同步的 Go 示例。",
        },
        {
          type: "image",
          url: SAMPLE_SVG_DATA_URI,
          mimeType: "image/svg+xml",
          byteSize: 18420,
        },
      ],
    }),
    makeEntry("md-e2", "md-e1", "2026-11-21T05:10:18Z", {
      role: "assistant",
      timestamp: Date.parse("2026-11-21T05:10:18Z"),
      content: [
        {
          type: "text",
          text:
            "收到，这套草图的留白比例非常舒服。下面按**三端一致但保留平台必要差异**的思路，把关键适配点整理成对照表：\n\n" +
            "| 维度 | Desktop (macOS / Wails) | Web (Next.js / 浏览器) | Mobile (Android / 触控) | 核心状态归属 | 备注与退化策略 |\n" +
            "| :--- | :--- | :--- | :--- | :--- | :--- |\n" +
            "| 顶部导航 | 透明标题栏 + 交通灯安全占位 | 极简平铺顶栏，无额外装饰 | 单手可达顶栏 + 侧滑抽屉手势 | `PiWorkbench` 本地 UI 态 | 窄屏自动隐藏冗余次要按钮 |\n" +
            "| 侧边栏分区 | 可拖拽宽度（`240px–360px`） | 同桌面端，支持折叠收起 | 全高抽屉浮层 + 柔和遮罩 | `useResizableSidebar` | 依赖极淡底色差，不画重边框 |\n" +
            "| 过程折叠块 | 默认折叠，单行轻盈摘要 | 默认折叠，hover 柔和反馈 | 默认折叠，点击热区 >= 44px | `ApplicationDataStore` | 静默成功，仅运行中/失败提示 |\n" +
            "| 底部输入区 | 居中大圆角悬浮卡片（`24px`） | 同桌面端，底部柔和渐隐 | 贴底适配虚拟键盘与安全区 | `Composer` + `drafts` | 支持拖拽上传与粘贴图片 |\n\n" +
            "> 核心原则很简单：让视觉边界退居幕后，把注意力完整还给正在阅读的内容和即将输入的问题。\n\n" +
            "### 事件顺序与复杂度\n\n" +
            "在 `ApplicationDataStore` 中，窗口归并的时间复杂度控制在 $O(k \\log k)$，其中单次增量追平满足：\n\n" +
            "$$\\Delta_{\\text{window}} = \\min(\\text{pageLimit},\\; N_{\\text{tail}} - N_{\\text{cursor}})$$\n\n" +
            "```go\n" +
            "package application\n\n" +
            "import \"context\"\n\n" +
            "// DispatchCommand forwards a transport-agnostic command to the active session runtime without leaking HTTP/SSE framing into the core domain layer.\n" +
            "func (s *Service) DispatchCommand(ctx context.Context, sessionID string, cmd Command, opts ...DispatchOption) (*DispatchReceipt, error) {\n" +
            "\trt, err := s.acquireRuntimeWithPolicy(ctx, sessionID, DefaultAdmissionPolicy{AllowReadOnlyFallback: true, RetryOnWriterConflict: true})\n" +
            "\tif err != nil {\n" +
            "\t\treturn nil, err\n" +
            "\t}\n" +
            "\treturn rt.Execute(ctx, cmd, opts...)\n" +
            "}\n" +
            "```\n\n" +
            "如果你希望进一步验证极窄视口下的表格横向滚动与代码折行，可以直接把窗口拉窄到 `390px` 观察。",
        },
      ],
      stopReason: "stop",
    }),
  ];

  const mdCompactionTurn: MockEntry[] = [
    makeEntry(
      "md-c1",
      "md-e2",
      "2026-11-21T05:16:00Z",
      {
        role: "compactionSummary",
        content:
          "### 已压缩的早期讨论摘要\n\n" +
          "- 已确定三端共享 `@cat3399/pi-workbench`，遵循 ChatGPT / Claude 风格的圆润宽松视觉基调；\n" +
          "- 已明确工具调用默认折叠、成功静默、失败显性；\n" +
          "- 接下来继续验证移动端安全区与深层目录文件预览。",
        details: {
          strategy: "compaction",
          tokensBefore: 128450,
          focus: "保留视觉基调与三端适配结论",
        },
        timestamp: Date.parse("2026-11-21T05:16:00Z"),
      },
      "compaction",
    ),
  ];

  const mdTurn2: MockEntry[] = [
    makeEntry("md-e3", "md-c1", "2026-11-21T05:18:00Z", {
      role: "user",
      content: "再顺便看一眼 JSON 配置块和无序/有序嵌套列表的间距表现。",
      timestamp: Date.parse("2026-11-21T05:18:00Z"),
    }),
    makeEntry("md-e4", "md-e3", "2026-11-21T05:18:09Z", {
      role: "assistant",
      timestamp: Date.parse("2026-11-21T05:18:09Z"),
      content: [
        {
          type: "text",
          text:
            "下面是一段典型的嵌套清单与配置示例：\n\n" +
            "1. **圆角体系**保持同心递减：\n" +
            "   - 输入区与主气泡使用 `20px–24px` 大圆角；\n" +
            "   - 内部按钮与选项使用 `10px–12px` 中小圆角；\n" +
            "2. **留白节奏**优先松弛：\n" +
            "   - 段落行高控制在 `1.7` 左右；\n" +
            "   - 卡片与代码块减少强框线，改用极淡灰底衬托。\n\n" +
            "```json\n" +
            "{\n" +
            "  \"surface\": \"web\",\n" +
            "  \"theme\": \"soft-light\",\n" +
            "  \"radius\": { \"composer\": 24, \"bubble\": 20, \"card\": 14, \"control\": 10 }\n" +
            "}\n" +
            "```",
        },
      ],
      stopReason: "stop",
    }),
  ];

  sessions.set("mock-markdown-rich", {
    info: {
      id: "mock-markdown-rich",
      path: `${p1}/.pi/sessions/mock-markdown-rich.jsonl`,
      cwd: p1,
      projectRoot: p1,
      name: "排版压测：宽表、代码块、公式与压缩",
      created: "2026-11-21T05:10:00Z",
      modified: "2026-11-21T05:18:09Z",
      messageCount: 5,
      firstMessage: "这是我画的布局草图，帮我整理一份桌面 / Web / 移动三端的布局对照表",
    },
    generation: "gen-md-1",
    model: { provider: "openai", modelId: "gpt-5.4", name: "GPT-5.4" },
    thinkingLevel: "high",
    contextUsage: { percent: 64, contextWindow: 200000, tokens: 128450 },
    turns: [
      { id: "md-e1", entries: mdTurn1, resultEntryId: "md-e2", stopReason: "stop" },
      { id: "md-c1", entries: mdCompactionTurn, stopReason: "stop" },
      { id: "md-e3", entries: mdTurn2, resultEntryId: "md-e4", stopReason: "stop" },
    ],
  });

  // 3. 实时流式运行中 + 思考中 + 运行中工具 + 排队消息
  const liveTurn1: MockEntry[] = [
    makeEntry("live-e1", null, "2026-11-21T07:02:00Z", {
      role: "user",
      content: "帮我重构 SSE 断线重连逻辑，加上带抖动的指数退避。",
      timestamp: Date.parse("2026-11-21T07:02:00Z"),
    }),
    makeEntry("live-e2", "live-e1", "2026-11-21T07:02:14Z", {
      role: "assistant",
      timestamp: Date.parse("2026-11-21T07:02:14Z"),
      content: [
        {
          type: "text",
          text: "好的，我已经先读完了 `surface/ui/src/http-client.ts` 中的事件订阅循环。接下来我会把退避计时器拆成纯函数，并补上断网恢复时的重置逻辑。",
        },
      ],
      stopReason: "stop",
    }),
  ];

  const liveTurn2: MockEntry[] = [
    makeEntry("live-e3", "live-e2", "2026-11-21T07:09:40Z", {
      role: "user",
      content: "直接改吧，顺便跑一下前端单测确认边界行为。",
      timestamp: Date.parse("2026-11-21T07:09:40Z"),
    }),
  ];

  sessions.set("mock-live-streaming", {
    info: {
      id: "mock-live-streaming",
      path: `${p1}/.pi/sessions/mock-live-streaming.jsonl`,
      cwd: p1,
      projectRoot: p1,
      name: "实时重构 SSE 断线重连（进行中）",
      created: "2026-11-21T07:02:00Z",
      modified: "2026-11-21T07:09:55Z",
      messageCount: 3,
      firstMessage: "帮我重构 SSE 断线重连逻辑，加上带抖动的指数退避。",
    },
    generation: "gen-live-1",
    model: { provider: "anthropic", modelId: "claude-opus-4-6", name: "Claude Opus 4.6" },
    thinkingLevel: "high",
    contextUsage: { percent: 41, contextWindow: 200000, tokens: 82100 },
    turns: [
      { id: "live-e1", entries: liveTurn1, resultEntryId: "live-e2", stopReason: "stop" },
      { id: "live-e3", entries: liveTurn2 },
    ],
    live: {
      active: true,
      state: {
        sessionId: "mock-live-streaming",
        cwd: p1,
        thinkingLevel: "high",
        phase: "provider",
        isStreaming: true,
        isPromptRunning: true,
        isBashRunning: true,
        isCompacting: false,
        queuedMessages: {
          steering: ["退避上限控制在 30 秒以内，避免休眠唤醒后等待太久"],
          followUp: ["改完顺手补一个断线重连的单元测试"],
        },
        contextUsage: { percent: 41, contextWindow: 200000, tokens: 82100 },
        model: { id: "claude-opus-4-6", name: "Claude Opus 4.6", provider: "anthropic" },
      },
      streaming: {
        id: "live-stream-1",
        message: {
          role: "assistant",
          timestamp: Date.parse("2026-11-21T07:09:52Z"),
          content: [
            {
              type: "thinking",
              thinking: "正在核对 backoff 上限与浏览器 online 事件唤醒时的计时器清理顺序...",
            },
            {
              type: "toolCall",
              id: "tc-live-bash",
              name: "bash",
              arguments: { command: "node --test surface/ui/test/refresh-scheduler.test.mjs" },
            },
            {
              type: "text",
              text: "已经把 `computeReconnectDelay` 抽取出来并接入 `EventSource` 重连分支，正在跑调度器单测验证边界条件……",
            },
          ],
        },
      },
      toolsRunning: [
        {
          toolCallId: "tc-live-bash",
          toolName: "bash",
          args: { command: "node --test surface/ui/test/refresh-scheduler.test.mjs" },
          partialResult: {
            content: [{ type: "text", text: "TAP version 13\n# Subtest: exponential backoff caps at 30s\n" }],
          },
        },
      ],
    },
  });

  // 4. 报错与手动中断恢复场景
  const errTurn1: MockEntry[] = [
    makeEntry("err-e1", null, "2026-11-21T04:10:00Z", {
      role: "user",
      content: "帮我扫描整个仓库里所有过期的依赖并生成升级方案。",
      timestamp: Date.parse("2026-11-21T04:10:00Z"),
    }),
    makeEntry("err-e2", "err-e1", "2026-11-21T04:10:08Z", {
      role: "assistant",
      timestamp: Date.parse("2026-11-21T04:10:08Z"),
      content: [],
      stopReason: "error",
      errorMessage: "Anthropic API returned HTTP 429 Too Many Requests: rate limit exceeded for claude-opus-4-6. Please retry after 12s.",
    }),
  ];

  const errTurn2: MockEntry[] = [
    makeEntry("err-e3", "err-e2", "2026-11-21T04:11:00Z", {
      role: "user",
      content: "没事，先只看 `surface/ui` 这一个包就行。",
      timestamp: Date.parse("2026-11-21T04:11:00Z"),
    }),
    makeEntry("err-e4", "err-e3", "2026-11-21T04:11:10Z", {
      role: "assistant",
      timestamp: Date.parse("2026-11-21T04:11:10Z"),
      content: [
        {
          type: "text",
          text: "`surface/ui/package.json` 的直接依赖非常克制，目前只有 `lucide-react`、`react-markdown`、`@xterm/xterm` 等核心渲染库，均处于最新稳定小版本，暂无需强制升级。",
        },
      ],
      stopReason: "stop",
    }),
  ];

  sessions.set("mock-error-aborted", {
    info: {
      id: "mock-error-aborted",
      path: `${p1}/.pi/sessions/mock-error-aborted.jsonl`,
      cwd: p1,
      projectRoot: p1,
      name: "接口限流报错与恢复",
      created: "2026-11-21T04:10:00Z",
      modified: "2026-11-21T04:11:10Z",
      messageCount: 4,
      firstMessage: "帮我扫描整个仓库里所有过期的依赖并生成升级方案。",
    },
    generation: "gen-err-1",
    model: { provider: "anthropic", modelId: "claude-opus-4-6", name: "Claude Opus 4.6" },
    thinkingLevel: "low",
    contextUsage: { percent: 12, contextWindow: 200000, tokens: 24100 },
    turns: [
      { id: "err-e1", entries: errTurn1, resultEntryId: "err-e2", stopReason: "error" },
      { id: "err-e3", entries: errTurn2, resultEntryId: "err-e4", stopReason: "stop" },
    ],
  });

  // 5 & 6. 超长项目名 & 超长会话名 & 未读状态
  const p2 = "/Users/dev/Documents/code/enterprise-platform-monorepo-service-gateway-v2";
  const longTurn1: MockEntry[] = [
    makeEntry("long-e1", null, "2026-11-21T06:15:00Z", {
      role: "user",
      content: "请核对一下跨区域灰度流量路由策略在权重切回 0% 时的连接排空行为。",
      timestamp: Date.parse("2026-11-21T06:15:00Z"),
    }),
    makeEntry("long-e2", "long-e1", "2026-11-21T06:15:18Z", {
      role: "assistant",
      timestamp: Date.parse("2026-11-21T06:15:18Z"),
      content: [
        {
          type: "text",
          text: "当权重切回 `0%` 时，新连接会立刻全部路由至主区域，既有长连接通过 `gracefulDrainTimeout = 30s` 温和排空，不会直接发送 `RST`。",
        },
      ],
      stopReason: "stop",
    }),
  ];

  sessions.set("mock-long-titles", {
    info: {
      id: "mock-long-titles",
      path: `${p2}/.pi/sessions/mock-long-titles.jsonl`,
      cwd: p2,
      projectRoot: p2,
      name: "超长标题压测：重构多租户网关鉴权中间件与跨区域灰度流量路由策略（含回滚方案）",
      created: "2026-11-21T06:15:00Z",
      modified: "2026-11-21T06:20:00Z",
      messageCount: 2,
      firstMessage: "请核对一下跨区域灰度流量路由策略在权重切回 0% 时的连接排空行为。",
    },
    generation: "gen-long-1",
    model: { provider: "openai", modelId: "gpt-5.4", name: "GPT-5.4" },
    thinkingLevel: "medium",
    contextUsage: { percent: 9, contextWindow: 200000, tokens: 18200 },
    turns: [{ id: "long-e1", entries: longTurn1, resultEntryId: "long-e2", stopReason: "stop" }],
  });

  const unreadTurn1: MockEntry[] = [
    makeEntry("unread-e1", null, "2026-11-21T03:00:00Z", {
      role: "user",
      content: "整理昨晚的巡检异常日志。",
      timestamp: Date.parse("2026-11-21T03:00:00Z"),
    }),
    makeEntry("unread-e2", "unread-e1", "2026-11-21T03:00:42Z", {
      role: "assistant",
      timestamp: Date.parse("2026-11-21T03:00:42Z"),
      content: [{ type: "text", text: "昨晚共有 2 次短暂上游超时（均在 1.2s 内自动重试恢复），无 5xx 告警。" }],
      stopReason: "stop",
    }),
  ];

  sessions.set("mock-unread-done", {
    info: {
      id: "mock-unread-done",
      path: `${p2}/.pi/sessions/mock-unread-done.jsonl`,
      cwd: p2,
      projectRoot: p2,
      name: "夜间巡检日志分析报告",
      created: "2026-11-21T03:00:00Z",
      modified: "2026-11-21T03:00:42Z",
      messageCount: 2,
      firstMessage: "整理昨晚的巡检异常日志。",
      lastAssistantMessageId: "unread-e2",
    },
    generation: "gen-unread-1",
    model: { provider: "anthropic", modelId: "claude-sonnet-4-6", name: "Claude Sonnet 4.6" },
    thinkingLevel: "off",
    contextUsage: { percent: 4, contextWindow: 200000, tokens: 8100 },
    turns: [{ id: "unread-e1", entries: unreadTurn1, resultEntryId: "unread-e2", stopReason: "stop" }],
  });

  // 7. 轻松日常对话
  const p3 = "/Users/dev/Documents/code/design-system-tokens";
  const qaTurn1: MockEntry[] = [
    makeEntry("qa-e1", null, "2026-11-20T18:25:00Z", {
      role: "user",
      content: "为什么很多工具看起来容易让人紧张，而像 ChatGPT 或 Claude 用久了感觉更放松？",
      timestamp: Date.parse("2026-11-20T18:25:00Z"),
    }),
    makeEntry("qa-e2", "qa-e1", "2026-11-20T18:25:12Z", {
      role: "assistant",
      timestamp: Date.parse("2026-11-20T18:25:12Z"),
      content: [
        {
          type: "text",
          text:
            "通常有三个很直接的原因：\n\n" +
            "1. **边界很轻**：界面主要靠大面积留白和极其柔和的底色差来分区，而不是到处画 1px 实线边框把视线切成小格子。\n" +
            "2. **圆角和间距从容**：输入区、气泡、列表项都有温润的圆角与宽裕的内边距，视觉上没有尖锐的棱角和压迫感。\n" +
            "3. **静默成功**：正常发生的事情悄悄退在背景里，不弹徽章、不刷数字；只有真正需要你注意的异常才会出现。",
        },
      ],
      stopReason: "stop",
    }),
  ];

  sessions.set("mock-simple-qa", {
    info: {
      id: "mock-simple-qa",
      path: `${p3}/.pi/sessions/mock-simple-qa.jsonl`,
      cwd: p3,
      projectRoot: p3,
      name: "探讨柔和留白与阅读节奏",
      created: "2026-11-20T18:25:00Z",
      modified: "2026-11-20T18:30:00Z",
      messageCount: 2,
      firstMessage: "为什么很多工具看起来容易让人紧张，而像 ChatGPT 或 Claude 用久了感觉更放松？",
    },
    generation: "gen-qa-1",
    model: { provider: "anthropic", modelId: "claude-opus-4-6", name: "Claude Opus 4.6" },
    thinkingLevel: "low",
    contextUsage: { percent: 5, contextWindow: 200000, tokens: 9500 },
    turns: [{ id: "qa-e1", entries: qaTurn1, resultEntryId: "qa-e2", stopReason: "stop" }],
  });

  return sessions;
}

const MOCK_FILES: Record<string, FileEntry[]> = {
  "/Users/dev/Documents/code/pi-package/pi-go": [
    { name: "surface", isDir: true, size: 0, modified: "2026-11-21T06:00:00Z" },
    { name: "internal", isDir: true, size: 0, modified: "2026-11-21T05:30:00Z" },
    { name: "docs", isDir: true, size: 0, modified: "2026-11-20T12:00:00Z" },
    { name: "AGENTS.md", isDir: false, size: 4120, modified: "2026-11-20T10:00:00Z" },
    { name: "Makefile", isDir: false, size: 2890, modified: "2026-11-19T14:00:00Z" },
    { name: "README.md", isDir: false, size: 5420, modified: "2026-11-18T09:00:00Z" },
  ],
  "/Users/dev/Documents/code/pi-package/pi-go/surface": [
    { name: "ui", isDir: true, size: 0, modified: "2026-11-21T06:00:00Z" },
    { name: "web", isDir: true, size: 0, modified: "2026-11-21T05:45:00Z" },
    { name: "gui", isDir: true, size: 0, modified: "2026-11-20T16:00:00Z" },
    { name: "mobile", isDir: true, size: 0, modified: "2026-11-19T11:00:00Z" },
  ],
  "/Users/dev/Documents/code/pi-package/pi-go/surface/ui": [
    { name: "src", isDir: true, size: 0, modified: "2026-11-21T06:00:00Z" },
    { name: "package.json", isDir: false, size: 912, modified: "2026-11-18T12:00:00Z" },
  ],
  "/Users/dev/Documents/code/pi-package/pi-go/surface/ui/src": [
    { name: "styles.css", isDir: false, size: 144844, modified: "2026-11-21T06:00:00Z" },
    { name: "contracts.ts", isDir: false, size: 14561, modified: "2026-11-19T19:12:00Z" },
    { name: "very-long-component-filename-for-overflow-verification.tsx", isDir: false, size: 3210, modified: "2026-11-21T06:00:00Z" },
  ],
  "/Users/dev/Documents/code/pi-package/pi-go/internal": [
    { name: "session", isDir: true, size: 0, modified: "2026-11-21T06:40:00Z" },
    { name: "application", isDir: true, size: 0, modified: "2026-11-21T06:45:00Z" },
  ],
  "/Users/dev/Documents/code/pi-package/pi-go/internal/session": [
    { name: "manager.go", isDir: false, size: 8420, modified: "2026-11-21T06:40:00Z" },
    { name: "projection.go", isDir: false, size: 4210, modified: "2026-11-21T06:40:00Z" },
  ],
  "/Users/dev/Documents/code/pi-package/pi-go/internal/application": [
    { name: "history.go", isDir: false, size: 6150, modified: "2026-11-21T06:45:00Z" },
  ],
  "/Users/dev/Documents/code/pi-package/pi-go/docs": [
    { name: "architecture.md", isDir: false, size: 6840, modified: "2026-11-17T08:00:00Z" },
  ],
};

const MOCK_PREVIEW_CONTENT: Record<string, { language: string; content: string }> = {
  "README.md": {
    language: "markdown",
    content:
      "# pi-go\n\nGo 编写的编程 Agent 运行时，桌面、Web 与移动端共用一套工作台体验。\n\n## 核心原则\n\n- 圆润、宽松、低视觉噪音\n- 工具过程默认折叠，异常清晰可见\n- 核心状态由 Go 统一持有\n",
  },
  "styles.css": {
    language: "css",
    content:
      ":root {\n  --pi-bg: #ffffff;\n  --pi-panel: #f8f8f7;\n  --pi-radius-composer: 24px;\n  --pi-radius-bubble: 20px;\n}\n",
  },
  "Makefile": {
    language: "makefile",
    content:
      "SURFACE ?= web\n\n" +
      "# 超长单行压测：验证文件源码预览自动换行而不出现横向滚动条\n" +
      "check:\n" +
      "\t@./scripts/surface.sh check --surface=$(SURFACE) --verify-types --verify-lint --verify-go-tests --race --timeout=120s --strict-contract-boundary\n",
  },
  "projection.go": {
    language: "go",
    content:
      "package session\n\n// ProjectWithAnchor keeps tool results linked across compaction boundaries even when multiple nested runs span an anchor checkpoint.\nfunc ProjectWithAnchor(anchor Entry, window []Entry) []Entry {\n\treturn compactOrphans(append([]Entry{anchor}, window...))\n}\n",
  },
};

const MOCK_MODELS_VIEW: ModelsView = {
  models: {
    "anthropic/claude-opus-4-6": "Claude Opus 4.6",
    "anthropic/claude-sonnet-4-6": "Claude Sonnet 4.6",
    "openai/gpt-5.4": "GPT-5.4",
    "google/gemini-2.5-pro": "Gemini 2.5 Pro",
  },
  modelList: [
    { id: "claude-opus-4-6", name: "Claude Opus 4.6", provider: "anthropic", providerName: "Anthropic", defaultThinkingLevel: "medium" },
    { id: "claude-sonnet-4-6", name: "Claude Sonnet 4.6", provider: "anthropic", providerName: "Anthropic", defaultThinkingLevel: "low" },
    { id: "gpt-5.4", name: "GPT-5.4", provider: "openai", providerName: "OpenAI", defaultThinkingLevel: "high" },
    { id: "gemini-2.5-pro", name: "Gemini 2.5 Pro", provider: "google", providerName: "Google", defaultThinkingLevel: "medium" },
  ],
  defaultModel: { provider: "anthropic", modelId: "claude-opus-4-6" },
  thinkingLevels: {
    "anthropic/claude-opus-4-6": ["off", "low", "medium", "high"],
    "anthropic/claude-sonnet-4-6": ["off", "low", "medium"],
    "openai/gpt-5.4": ["low", "medium", "high"],
    "google/gemini-2.5-pro": ["off", "low", "medium", "high"],
  },
  thinkingLevelMaps: {},
  thinkingLevelPins: {},
};

const MOCK_PROVIDERS: ModelProviderInfo[] = [
  { id: "anthropic", name: "Anthropic", configured: true, source: "api_key", credentialType: "api_key", modelCount: 4, supportsApiKey: true, supportsOAuth: true, builtin: true, enabled: true },
  { id: "openai", name: "OpenAI", configured: true, source: "api_key", credentialType: "api_key", modelCount: 6, supportsApiKey: true, supportsOAuth: false, builtin: true, enabled: true },
  { id: "google", name: "Google Gemini", configured: true, source: "api_key", credentialType: "api_key", modelCount: 3, supportsApiKey: true, supportsOAuth: true, builtin: true, enabled: true },
  { id: "openrouter", name: "OpenRouter", configured: false, modelCount: 42, supportsApiKey: true, supportsOAuth: false, builtin: true, enabled: false },
];

const MOCK_SLASH_COMMANDS: SlashCommandInfo[] = [
  { name: "compact", description: "压缩当前会话上下文，保留关键结论", argumentHint: "[保留重点]", source: "extension" },
  { name: "fork", description: "从当前或历史消息创建新分支会话", source: "extension" },
  { name: "model", description: "切换当前会话使用的模型", argumentHint: "<provider/model>", source: "extension" },
  { name: "thinking", description: "调整思考强度（off / low / medium / high）", argumentHint: "<level>", source: "extension" },
  { name: "review", description: "按项目规范审视当前未提交改动", source: "skill" },
];

function buildEntryNode(entry: MockEntry, position: number): EntryNode {
  return {
    id: entry.id,
    parentId: entry.parentId,
    type: entry.type,
    role: entry.role,
    timestamp: entry.timestamp,
    preview: entry.preview,
    hasChildren: false,
    rawBytes: JSON.stringify(entry.message).length,
    contentToken: `tok-${entry.id}`,
    position,
  };
}

function buildSessionIndex(session: MockSessionData) {
  const flat: MockEntry[] = [];
  const runs: HistoryRun[] = [];
  const itemsByEntryId = new Map<string, HistoryItem>();
  const runEntries = new Map<string, HistoryItem[]>();

  for (const turn of session.turns) {
    const start = flat.length;
    const turnItems: HistoryItem[] = [];
    for (const entry of turn.entries) {
      const position = flat.length;
      flat.push(entry);
      const item: HistoryItem = {
        node: buildEntryNode(entry, position),
        message: entry.message,
      };
      itemsByEntryId.set(entry.id, item);
      turnItems.push(item);
    }
    const end = flat.length;
    const header = turnItems[0].node;
    const resultItem = turn.resultEntryId ? itemsByEntryId.get(turn.resultEntryId) : undefined;
    const lastItem = turnItems[turnItems.length - 1];
    const processCount = Math.max(0, turnItems.length - (resultItem ? 2 : 1));
    runs.push({
      id: header.id,
      header,
      result: resultItem?.node,
      start,
      end,
      lastEntryId: lastItem.node.id,
      lastActivity: lastItem.node.timestamp,
      processCount,
      resultHasProcess: false,
      stopReason: turn.stopReason,
      anchors: [header],
    });
    runEntries.set(header.id, turnItems);
  }

  return { flat, runs, itemsByEntryId, runEntries };
}

export function createMockApplicationClient(): ApplicationClient {
  const sessions = buildMockSessions();
  const projects = PROJECTS.map((project) => ({ ...project }));
  const drafts = new Map<string, DraftText>();
  const observers = new Set<EventObserver>();
  let sequence = 1;
  let versionCounter = 1;

  const listSessionInfos = () =>
    Array.from(sessions.values())
      .map((item) => item.info)
      .sort((a, b) => Date.parse(b.modified) - Date.parse(a.modified));

  const runningSessionIds = () =>
    Array.from(sessions.values())
      .filter((item) => item.live?.active && item.live.state?.isStreaming)
      .map((item) => item.info.id);

  const buildProjectsPage = (): ProjectDirectoryPage => {
    const allSessions = listSessionInfos();
    return {
      version: `mock-dir-${versionCounter}`,
      start: 0,
      total: projects.length,
      projects: projects.map((project) => {
        const matched = allSessions.filter((session) => (session.projectRoot || session.cwd) === project.path);
        const page: SessionDirectoryPage = {
          version: `mock-dir-${versionCounter}`,
          sessions: matched,
          total: matched.length,
          start: 0,
        };
        return { project: { ...project, sessionCount: matched.length }, sessions: page };
      }),
    };
  };

  const client: ApplicationClient = {
    kind: "local",
    endpoint: typeof window !== "undefined" ? window.location.origin : "http://127.0.0.1:30141",

    async authStatus(): Promise<AuthState> {
      return { authRequired: false, authenticated: true };
    },

    async login(): Promise<void> {},

    async snapshot(): Promise<ApplicationSnapshot> {
      return {
        revision: versionCounter,
        cursor: `mock:${sequence}`,
        agentDir: "/Users/dev/.pi",
        defaultCwd: projects[0].path,
        projects,
        sessions: listSessionInfos(),
        runningSessionIds: runningSessionIds(),
      };
    },

    async readApplicationResource<K extends keyof ApplicationResources>(
      kind: K,
      query?: ApplicationReadRequest,
    ): Promise<ResourceResponse<ApplicationResources[K]>> {
      const allSessions = listSessionInfos();
      if (kind === "overview") {
        const overview: ApplicationOverview = {
          agentDir: "/Users/dev/.pi",
          defaultCwd: projects[0].path,
          projects,
          sessionCount: allSessions.length,
          runningSessionIds: runningSessionIds(),
          directory: buildProjectsPage(),
        };
        return {
          token: `overview-${versionCounter}`,
          cursor: `mock:${sequence}`,
          data: overview as ApplicationResources[K],
        };
      }
      if (kind === "projects") {
        const page = buildProjectsPage();
        return { token: `projects-${versionCounter}`, cursor: `mock:${sequence}`, data: page as ApplicationResources[K] };
      }
      const projectFilter = query?.project ?? "";
      const searchFilter = (query?.search ?? "").trim().toLowerCase();
      const filtered = allSessions.filter((session) => {
        if (projectFilter && (session.projectRoot || session.cwd) !== projectFilter) return false;
        if (searchFilter) {
          const hay = `${session.name ?? ""} ${session.firstMessage} ${session.id}`.toLowerCase();
          return hay.includes(searchFilter);
        }
        return true;
      });
      const directory: SessionDirectoryPage = {
        version: `mock-dir-${versionCounter}`,
        sessions: filtered,
        total: filtered.length,
        start: 0,
      };
      return { token: `dir-${versionCounter}-${projectFilter}-${searchFilter}`, cursor: `mock:${sequence}`, data: directory as ApplicationResources[K] };
    },

    async sessionView(sessionId: string): Promise<SessionView> {
      const session = sessions.get(sessionId);
      if (!session) throw new Error(`Mock session not found: ${sessionId}`);
      const indexed = buildSessionIndex(session);
      return {
        revision: versionCounter,
        sessionId,
        filePath: session.info.path,
        info: session.info,
        leafId: indexed.flat.at(-1)?.id ?? null,
        tree: [],
        context: {
          messages: indexed.flat.map((entry) => entry.message),
          entryIds: indexed.flat.map((entry) => entry.id),
          thinkingLevel: session.thinkingLevel,
          model: { provider: session.model.provider, modelId: session.model.modelId },
        },
        running: Boolean(session.live?.active),
        state: session.live?.state,
      };
    },

    async readResource<K extends keyof SessionResources>(
      sessionId: string,
      kind: K,
      query?: ReadResourceRequest,
    ): Promise<ResourceResponse<SessionResources[K]>> {
      const session = sessions.get(sessionId);
      if (!session) throw Object.assign(new Error(`session not found: ${sessionId}`), { status: 404 });
      const indexed = buildSessionIndex(session);
      const log = indexed.flat.length;
      const version = { generation: session.generation, log, view: log };
      const leafId = indexed.flat.at(-1)?.id ?? null;

      if (kind === "metadata") {
        const data: SessionMetadata = {
          sessionId,
          version,
          info: session.info,
          filePath: session.info.path,
          leafId,
          latestUsage: {
            entryId: leafId ?? "e0",
            timestamp: Date.parse(session.info.modified),
            provider: session.model.provider,
            api: "anthropic-messages",
            model: session.model.modelId,
            usage: {
              input: session.contextUsage?.tokens ?? 12000,
              output: 1840,
              cacheRead: 32000,
              cacheWrite: 1200,
              totalTokens: (session.contextUsage?.tokens ?? 12000) + 1840,
              cost: { input: 0.04, output: 0.03, cacheRead: 0.01, cacheWrite: 0.01, total: 0.09 },
            },
          },
        };
        return { token: `meta-${sessionId}-${versionCounter}`, data: data as SessionResources[K] };
      }

      if (kind === "live") {
        const data: LiveSnapshot = {
          sessionId,
          active: session.live?.active ?? true,
          version: { generation: session.generation, state: versionCounter, configuration: versionCounter, queue: versionCounter },
          history: { ...version, leafId },
          state: {
            sessionId,
            cwd: session.info.cwd,
            thinkingLevel: session.thinkingLevel,
            phase: "idle",
            isStreaming: false,
            isPromptRunning: false,
            isBashRunning: false,
            isCompacting: false,
            queuedMessages: { steering: [], followUp: [] },
            contextUsage: session.contextUsage ?? { percent: 18, contextWindow: 200000, tokens: 36000 },
            model: { id: session.model.modelId, name: session.model.name, provider: session.model.provider },
            ...session.live?.state,
          },
          streaming: session.live?.streaming ?? null,
          pending: session.live?.pending ?? [],
          toolsRunning: session.live?.toolsRunning ?? [],
          errorMessage: session.live?.errorMessage,
        };
        return { token: `live-${sessionId}-${versionCounter}`, data: data as SessionResources[K] };
      }

      if (kind === "runs") {
        const items: HistoryItem[] = [];
        for (const run of indexed.runs) {
          const headerItem = indexed.itemsByEntryId.get(run.header.id);
          if (headerItem) items.push(headerItem);
          if (run.result) {
            const resultItem = indexed.itemsByEntryId.get(run.result.id);
            if (resultItem) items.push({ ...resultItem, projection: "answer" });
          }
        }
        const data: RunsPage = {
          sessionId,
          version,
          anchorId: leafId ?? "",
          items,
          runs: indexed.runs,
          start: 0,
          end: indexed.runs.length,
          total: indexed.runs.length,
          connected: true,
        };
        return { token: `runs-${sessionId}-${versionCounter}`, data: data as SessionResources[K] };
      }

      if (kind === "history") {
        const scopedRun = query?.runId ? indexed.runs.find((run) => run.id === query.runId) : undefined;
        const items = scopedRun
          ? (indexed.runEntries.get(scopedRun.id) ?? [])
          : indexed.flat.map((entry) => indexed.itemsByEntryId.get(entry.id)!);
        const data: HistoryPage = {
          sessionId,
          version,
          anchorId: leafId ?? "",
          items,
          start: 0,
          end: items.length,
          total: items.length,
          connected: true,
          run: scopedRun,
        };
        return { token: `hist-${sessionId}-${query?.runId ?? "all"}-${versionCounter}`, data: data as SessionResources[K] };
      }

      if (kind === "nodes") {
        const matching = indexed.flat
          .map((entry, idx) => buildEntryNode(entry, idx))
          .filter((node) => (!query?.role || node.role === query.role) && (!query?.entryType || node.type === query.entryType));
        const data: NodesPage = {
          connected: true,
          sessionId,
          version,
          items: matching,
        };
        return { token: `nodes-${sessionId}-${versionCounter}`, data: data as SessionResources[K] };
      }

      if (kind === "entry") {
        const target = indexed.flat.find((entry) => entry.id === query?.entryId);
        return {
          token: `entry-${query?.entryId ?? ""}`,
          data: {
            sessionId,
            generation: session.generation,
            entryId: query?.entryId ?? "",
            contentToken: `tok-${query?.entryId ?? ""}`,
            message: target?.message ?? null,
          } as SessionResources[K],
        };
      }

      return {
        token: `img-${sessionId}`,
        data: {
          sessionId,
          generation: session.generation,
          entryId: query?.entryId ?? "",
          mimeType: "image/svg+xml",
          data: SAMPLE_SVG_DATA_URI.replace("data:image/svg+xml;base64,", ""),
        } as SessionResources[K],
      };
    },

    async createSession(input: CreateSessionRequest): Promise<CreateSessionResult> {
      const id = `mock-new-${Date.now()}`;
      const now = new Date().toISOString();
      const model = MOCK_MODELS_VIEW.modelList.find((m) => m.id === input.modelId) ?? MOCK_MODELS_VIEW.modelList[0];
      sessions.set(id, {
        info: {
          id,
          path: `${input.cwd}/.pi/sessions/${id}.jsonl`,
          cwd: input.cwd,
          projectRoot: input.cwd,
          name: "新建模拟会话",
          created: now,
          modified: now,
          messageCount: 0,
          firstMessage: "",
        },
        generation: `gen-${id}`,
        model: { provider: model.provider, modelId: model.id, name: model.name },
        thinkingLevel: input.thinkingLevel ?? "medium",
        turns: [],
      });
      versionCounter += 1;
      return {
        sessionId: id,
        revision: versionCounter,
        model: { provider: model.provider, modelId: model.id },
        thinkingLevel: input.thinkingLevel ?? "medium",
      };
    },

    async models(): Promise<ModelsView> {
      return MOCK_MODELS_VIEW;
    },

    async syncBuiltinModels(): Promise<ModelSyncResult> {
      return { published: true, summary: "Mock 模型目录已同步（共 4 个常用模型）" };
    },

    async modelProviders(): Promise<ModelProviderInfo[]> {
      return MOCK_PROVIDERS;
    },

    async providerModels(_cwd: string, providerId: string): Promise<ProviderModelInfo[]> {
      return MOCK_MODELS_VIEW.modelList
        .filter((item) => item.provider === providerId)
        .map((item) => ({
          id: item.id,
          name: item.name,
          api: providerId === "anthropic" ? "anthropic-messages" : "openai-responses",
          contextWindow: 200000,
          maxTokens: 16384,
          reasoning: true,
          imageInput: true,
        }));
    },

    async setBuiltinProviderEnabled(): Promise<void> {},
    async readModelsConfig(): Promise<ModelsConfig> {
      return { providers: {} };
    },
    async writeModelProvider(): Promise<void> {},
    async setProviderAPIKey(): Promise<void> {},
    async deleteProviderCredential(): Promise<void> {},
    async lookupModelPresets() {
      return [];
    },
    async discoverModels(): Promise<ModelDiscovery> {
      return { models: [], endpoint: "mock://discover" };
    },

    async readDraftText(key: string): Promise<DraftText> {
      return drafts.get(key) ?? { revision: "0", text: "" };
    },

    async writeDraftText(key: string, update: DraftTextUpdate): Promise<DraftTextWriteResult> {
      const next: DraftText = { revision: update.revision, text: update.text, updatedAt: new Date().toISOString() };
      drafts.set(key, next);
      return { saved: true, draft: next };
    },

    async browseDirectories(path?: string): Promise<DirectoryView> {
      const current = path || "/Users/dev/Documents/code";
      return {
        path: current,
        parentPath: current === "/Users/dev" ? null : "/Users/dev/Documents",
        directories: [
          { name: "pi-go", path: "/Users/dev/Documents/code/pi-package/pi-go" },
          { name: "enterprise-platform-monorepo-service-gateway-v2", path: "/Users/dev/Documents/code/enterprise-platform-monorepo-service-gateway-v2" },
          { name: "design-system-tokens", path: "/Users/dev/Documents/code/design-system-tokens" },
          { name: "empty-sandbox-workspace", path: "/Users/dev/Documents/code/empty-sandbox-workspace" },
        ],
      };
    },

    async listFiles(path: string): Promise<FileList> {
      const clean = path.replace(/\/+$/, "");
      return {
        path: clean,
        entries: MOCK_FILES[clean] ?? [
          { name: "README.md", isDir: false, size: 1240, modified: "2026-11-20T12:00:00Z" },
          { name: "package.json", isDir: false, size: 820, modified: "2026-11-20T12:00:00Z" },
        ],
      };
    },

    async previewFile(path: string): Promise<FilePreview> {
      const name = path.split("/").pop() || "file.txt";
      const match = MOCK_PREVIEW_CONTENT[name] ?? {
        language: name.endsWith(".ts") || name.endsWith(".tsx") ? "typescript" : "plaintext",
        content: `// Mock preview for ${path}\nexport const mockPreview = true;\n`,
      };
      return {
        path,
        name,
        kind: "text",
        mimeType: "text/plain",
        language: match.language,
        size: match.content.length,
        content: match.content,
      };
    },

    async downloadFile(): Promise<void> {},
    async deleteFile(): Promise<void> {},
    async inspectUploadTargets(): Promise<UploadTargetInspection> {
      return { conflicts: [], nonReplaceable: [] };
    },
    async uploadFiles(_directory: string, files: File[]): Promise<UploadResult> {
      return { uploaded: files.map((file) => file.name), skipped: [], errors: [] };
    },

    async addProject(path: string): Promise<ProjectInfo> {
      const added: ProjectInfo = { path, modified: new Date().toISOString(), sessionCount: 0 };
      projects.push(added);
      versionCounter += 1;
      return added;
    },
    async removeProject(path: string): Promise<void> {
      const idx = projects.findIndex((item) => item.path === path);
      if (idx >= 0) projects.splice(idx, 1);
      versionCounter += 1;
    },
    async renameSession(sessionId: string, name: string): Promise<void> {
      const session = sessions.get(sessionId);
      if (session) session.info.name = name;
      versionCounter += 1;
    },
    async deleteSession(sessionId: string): Promise<void> {
      sessions.delete(sessionId);
      versionCounter += 1;
    },

    async dispatch<T = unknown>(sessionId: string, command: Record<string, unknown>): Promise<T> {
      const type = String(command.type ?? "");
      if (type === "get_commands") {
        return { commands: MOCK_SLASH_COMMANDS } as T;
      }
      if (type === "get_session_stats") {
        const session = sessions.get(sessionId);
        const stats: SessionStatsInfo = {
          sessionId,
          sessionName: session?.info.name,
          sessionFile: session?.info.path,
          userMessages: 4,
          assistantMessages: 7,
          toolCalls: 8,
          toolResults: 8,
          totalMessages: 19,
          tokens: { input: 54200, output: 6820, cacheRead: 112400, cacheWrite: 3800, total: 177220 },
          cost: 0.42,
          contextUsage: session?.contextUsage ?? { percent: 28, contextWindow: 200000, tokens: 56420 },
        };
        return stats as T;
      }
      if (type === "set_model") {
        const session = sessions.get(sessionId);
        if (session && typeof command.provider === "string" && typeof command.modelId === "string") {
          const found = MOCK_MODELS_VIEW.modelList.find((m) => m.provider === command.provider && m.id === command.modelId);
          session.model = { provider: command.provider, modelId: command.modelId, name: found?.name ?? command.modelId };
          versionCounter += 1;
        }
        return {} as T;
      }
      if (type === "set_thinking_level") {
        const session = sessions.get(sessionId);
        if (session && typeof command.level === "string") {
          session.thinkingLevel = command.level;
          versionCounter += 1;
        }
        return {} as T;
      }
      if (type === "prompt" && typeof command.message === "string") {
        const session = sessions.get(sessionId);
        if (session) {
          const now = new Date().toISOString();
          const prevId = buildSessionIndex(session).flat.at(-1)?.id ?? null;
          const uId = `u-${Date.now()}`;
          const aId = `a-${Date.now() + 1}`;
          session.turns.push({
            id: uId,
            resultEntryId: aId,
            stopReason: "stop",
            entries: [
              makeEntry(uId, prevId, now, { role: "user", content: command.message, timestamp: Date.now() }),
              makeEntry(aId, uId, now, {
                role: "assistant",
                content: [{ type: "text", text: `（Mock 预览回复）已收到你的消息：「${command.message}」。你可以在左侧切换其他预置场景查看复杂过程折叠、宽表、流式运行中或报错状态。` }],
                timestamp: Date.now(),
                stopReason: "stop",
              }),
            ],
          });
          session.info.modified = now;
          if (!session.info.firstMessage) session.info.firstMessage = command.message;
          versionCounter += 1;
          sequence += 1;
          for (const observer of observers) {
            observer.onEvent({
              sequence,
              cursor: `mock:${sequence}`,
              sessionId,
              event: { type: "session_updated" },
            });
          }
        }
        return {} as T;
      }
      if (type === "clear_queue") {
        return { steering: [], followUp: [] } as T;
      }
      return {} as T;
    },

    subscribe(_after: number | string, observer: EventObserver): EventSubscription {
      observers.add(observer);
      queueMicrotask(() => observer.onConnected?.(`mock:${sequence}`));
      return {
        ready: Promise.resolve(),
        close() {
          observers.delete(observer);
        },
      };
    },

    close(): void {
      observers.clear();
    },
  };

  return client;
}
