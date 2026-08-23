# riflo 浏览器扩展（Phase 3）

这是一个无构建依赖的 Manifest V3 浏览器扩展，使用同一套 manifest 和源码支持 Chrome 与 Firefox。它只负责观察当前浏览器标签页的网络响应，识别 HLS 播放列表（`.m3u8` 或 HLS Content-Type），再由用户明确点击按钮把候选地址交给本机 riflo Web UI。

当前最低版本为 Chrome 121 和 Firefox 121。manifest 同时声明 `background.service_worker` 与 `background.scripts`：Chrome 使用 service worker，Firefox 使用后台脚本；扩展源码通过 `globalThis.browser ?? globalThis.chrome` 使用两端 API。

## Chrome 加载

1. 先启动 riflo：`./dist/riflo serve`。
2. 在 Chrome 打开 `chrome://extensions`。
3. 打开右上角「开发者模式」。
4. 点击「加载已解压的扩展程序」，选择本目录 `extension/`。
5. 固定 riflo 扩展图标，打开一个视频页面并开始播放。
6. 点击扩展图标，在候选列表中选择「在 riflo 打开」。

## Firefox 临时加载

临时加载不会安装到所有配置文件，也不需要打包或签名；Firefox 重启后需要再次加载。

1. 先启动 riflo：`./dist/riflo serve`。
2. 在 Firefox 打开 `about:debugging#/runtime/this-firefox`。
3. 点击「临时载入附加组件…」/“Load Temporary Add-on…”。
4. 选择本目录 `extension/manifest.json`（也可以选择目录中的任意扩展文件）。
5. 打开一个视频页面并开始播放，再点击工具栏中的扩展图标。
6. 在候选列表中选择「在 riflo 打开」。

临时扩展只在当前 Firefox 会话中有效；正式安装需要通过 Firefox Add-ons 签名/发布流程，并补充固定的 Gecko 扩展 ID。

打开按钮会在新标签页打开：

```text
http://127.0.0.1:8787/#source=<编码后的完整播放列表地址>&referer=<编码后的页面>&origin=<编码后的来源>&user_agent=<编码后的浏览器标识>&from=extension
```

扩展不直接调用 riflo API，也不会自动开始下载。Web UI 仍由用户确认任务参数后提交。

候选请求会同时记录一个脱敏后可显示的来源。提取顺序是 `documentUrl`、`initiator`、`origin`（Firefox 的 `originUrl` 也会作为 `origin` 处理）；只接受 `http`/`https`，并移除来源中的 URL 用户信息和 fragment。来源的完整 URL 只在 `storage.session` 中保留，弹窗只显示去掉 query、fragment 和用户信息后的地址。点击「在 riflo 打开」时，优先复用浏览器实际发送的 Referer；没有捕获到时再使用精确的 `documentUrl`。`initiator` 和 `origin` 只是弱上下文，有当前顶层标签页 URL 时优先使用当前标签页，没有时才使用弱上下文。

扩展还会在浏览器允许读取时捕获明确 `.m3u8` HLS 请求中的 `Referer`、`Origin` 和 `User-Agent`，只保留这三个安全头并放入 `storage.session`。`Cookie`、`Authorization` 以及其他请求头使用正向白名单过滤，绝不会保存或传给 riflo。`Origin` 和 `User-Agent` 会在用户点击打开时通过 hash 传给 Web UI；它们不会在弹窗中显示。响应阶段识别到无 `.m3u8` 后缀的 HLS Content-Type 时仍会记录播放列表，但不会猜测其对应的请求头。

## 权限说明

- `webRequest`：只读监听请求、请求头和响应头，发现播放列表地址并捕获三个安全请求头；请求阶段只接受明确的 `.m3u8` URL，响应阶段还会识别 HLS Content-Type。
- `storage`：使用浏览器 API 的 `storage.session` 保存本次浏览器会话中的候选，关闭浏览器后不保留。
- `activeTab`：在用户打开扩展弹窗后读取当前标签页的标题和 URL，用于显示来源及 Referer。
- `<all_urls>`：媒体播放列表通常来自独立 CDN，需要观察不同站点和 CDN 的 HTTP(S) 响应。

扩展没有请求 `cookies`、`nativeMessaging`、`downloads`、`webRequestBlocking` 或 `scripting` 权限。它不读取 Cookie、不注入页面脚本，也不直接下载文件。

## 数据与限制

- 每个标签页最多保留 20 个候选，候选 30 分钟后过期；同一完整 URL 会更新最近发现时间而不会重复显示。
- 弹窗会显示发现来源（URL 后缀、Content-Type），同一 URL 的多次发现会合并来源；也可以单独删除某一条候选或清空当前标签页。
- 弹窗展示播放列表 URL 和请求来源都会移除 query、fragment 和 URL 用户信息；「在 riflo 打开」仅在用户点击后使用 storage.session 中保存的完整播放列表 URL 和安全请求上下文。
- 只识别 HTTP(S) HLS 播放列表候选；`.ts`、`.m4s`、`.aac` 等媒体分片会明确排除。扩展无法仅凭响应地址判断 VOD/Live，riflo 当前可靠下载范围仍以 HLS VOD 为主。
- 不能保证发现所有播放器（例如 Service Worker 内部转发、加密或 DRM 流），也不绕过登录、付费墙、DRM 或其他访问控制。
- 扩展只把经过白名单过滤的 Referer、Origin、User-Agent 参数传给 riflo，不携带 Cookie 或 Authorization。riflo 服务必须先在本机回环地址运行。

## 测试

逻辑模块和 manifest 检查可以直接使用 Node 24 内置测试运行器，无需安装依赖：

```bash
cd extension
node --test
```
