# 主动发送消息（结果可交付闭环 + 拟人强化）设计

> 二期第二阶段。依赖第一阶段「Agent 工具交付」已落地的统一交付层（`ToolCallResult.Delivery/Diagnostics`、`deliverToolProducts`、`shouldSkipToolLLM`）。

**目标**：主动消息从「到点单轮合成文本」升级为「到点完整 AI 回合」，并补齐登记/触发/被拦/重排/自省的统一结果语义与日志，为第三阶段控制台打底。

**主架构**：复用第一阶段回复链路（方案 A）。把 `reply()` 的工具循环抽成共享方法 `runAIRound`，回复与主动合成走同一条链路，不新增第二套投递系统。

---

## 0. 现状基线（已查证）

- `internal/persona/schedule.go`：`schedule_message`（origin=promise/proactive，proactive 按 (bot,sender) 去重）、`cancel_schedule`。结果只回文本 Content，无 Delivery/Diagnostics。
- `internal/bot/manager.go` `checkScheduledMessages()`（1 分钟粒度）→ `processDueScheduledMessage()`：claim → 容差 → 在线 → 24h 会话窗口 → `gateProactive`（静默/上限/间隔，仅拦 proactive）→ 热聊二次确认（gapNote <30min）→ `ComposeScheduledMessage`（单轮、无工具）→ 空回复/`【不发】` 则 cancel → `SendHumanized` → 状态落库。
- `internal/sink/ai.go` `ComposeScheduledMessage`：`BuildMessagesForSender`（含人格上下文）+ `CompleteMessages(..., nil)`（无工具）。`EmptyReplyMarker = "【不发】"`。
- store：`ScheduledMessageStore`（Add/ListDue/ListPendingByScope/Claim/MarkSent/MarkFailed/Cancel/CancelWithReason/List）。
- 配置（`store/channel.go` `AIConfig`）：`proactive_enabled`、`proactive_quiet_hours`、`proactive_daily_limit`、`proactive_min_interval`。

---

## 1. 模块 0：抽取共享 AI 回合（主架构）

把 `reply()` 的工具循环（executeToolCall → deliverToolProducts → shouldSkipToolLLM → ContinueWithToolResults → 最终文本）抽成共享方法：

```go
// runAIRound 执行一次完整的 AI 回合（含 persona 工具调用与统一交付），
// 返回最终要发给用户的文本（可能为空）。回复链路与主动合成共用。
func (s *AI) runAIRound(ctx context.Context, d Delivery, cfg store.AIConfig, messages []ai.Message) (string, error)
```

- `reply()` 的回复特有逻辑（状态消息、typing、span/trace 细节、token 统计）留在 `reply()`；工具循环与交付抽入 `runAIRound`。
- 抽取边界以保证现有 `reply()` 行为除「模块 4 说明微调」外不变；第一阶段全部测试兜底。
- 主动合成（`ComposeScheduledMessage` 升级）调用同一方法。

## 2. 模块 1：结果可交付闭环

### 2.1 工具结果元信息（`internal/persona/schedule.go`）

- `schedule_message`：
  - 成功：`Content` 保持现有文本（供 LLM 组织语言）；`Delivery.UserFacing=false`（静默登记）；`Diagnostics.LogFields = {delivery_kind:"schedule", result_count:"1", origin, fire_at}`。
  - 校验失败/内部失败：`UserFacing=false`，`Diagnostics.ErrorStage`（`validate` / `store`）、`ErrorMessage`、`LogFields.delivery_kind:"schedule"`。
- `cancel_schedule`：同理，`result_count` 按取消条数（0/1），`ErrorStage="cancel"` 时带 `ErrorMessage`。
- 效果：每次登记/取消自动进入 `deliverToolProducts` 的统一 `persona delivery` 日志（`explained_to_user=false`）。

### 2.2 manager 触发统一日志（`internal/bot/manager.go`）

`processDueScheduledMessage` 每个出口统一结构化日志，字段与设计 §8 / 第一阶段对齐：

```
slog.Info("scheduled message",
  "tool_name", "schedule",
  "bot_id", botID, "sender", sender, "origin", origin,
  "result", "sent|gated|cancelled|failed|rescheduled|self-check",
  "error_stage", ..., "error_message", ...,
  "reschedule_count", n, "delivery_kind", "text|file|image")
```

各分支：gated（被拦 → 重定）、重排成功、重排超限、promise 自省取消、AI 合成失败、发送失败、发送成功、会话窗口过期等。

## 3. 模块 2：被拦后 AI 现场重定

gate 命中（静默时段 / 每日上限 / 最小间隔）且 `origin=proactive` 时，不再直接 cancel：

1. 调 AI 重定时间（新方法 `ComposeReschedule`）：
   - 输入：原 `prompt`、被拦原因（如「静默时段 23:00-08:00」）、当前时间、角色上下文（`BuildMessagesForSender`）。
   - 输出：新的 `fire_at`（Unix 秒）或放弃（`【不发】`）。
   - system 提示：结合角色性格与原因挑一个更合适的时间；若觉得不该再发则放弃。
