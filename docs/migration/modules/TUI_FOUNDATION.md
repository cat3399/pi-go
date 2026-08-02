# M-TUI：terminal foundation / v0.1 charter

状态：最近复审为 0 Blocker / 0 Major / 1 Minor；最后一项 padding 修复候选已完成，
`awaiting final rereview`。

## 职责与边界

本里程碑负责 raw terminal lifecycle、bounded stdin framing、VT/Kitty key
decoding、paste、cell width 与文本列操作、word navigation，以及 OSC/terminal
capability 的纯解析。它不负责 editor buffer、autocomplete、markdown、overlay、完整
renderer 或 interactive application assembly。

实现位于 `internal/tui`。`Framer` 只拥有未完成的输入字节，调用者明确拥有 timeout、EOF
和 cancellation；`Terminal` 只拥有 raw-mode 与写出的 terminal mode 的恢复。没有后台 input
goroutine，因此停止不能遗留 reader 或 terminal state。输入上限默认 1 MiB，只约束尚未完成
的 UTF-8/control 与必须聚合的 paste；普通输入由 `FeedTo` 分批交付，不因单个大 chunk 被拒绝。
超限清空当前不可信片段并返回错误。非法 UTF-8 可配置为 reject，或将每个 maximal invalid
byte run 替换为一个 U+FFFD；该策略同样覆盖 control payload。partial UTF-8/escape 在 `Flush`
（timeout/EOF）时有确定结果。上限允许恰好等于配置值；bracketed-paste 结束标记作为固定协议
开销独立识别，不与 paste content 的上限混算。

## 上游证据

- `packages/tui/src/stdin-buffer.ts` 与 `test/stdin-buffer.test.ts`：chunk 边界、CSI、X10/SGR
  mouse、bracketed paste、Kitty printable de-dup 和 incomplete flush。
- `src/keys.ts` 与 `test/keys.test.ts`：legacy VT、modifyOtherKeys、CSI-u、modifier/event
  semantics。
- `src/utils.ts` 与 `test/wrap-ansi.test.ts`、`truncate-to-width.test.ts`、`tab-width.test.ts`、
  `regression-regional-indicator-width.test.ts`：ANSI-invisible cells、CJK/combining/emoji/tab、
  wrapping/truncation/slicing。
- `src/word-navigation.ts` 与 `test/word-navigation.test.ts`：whitespace/punctuation/CJK navigation。
- `src/terminal.ts`、`terminal-colors.ts` 与相关 tests：raw restore、paste/keyboard negotiation、
  dimensions、OSC 11/color report parsing。

所有证据固定在 `a116523434806910336b9de3e38a41aa5860030b`。

## 当前决策与后续

Go 的 word offsets 使用 UTF-8 byte offsets（不是 JS UTF-16 offsets）；cell API 从不切开
grapheme。Wrap 在 whitespace 优先断行并跨 continuation 重放 SGR/OSC-8 state；renderer 的
incremental diff 仍不属于本模块。Mouse wheel 以 `Scroll=+1/-1`、`Button=-1` 明确表达，未知
扩展按钮严格拒绝。Slice 在起始列重放有效 SGR/OSC-8 状态并在右边界关闭；ZWJ 不跨
ESC/control 边界吞并 terminal sequence。真实 Windows console runtime 与 pseudo-terminal smoke 留给平台/interactive
assembly，不以 host-specific test 假装覆盖。
