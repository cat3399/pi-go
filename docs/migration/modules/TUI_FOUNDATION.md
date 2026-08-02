# M-TUI：terminal foundation / v0.1 charter

状态：实现候选已完成，`awaiting independent review`。

## 职责与边界

本里程碑负责 raw terminal lifecycle、bounded stdin framing、VT/Kitty key
decoding、paste、cell width 与文本列操作、word navigation，以及 OSC/terminal
capability 的纯解析。它不负责 editor buffer、autocomplete、markdown、overlay、完整
renderer 或 interactive application assembly。

实现位于 `internal/tui`。`Framer` 只拥有未完成的输入字节，调用者明确拥有 timeout、EOF
和 cancellation；`Terminal` 只拥有 raw-mode 与写出的 terminal mode 的恢复。没有后台 input
goroutine，因此停止不能遗留 reader 或 terminal state。输入上限默认 1 MiB；超限清空当前
不可信片段并返回错误。非法 UTF-8 可配置为 replacement 或 reject，partial UTF-8/escape 在
`Flush`（timeout/EOF）时有确定结果。

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
grapheme。当前 wrap 保证 cell/UTF-8 边界，不迁移下游 renderer 的完整 ANSI-style replay；当
renderer 模块需要具体 style state 时再扩展为单独 slice。真实 Windows console runtime 与
pseudo-terminal smoke 留给平台/interactive assembly，不以 host-specific test 假装覆盖。
