## CodexTools 1.2.8

### Changes

- Adds provider routing and desktop interaction features, including remote connections, real mouse control, browser use, and Dream Skin import flows.
- Extends relay, proxy, and sub2api handling so more upstream request types and billing/diagnostic paths stay available across provider modes.
- Improves remote control recovery, passthrough, helper behavior, and launcher/manager handling for newer desktop workflows.
- Hardens model catalog, provider doctor, provider sync, session repair, and vision handling around the latest upstream request shapes.
- Adds broad regression coverage for provider routing, remote control, session actions, and upstream compatibility.
- Refreshes the injected UI and supporting web assets for the new provider-routing and desktop-interaction surface area.

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