2. 重定成功 → 原记录 `CancelScheduledMessageWithReason(id, "重定")`，登记新 pending（`origin=proactive`，`RescheduleCount=旧+1`，同 (bot,sender) 替换旧 pending）。
3. 重排上限：`RescheduleCount >= 3` 时不再重定，直接 `CancelScheduledMessageWithReason(id, "重排超限")`。
4. 每次重排落统一日志（`result="rescheduled"`）。

## 4. 模块 3：promise 必发 + 自省例外

- promise 仍直通 gate（必发、不占 proactive 额度）。
- 到点合成输入显式包含最近对话上下文（现有 `BuildMessagesForSender` 已含），并追加提示：若该约定事件已实现 / 用户已提过，可回 `【不发】` 并说明。
- AI 回 `【不发】` → `CancelScheduledMessageWithReason(id, "AI 判断已实现")`（reason 以 AI 说明为准，去前缀化存储），落统一日志（`result="self-check"`）。
- 与现有「AI 决定不发」的区别：promise 现在也被明确授权自省，且 reason 落库可追溯。

## 5. 模块 4：主动合成升级（融合 + 产物 + 微调说明 + 热聊）

### 5.1 融合

到点输入 = 存储 `prompt` + 当前语境（gapNote / 最近对话）+ 触发说明；`runAIRound` 单轮完成「优化融合」：system 提示 AI「先结合你们最近的对话和这条提示过一遍，再决定怎么说、发什么」。

### 5.2 产物

`runAIRound` 携带完整 persona 工具集（`web_search`/`write_doc`/`gen_image`/…）→ AI 到点可调工具 → `deliverToolProducts` 直发文件/图。`deliverToolProducts` 的 `success=false` 兜底走 LLM 续轮（沿用第一阶段行为）。

### 5.3 微调说明（行为变更，已确认）

**媒体轮（write_doc / gen_image）改为：先发产品本体 → LLM 续轮现场产出微调说明（按语境/性格）→ LLM 空或失败用 canned 兜底。**

- 变更点：`shouldSkipToolLLM` 对「persona + UserFacing + 非空 Explanation + 有媒体」的轮次由「跳过」改为「不跳过」（继续 LLM）；`deliverToolProducts` 媒体轮不再抢先发 canned 说明，改为记录 pending canned 说明，待续轮结果确定后：LLM 文本非空 → 发 LLM 文本；空/失败 → 发 canned 兜底。
- 影响：回复链路与主动链路一致；第一阶段相关测试（`TestReply_PersonaToolProductsDeliveredDirectly` 等）同步更新为「LLM 调 2 次、说明来自 LLM」。
- 纯文本轮（`web_search` conclusion）保持「直发 + 跳过 LLM」不变。

### 5.4 热聊不发稍后淡带一句

触发时在热聊（gapNote <30min），AI 三选一：

1. 直接发：AI 改口贴合当下语境（正常发送）。
2. 回 `【不发】` → cancel（`fail_reason` 记录）。
3. 回 `【稍后】`（可带分钟数，如 `【稍后 10 分钟】`；默认 5 分钟）→ 短延迟重排：登记新 pending（origin 不变，`fire_at=now+N`，`RescheduleCount+1`），复用模块 2 的防死循环上限。

新增常量：`ProactiveDeferMarker = "【稍后】"`（解析可选分钟数）。

## 6. 模块 5：数据模型与迁移

- `scheduled_messages` 新增列：`reschedule_count INTEGER NOT NULL DEFAULT 0`。
- 同步：`store.ScheduledMessage` struct、`store/sqlite` + `store/postgres` migration、`store/storetest` 用例。

## 7. 配置

无新增配置项；复用现有 `ai.proactive_*`。重排上限 3、`【稍后】` 默认 5 分钟作为常量（可后续做成配置）。

## 8. 测试与验收

| 项 | 覆盖 |
|---|---|
| 工具元信息 | `schedule_message`/`cancel_schedule` 的 LogFields/ErrorStage 单测 |
| 现场重定 | gate 命中 → AI 返回新时间 → 新 pending；AI 放弃 → cancel；超限 → cancel |
| promise 自省 | AI 回 `【不发】` → cancel 且 reason 落库 |
| 合成升级 | 输入含 gapNote 融合提示；AI 调 gen_image → 产品直发 + LLM 微调说明；`【稍后】` → 短延迟重排 |
| runAIRound 重构 | 第一阶段 reply() 全量测试保持绿（除 5.3 更新的测试） |
| 数据模型 | sqlite/postgres migration + storetest |
| 全量 | `go test ./...` + `go build ./...` |

验收标准：

- 到点主动消息：产物直发 + 微调说明 + 统一日志可查。
- 被拦 proactive：不直接消失，AI 现场重定；超限才取消。
- promise：必发，但事件已实现时 AI 可自省取消并留痕。
- 热聊中触发：AI 可改口 / 不发 / 稍后淡带一句。
- 登记/触发/被拦/重排/自省结果全部进统一日志，为控制台打底。

## 9. 范围排除（YAGNI）

- 控制台页面、定时消息批量管理 API → 第三阶段「完善控制台」。
- 群聊主动消息（当前仅单聊 sender）。
- 重排上限/`【稍后】` 默认分钟的配置化（先常量）。
