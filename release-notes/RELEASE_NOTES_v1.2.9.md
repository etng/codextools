## CodexTools 1.2.9

### Changes

- Improves Remote Control routing in mixed relay mode, including local proxy startup, session provider normalization, targeted recovery, duplicate-request suppression, and native RPC passthrough.
- Adds Responses proxy compatibility for zstd-compressed HTTP request bodies and returns a clear HTTP upgrade response for unsupported Responses WebSocket requests.
- Hardens diagnostics with bounded log rotation, memory-safe tail reads, runtime diagnostics, quieter high-frequency tracing, and renderer-side failure deduplication.
- Improves bridge/helper fallback behavior, runtime settings refresh, model and plugin request patch retries, and conversation sorting performance.
- Fixes provider-sync metadata handling for large transcripts and limits rollout metadata reads to avoid excessive disk and memory usage.
- Improves Windows upgrades by stopping the running ChatGPT desktop process before replacing installed files.
- Adds regression coverage for bridge parity, mixed relay Remote Control, diagnostic limits, disk usage, settings refresh, and Windows restart behavior.

### macOS unsigned build notice

The macOS packages are unsigned community builds, including the pkg installers. If macOS blocks the first launch, run:

```bash
xattr -cr "/Applications/ChatGPT Codex 管理工具.app"
xattr -cr "/Applications/ChatGPT Codex.app"
xattr -cr "/Applications/Codex++ 管理工具.app"
xattr -cr "/Applications/Codex++.app"
```

### macOS 首次启动提醒

macOS 包是未签名的社区构建，pkg 安装包也一样。如果 macOS 阻止首次启动，请执行：

```bash
xattr -cr "/Applications/ChatGPT Codex 管理工具.app"
xattr -cr "/Applications/ChatGPT Codex.app"
xattr -cr "/Applications/Codex++ 管理工具.app"
xattr -cr "/Applications/Codex++.app"
```
