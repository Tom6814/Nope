# 控制台完善 S1：定时消息管理页 + 统计概览 设计

> 二期第三阶段第 1 个子项目。依赖前两个阶段已就绪的 `scheduled_messages` 状态字段（pending/sent/failed/cancelled、fail_reason、reschedule_count、sent_at）与统一日志语义。

**目标**：在控制台 bot 详情页新增「定时/主动消息」管理区块，让 promise/proactive 消息可见、可取消、可统计。

## 现状基线（已查证）

- 后端已有：`GET /api/bots/{id}/scheduled-messages`（`internal/api/character_handler.go:215`，`ListScheduledMessages(botID, 100)`，仅校验 bot 归属，router.go:169）。
- 前端已有：`api.listScheduledMessages(botId)`（`web/src/lib/api.ts:247`）；persona.tsx「行为设置」里有主动消息配置卡片（开关/静默/上限/间隔）。
- 缺失：取消接口、统计接口、任何前端列表展示。

## 后端改动

### 1. 新增取消接口

`DELETE /api/bots/{id}/scheduled-messages/{msgID}`（router 挂在 protected，带 bot 归属校验）：

- 校验 bot 归属：`ListScheduledMessages(botID, 500)` 中找到 `msgID` 且 `msg.BotID == botID`，否则 404「not found or not owned」。
- 仅 `status == "pending"` 可取消；否则 409「只能取消待发送的消息」。
- 成功：`CancelScheduledMessageWithReason(msgID, "控制台取消")` → 200 `{ok: true}`。

### 2. 新增统计接口

`GET /api/bots/{id}/scheduled-messages/stats?days=7`：

- `ListScheduledMessages(botID, 500)`（已有，按 created_at DESC）。
- 内存统计近 N 天（默认 7，上限 30）内 **created_at 或 sent_at** 落入窗口的记录：
  - `total`、`by_status`（pending/sent/failed/cancelled 计数）
  - `by_origin`（promise/proactive 计数）
  - `recent`（最近 10 条完整消息，供列表直接用）
- 返回 `{total, by_status, by_origin, recent}`。
- 列表页可直接用 stats.recent 作为数据源（省一次请求），`listScheduledMessages` 保留不删。

## 前端改动

### 1. `web/src/lib/api.ts`

- `cancelScheduledMessage: (botId, msgId) => request(`/api/bots/${botId}/scheduled-messages/${msgId}`, { method: "DELETE" })`
- `scheduledMessageStats: (botId, days = 7) => request(`/api/bots/${botId}/scheduled-messages/stats?days=${days}`)`

### 2. `web/src/pages/bot-detail.tsx` — 新增「定时/主动消息」卡片区块

放在 bot 详情页工具/应用区块之后：

- **统计摘要行**：近 7 天 sent / failed / cancelled 计数（badge 或小卡片；gated/self-check 归入 cancelled 口径，前端只展示 sent/failed/cancelled/pending 四类，与统一日志 result 语义一致）。
- **列表**：`stats.recent` 表格：触发时间（fire_at，`formatRelativeTime`）、来源（promise/proactive badge）、状态（pending/sent/failed/cancelled badge）、失败/取消原因（fail_reason）、重排次数（reschedule_count）、发送时间（sent_at）。
- **取消按钮**：仅 `status === "pending"` 显示；点击调 `cancelScheduledMessage` → 成功后刷新 stats。
- **空态**：「还没有定时/主动消息」。
- 数据加载失败时展示错误并允许重试；加载中用 Skeleton。

## 测试与验收

- 后端 handler 测试（`internal/api/api_test.go`）：
  - 取消：成功取消 pending（200，状态变 cancelled，fail_reason=控制台取消）
  - 取消：非 pending → 409
  - 取消：不属于该 bot → 404
  - 统计：造几条不同 status/origin 的消息，断言计数正确
- 前端按现有无测试惯例，靠类型与手动路径；跑 `pnpm build`（web 目录）确认编译通过。
- 全量 `go test ./...` + `go build ./...` 通过。

## 范围排除

- 不做重发（failed/sent 重发到新 pending）——留待后续。
- 不做分页（stats.recent 取最近 10 条即可）。
- 不迁移现有统一日志进 DB（统计基于 scheduled_messages 表，已够）。
