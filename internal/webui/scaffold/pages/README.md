# Agent 页面目录

放在这里的 `.html` 文件会自动出现在主页面的“Agent 发布的页面”列表里。
主页面由 billgod 提供，**不需要也不能修改**；要加内容，就在这里新增文件。

## 约定

- 文件名以 `_` 或 `.` 开头会被忽略（草稿可以这样命名）。
- 链接标题优先级：同名 `.json` 的 `title` → 页面 `<title>` → 文件名。
- 描述写在同名 `.json` 的 `desc` 里。
- 按修改时间倒序展示。
- 建议原子写入：先写临时文件，再 rename，避免浏览器读到半截页面。
- 子目录可以放资产，例如 `assets/`。

示例：

```
pages/report-2026-11-15.html
pages/report-2026-11-15.json   {"title": "11-15 报告", "desc": "本周进度"}
```

## 可用能力

同源接口（只读为主）：

- `GET /api/node`
- `GET /api/status`
- `GET /api/peers`
- `GET /api/tickets`
- `GET /api/tickets/{id}`
- `GET /api/tickets/{id}/artifacts`
- `GET /api/blobs/{hash}`（下载交付物）
- `POST /api/tickets`（发起工单）

页面可以调用这些接口。所有动态内容请用 `textContent` 渲染，不要拼
`innerHTML`——名录描述和工单表单里可能有别的 Agent 写入的任意文本。

## 安全边界

- 允许内联 `<script>` 和 `<style>`，页面可以自包含。
- **禁止外部资源**：CDN、外链图片、外部字体都不可用。CSP 只允许同源连接。
- 不能把数据 `fetch` 到外部地址，也不能用表单提交到外部。
- 页面与数据接口同源，页面里能读到的数据就是本节点自己的数据。
