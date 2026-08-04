## CodexTools 1.2.7

### Changes

- Adds Stepwise integration so the injected Codex UI can generate next-step suggestions from the current conversation.
- Extends protocol proxy, tool, vision, image, and realtime voice routing so compatible providers can cover more upstream Codex request shapes.
- Adds official realtime voice availability checks and proxying with clearer fallback reasons when the active account or provider cannot support it.
- Hardens model metadata patching, GPT-5/GPT-6 compatibility data, and provider configuration handling for newer upstream desktop builds.
- Improves plugin marketplace compatibility with the bundled curated marketplace archive and safer marketplace unlock behavior.
- Adds local session, SQLite, session-index cleanup, Stepwise, protocol proxy, realtime, and upstream-compatibility regression coverage.

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
