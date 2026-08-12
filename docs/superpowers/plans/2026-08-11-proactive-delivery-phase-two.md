# 主动发送消息（结果可交付闭环 + 拟人强化）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让主动消息从「到点单轮合成文本」升级为「到点完整 AI 回合（融合语境 → 可调工具 → 产品直发 → 微调说明）」，并把登记/触发/被拦/重排/自省统一成结果可交付闭环。

**Architecture:** 复用第一阶段统一交付层。把 `reply()` 的工具循环抽成共享 `runAIRound`，回复与主动合成走同一条链路；被拦的 proactive 由 AI 现场重定时间（`reschedule_count` 防死循环）；promise 必发 + 自省例外；媒体轮说明改为 LLM 续轮微调 + canned 兜底；热聊触发可「改口 / 不发 / 稍后淡带一句」。

**Tech Stack:** Go，现有 persona 引擎、sink 交付层（`deliverToolProducts`/`shouldSkipToolLLM`）、`scheduled_messages` 存储、goose 迁移、Go testing。

---

## File Map

- Modify: `internal/sink/ai.go` — 抽 `runAIRound`；`ComposeScheduledMessage` 升级（融合/工具/自省/【稍后】）；新增 `ComposeReschedule`；`deliverToolProducts` 媒体轮延后 canned；`shouldSkipToolLLM` 调整；新常量 `ProactiveDeferMarker`
- Modify: `internal/persona/schedule.go` — `schedule_message`/`cancel_schedule` 结果带 Diagnostics 元信息
- Modify: `internal/bot/manager.go` — 统一触发日志；被拦→`ComposeReschedule` 现场重定；合成结果解析（发送/【不发】/【稍后】）；promise 自省落库
- Modify: `internal/store/persona.go` — `ScheduledMessage.RescheduleCount` 字段
- Modify: `internal/store/sqlite/persona.go`、`internal/store/postgres/persona.go` — `schedCols`/scan/Add 支持 `reschedule_count`
- Create: `internal/store/sqlite/migrations/0011_proactive_reschedule.sql`
- Create: `internal/store/postgres/migrations/0040_proactive_reschedule.sql`
- Modify: `internal/store/storetest/persona.go` — reschedule_count 往返测试
- Modify: 各相关 `*_test.go`（详见各任务）

## Task 1: 抽取共享 `runAIRound`

**Files:**
- Modify: `internal/sink/ai.go`（`reply()` 工具循环 → 新方法 `runAIRound`）
- Test: `internal/sink/ai_test.go`

背景（已查证）：`reply()`（ai.go:173-444）流程 = 配置/span/typing/图片下载/`BuildMessagesPersona`（:259）→ 首次 `CompleteMessages`（:260）→ 工具循环（:297-381：状态消息 → AppendAssistantToolCalls → executeToolCall → deliverToolProducts → shouldSkipToolLLM → toolResultsForContinuation → ContinueWithToolResults）→ 收尾（setTokenUsage/StripMarkdown/SendHumanized/存历史）。

- [ ] **Step 1: 写失败测试，锁定 `runAIRound` 契约**

在 `internal/sink/ai_test.go` 增加：

```go
func TestRunAIRound_TextRoundReturnsOutcomeText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": "你好呀"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()
	db := testSinkStore(t)
	for k, v := range map[string]string{
		"ai.api_key": "test-key", "ai.base_url": srv.URL, "ai.model": "test-model",
	} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}
	cfg := store.AIConfig{APIKey: "test-key", BaseURL: srv.URL, Model: "test-model"}
	s := &AI{Store: db}
	msgs := []ai.Message{{Role: "user", Content: "hi"}}
	text, thinking, outcome, err := s.runAIRound(context.Background(), Delivery{}, cfg, msgs, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("runAIRound: %v", err)
	}
	if text != "你好呀" {
		t.Fatalf("text = %q, want 你好呀", text)
	}
	if thinking != "" {
		t.Fatalf("thinking = %q, want empty", thinking)
	}
	if outcome != roundOutcomeText {
		t.Fatalf("outcome = %v, want roundOutcomeText", outcome)
	}
}
```

- [ ] **Step 2: 运行测试并确认失败**

Run: `go test ./internal/sink -run TestRunAIRound_TextRoundReturnsOutcomeText -count=1`
Expected: FAIL，`runAIRound undefined`（编译失败）。

- [ ] **Step 3: 抽取 `runAIRound`**

在 `internal/sink/ai.go` 新增类型与方法，并把 `reply()` 中对应逻辑改为调用它（`reply()` 内移走：首次 `CompleteMessages`、工具循环、最终文本提取；保留：span/typing/图片下载/`BuildMessagesPersona`/setTokenUsage/StripMarkdown/SendHumanized/存历史/错误通知）：

```go
// roundOutcome 描述一轮 AI 回合的最终状态。
type roundOutcome int

const (
	roundOutcomeText roundOutcome = iota // Text 里有最终要发给用户的文本
	roundOutcomeDelivered                // 产物+说明已直发，无需再发文本
	roundOutcomeEmpty                    // AI 未产出文本且无直发（视为放弃）
)

// roundResult 是一次 runAIRound 的返回。
type roundResult struct {
	Text     string
	Thinking string
	Outcome  roundOutcome
}

// usageTally 累计一轮内多次 completion 的 token 用量。
type usageTally struct {
	Prompt, Completion, Total, Cached, Reasoning int
}

// runAIRound 执行一次完整 AI 回合（首次 completion + 工具轮 + 统一交付），
// 返回最终文本与结果类型。reply() 与主动合成共用。
// notify 非空时，每个非 persona 工具调用前回调（reply() 用于发"🔧 调用"状态，主动合成传 nil）。
// parentSpan 用于 executeToolCall 的 trace 子 span（reply() 传自己的 span，主动合成传 nil）。
// usage 非空时累计各轮 token 用量（reply() 用于 setTokenUsage）。
func (s *AI) runAIRound(ctx context.Context, d Delivery, cfg store.AIConfig, messages []ai.Message, tools []ai.Tool, notify func(string), parentSpan *store.SpanBuilder, usage *usageTally) (roundResult, error) {
	result, messages, err := ai.CompleteMessages(ctx, cfg, messages, tools)
	if err != nil {
		return roundResult{Outcome: roundOutcomeEmpty}, err
	}
	if usage != nil {
		accumulateUsage(usage, result.Usage)
	}

	// Build installationID → appName map for status messages.
	toolAppNames := make(map[string]string)
	for _, t := range tools {
		if idx := strings.Index(t.Function.Name, "__"); idx >= 0 {
			instID := t.Function.Name[:idx]
			desc := t.Function.Description
			if len(desc) > 1 && desc[0] == '[' {
				if end := strings.Index(desc, "]"); end > 0 {
					toolAppNames[instID] = desc[1:end]
				}
			}
		}
	}

	for round := 0; round < ai.MaxToolRounds && len(result.ToolCalls) > 0; round++ {
		for _, tc := range result.ToolCalls {
			if s.Persona != nil && s.Persona.IsPersonaTool(tc.Name) {
				continue
			}
			toolName := tc.Name
			appName := ""
			if idx := strings.Index(tc.Name, "__"); idx >= 0 {
				appName = toolAppNames[tc.Name[:idx]]
				toolName = tc.Name[idx+2:]
			}
			if notify != nil {
				status := fmt.Sprintf("🔧 调用 %s ...", toolName)
				if appName != "" {
					status = fmt.Sprintf("🔧 调用 %s 的 %s ...", appName, toolName)
				}
				notify(status)
			}
		}

		messages = ai.AppendAssistantToolCalls(messages, result.ToolCalls)

		var toolResults []ai.ToolCallResult
		for _, tc := range result.ToolCalls {
			toolResults = append(toolResults, s.executeToolCall(ctx, d, tc, parentSpan))
		}

		allDelivered, pending := s.deliverToolProducts(ctx, d, toolResults)
		if s.shouldSkipToolLLM(toolResults) && allDelivered {
			// 全直发轮（文本型直接发完、或全部 async），无需续轮。
			return roundResult{Outcome: roundOutcomeDelivered}, nil
		}

		llmResults := toolResultsForContinuation(toolResults)
		result, messages, err = ai.ContinueWithToolResults(ctx, cfg, messages, llmResults, tools)
		if err != nil {
			s.flushPendingExplanations(ctx, d, pending)
			return roundResult{Outcome: roundOutcomeEmpty}, err
		}
		if usage != nil {
			accumulateUsage(usage, result.Usage)
		}
	}

	reply := strings.TrimSpace(result.Content)
	if reply == "" {
		if len(pending) > 0 {
			s.flushPendingExplanations(ctx, d, pending)
			return roundResult{Outcome: roundOutcomeDelivered}, nil
		}
		return roundResult{Outcome: roundOutcomeEmpty}, nil
	}
	return roundResult{Text: reply, Thinking: result.Thinking, Outcome: roundOutcomeText}, nil
}

func accumulateUsage(u *usageTally, usage *ai.Usage) {
	if usage == nil {
		return
	}
	u.Prompt += usage.PromptTokens
	u.Completion += usage.CompletionTokens
	u.Total += usage.TotalTokens
	u.Cached += usage.CachedTokens
	u.Reasoning += usage.ReasoningTokens
}
```

`reply()` 改为（替换 :259-444 的对应段落，保留 span/typing/图片下载/错误通知/StripMarkdown/SendHumanized/存历史；删除内联的工具循环与首次 CompleteMessages）：

```go
	// Build messages for conversation context (reused across tool-call rounds)
	messages := ai.BuildMessagesPersona(ctx, cfg, s.Store, d.BotDBID, d.Channel.ID, sender, text, currentImages, resolver)
	notify := func(status string) {
		d.Provider.Send(ctx, provider.OutboundMessage{Recipient: sender, Text: status})
	}
	var usage usageTally
	res, err := s.runAIRound(ctx, d, cfg, messages, tools, notify, span, &usage)
	s.setTokenUsage(span, d.RootSpan, usage.Prompt, usage.Completion, usage.Total, usage.Cached, usage.Reasoning)
	if err != nil {
		slog.Error("ai completion failed", "bot", d.BotDBID, "err", err)
		if span != nil {
			span.SetStatus(store.StatusError, err.Error())
			span.End()
		}
		s.stopTyping(d, typingTicket)
		s.sendErrorNotice(d, sender)
		return
	}
	if res.Outcome != roundOutcomeText {
		if span != nil {
			span.SetAttr("reply.content", "(tool handled directly)")
			span.End()
		}
		s.stopTyping(d, typingTicket)
		return
	}
	reply := res.Text
	thinking := res.Thinking
	// …… 以下保留原 :395-443 的 thinking span 属性 / StripMarkdown / 空回复判断（改为 res.Outcome 已覆盖）/ SendHumanized / 存历史
```

注意：`runAIRound` 已包含「reply 为空」判定，`reply()` 中旧的 `if reply == "" { return }` 由 `res.Outcome != roundOutcomeText` 分支覆盖；其余（StripMarkdown、SendHumanized、SaveMessage、span 属性）原样保留。

- [ ] **Step 4: 运行测试并确认通过**

Run: `go test ./internal/sink -count=1`
Expected: PASS（除 Task 5 将更新的媒体轮测试外，第一阶段全部测试保持绿）。若 `flushPendingExplanations`/`deliverToolProducts` 新签名未实现导致编译失败，先按 Task 5 的最小形态补齐（`deliverToolProducts` 暂返回 `(bool, nil)`，`flushPendingExplanations` 为空实现），保证本任务可编译通过。

- [ ] **Step 5: 提交**

```bash
git add internal/sink/ai.go internal/sink/ai_test.go
git commit -m "refactor(agent): extract shared runAIRound for reply and proactive"
```

## Task 2: `scheduled_messages` 增加 `reschedule_count`

**Files:**
- Modify: `internal/store/persona.go`（`ScheduledMessage` struct）
- Modify: `internal/store/sqlite/persona.go`、`internal/store/postgres/persona.go`
- Create: `internal/store/sqlite/migrations/0011_proactive_reschedule.sql`
- Create: `internal/store/postgres/migrations/0040_proactive_reschedule.sql`
- Modify: `internal/store/storetest/persona.go`

- [ ] **Step 1: 写失败测试（store 往返）**

在 `internal/store/storetest/persona.go`（`TestScheduledMessageStore` 或等价函数内）加子用例：`AddScheduledMessage` 写入 `RescheduleCount: 2` 后 `ListDueScheduledMessages`/`ListPendingByScope` 读回 `RescheduleCount == 2`。

- [ ] **Step 2: 运行并确认失败**

Run: `go test ./internal/store/sqlite -run TestScheduledMessageStore -count=1`
Expected: FAIL（字段未定义或读回为 0）。

- [ ] **Step 3: 实现字段与迁移**

`internal/store/persona.go` 的 `ScheduledMessage` 增加：

```go
	RescheduleCount int64 `json:"reschedule_count"` // 被防打扰闸门拦截后的重定次数（防死循环）
```

迁移文件（goose 格式，`-- +goose Up` / `-- +goose Down`）：

`internal/store/sqlite/migrations/0011_proactive_reschedule.sql`：
```sql
-- +goose Up
ALTER TABLE scheduled_messages ADD COLUMN reschedule_count INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE scheduled_messages DROP COLUMN reschedule_count;
```

`internal/store/postgres/migrations/0040_proactive_reschedule.sql`：同上（Postgres 也支持 `DROP COLUMN`）。

`internal/store/sqlite/persona.go`：
- `schedCols`（:357）改为 `id, bot_id, sender, fire_at, tolerance, prompt, origin, status, fail_reason, created_at, sent_at, reschedule_count`
- `scanScheduledMessage`（:350）的 `Scan` 追加 `&m.RescheduleCount`（参数顺序与 schedCols 一致）
- `AddScheduledMessage`（:371）的 INSERT 列与 VALUES 追加 `reschedule_count` 与 `m.RescheduleCount`

`internal/store/postgres/persona.go` 同步修改（对应 :359/:370 附近，列名/占位符按 `$` 序号对齐）。

- [ ] **Step 4: 运行并确认通过**

Run: `go test ./internal/store/sqlite ./internal/store/postgres -run TestScheduledMessageStore -count=1`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/store/persona.go internal/store/sqlite/persona.go internal/store/postgres/persona.go internal/store/sqlite/migrations/0011_proactive_reschedule.sql internal/store/postgres/migrations/0040_proactive_reschedule.sql internal/store/storetest/persona.go
git commit -m "feat(store): track proactive reschedule count"
```

## Task 3: `schedule_message` / `cancel_schedule` 工具结果元信息

**Files:**
- Modify: `internal/persona/schedule.go`
- Modify: `internal/persona/persona_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/persona/persona_test.go` 增加：

```go
func TestScheduleMessage_SetsDeliveryDiagnostics(t *testing.T) {
	e, db, botID := newTestEngine(t, 100)
	res := e.Execute(context.Background(), botID, "wxid_a", "schedule_message",
		json.RawMessage(`{"fire_at":"2026-08-12 06:30","prompt":"叫起床","origin":"promise"}`))
	if res.Delivery.UserFacing {
		t.Fatal("scheduling should stay silent (not user-facing)")
	}
	if res.Diagnostics.LogFields["delivery_kind"] != "schedule" {
		t.Fatalf("delivery_kind = %q, want schedule", res.Diagnostics.LogFields["delivery_kind"])
	}
	if res.Diagnostics.LogFields["origin"] != "promise" {
		t.Fatalf("origin = %q, want promise", res.Diagnostics.LogFields["origin"])
	}
}

func TestCancelSchedule_SetsDeliveryDiagnostics(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	res := e.Execute(context.Background(), botID, "wxid_a", "cancel_schedule", json.RawMessage(`{"id":"missing"}`))
	if res.Diagnostics.LogFields["delivery_kind"] != "schedule" {
		t.Fatalf("delivery_kind = %q, want schedule", res.Diagnostics.LogFields["delivery_kind"])
	}
	if res.Diagnostics.ErrorStage != "cancel" {
		t.Fatalf("error stage = %q, want cancel", res.Diagnostics.ErrorStage)
	}
	if res.Diagnostics.ErrorMessage == "" {
		t.Fatal("expected diagnostic error message for not-found cancel")
	}
}
```

- [ ] **Step 2: 运行并确认失败**

Run: `go test ./internal/persona -run 'TestScheduleMessage_SetsDeliveryDiagnostics|TestCancelSchedule_SetsDeliveryDiagnostics' -count=1`
Expected: FAIL（`delivery_kind` 为空）。

- [ ] **Step 3: 最小实现**

`internal/persona/schedule.go`：

- `handleScheduleMessage` 成功分支改为：

```go
	return ai.ToolCallResult{
		Content: fmt.Sprintf("已登记，id=%s，origin=%s，fire_at=%s", m.ID, origin, t.Format("2006-01-02 15:04")),
		Diagnostics: ai.ToolCallDiagnostics{LogFields: map[string]string{
			"delivery_kind": "schedule",
			"result_count":  "1",
			"origin":        origin,
			"fire_at":       t.Format("2006-01-02 15:04"),
		}},
	}
```

- 校验失败分支（:19/:23）带 `Diagnostics{ErrorStage: "validate", ErrorMessage: ..., LogFields: {delivery_kind: "schedule", result_count: "0"}}`；store 失败分支（:51）带 `ErrorStage: "store"`。
- `handleCancelSchedule`：找到并取消 → `LogFields{delivery_kind: "schedule", result_count: "1"}`；未找到 → `ErrorStage: "cancel", ErrorMessage: "not found", LogFields{delivery_kind: "schedule", result_count: "0"}`；缺 id → `ErrorStage: "validate", ErrorMessage: "id required"`。

（`Delivery` 保持零值：`UserFacing=false`，静默登记，由 AI 自行组织对用户的回复。）

- [ ] **Step 4: 运行并确认通过**

Run: `go test ./internal/persona -run 'TestScheduleMessage_|TestCancelSchedule_' -count=1 && go test ./internal/persona -count=1`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/persona/schedule.go internal/persona/persona_test.go
git commit -m "feat(persona): schedule tools emit delivery diagnostics"
```

## Task 4: manager 触发统一日志

**Files:**
- Modify: `internal/bot/manager.go`
- Test: `internal/bot/proactive_test.go`

- [ ] **Step 1: 写失败测试（slog 捕获）**

在 `internal/bot/proactive_test.go` 增加：

```go
func TestProcessDueScheduledMessage_LogsUnifiedFields(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "proactive_log.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	user, _ := db.CreateUser("sched_log_owner", "Sched Log")
	botRow, _ := db.CreateBot(user.ID, "LogBot", "test", "wx_bot", json.RawMessage(`{}`))
	msg := &store.ScheduledMessage{BotID: botRow.ID, Sender: "wxid_a", FireAt: time.Now().Add(-30 * time.Minute).Unix(), Tolerance: 60, Prompt: "x", Origin: "promise", Status: "pending"}
	if err := db.AddScheduledMessage(msg); err != nil {
		t.Fatal(err)
	}

	var got string
	h := &captureSlogHandler{fn: func(rec slog.Record) {
		var result string
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "result" {
				result = a.Value.String()
			}
			return true
		})
		if result != "" {
			got = result
		}
	}}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(old)

	m := &Manager{store: db, aiSink: &sink.AI{}, instances: map[string]*Instance{botRow.ID: NewInstance(botRow.ID, &fakeProvider{})}}
	m.processDueScheduledMessage(msg, store.AIConfig{}, time.Now())

	if got != "cancelled" {
		t.Fatalf("logged result = %q, want cancelled", got)
	}
}
```

同时在测试文件加捕获 handler（handler 需实现 `slog.Handler` 接口：`Enabled`/`Handle`/`WithAttrs`/`WithGroup`，`Handle` 里调用 `fn(rec)`，其余返回自身/原样）：

```go
type captureSlogHandler struct {
	fn func(rec slog.Record)
}

func (h *captureSlogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureSlogHandler) Handle(ctx context.Context, rec slog.Record) error {
	h.fn(rec)
	return nil
}
func (h *captureSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureSlogHandler) WithGroup(string) slog.Handler      { return h }
```

（需要新增 import：`context`、`log/slog`。）

- [ ] **Step 2: 运行并确认失败**

Run: `go test ./internal/bot -run TestProcessDueScheduledMessage_LogsUnifiedFields -count=1`
Expected: FAIL（`got == ""`，因为尚未输出统一 `result` 字段）。

- [ ] **Step 3: 实现统一日志**

在 `internal/bot/manager.go` 增加小助手并在 `processDueScheduledMessage` 各出口调用：

```go
// logScheduledResult 输出触发结果统一日志（为第三阶段控制台打底）。
func logScheduledResult(botID, sender, origin, result, errorStage, errorMessage string, rescheduleCount int64) {
	slog.Info("scheduled message",
		"tool_name", "schedule",
		"bot_id", botID,
		"sender", sender,
		"origin", origin,
		"result", result,
		"error_stage", errorStage,
		"error_message", errorMessage,
		"reschedule_count", rescheduleCount,
		"delivery_kind", "text",
	)
}
```

各出口替换/补充（用现有变量）：
- 容差过期（:256）：`logScheduledResult(msg.BotID, msg.Sender, msg.Origin, "cancelled", "tolerance", "超出容差", msg.RescheduleCount)`
- bot 不在线（:262）：`result="failed", error_stage="offline"`
- 会话窗口过期（:269）：`result="failed", error_stage="window"`
- 被拦→取消/重定（:278，Task 6 后分支会变）：`result="gated"`（重定成功则 `result="rescheduled"`）
- 合成失败（:297）：`result="failed", error_stage="compose"`
- AI 决定不发（:302）：`result="cancelled", error_stage="ai-decline"`
- 发送失败（:308）：`result="failed", error_stage="send"`
- 发送成功（:317）：`result="sent"`

保留原有 `slog.Info("proactive message gated"...)` / `slog.Info("scheduled message sent"...)` 或删除均可（统一日志已覆盖）。

- [ ] **Step 4: 运行并确认通过**

Run: `go test ./internal/bot -run 'TestProcessDueScheduledMessage' -count=1 && go test ./internal/bot -count=1`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/bot/manager.go internal/bot/proactive_test.go
git commit -m "feat(bot): unified scheduled-message trigger logging"
```

## Task 5: 微调说明行为变更（媒体轮延后 canned + 续轮微调）

**Files:**
- Modify: `internal/sink/ai.go`（`deliverToolProducts`、`shouldSkipToolLLM`、新增 `flushPendingExplanations`）
- Modify: `internal/sink/ai_test.go`、`internal/sink/humanize_test.go`

背景：第一阶段媒体轮是「产品 + canned 说明直发并跳过 LLM」。本任务改为「产品直发 → LLM 续轮现场产出微调说明；LLM 空/失败用 canned 兜底」。纯文本轮（web_search）行为不变。

- [ ] **Step 1: 先更新受影响测试（会先失败）**

`internal/sink/ai_test.go`：
- `TestDeliverToolProducts_SendsFileThenExplanation`：改为断言文件先发、canned 说明**不立即发**、返回 pending 含该说明：

```go
func TestDeliverToolProducts_DefersFileExplanation(t *testing.T) {
	db := testSinkStore(t)
	eng := persona.New(db, nil, fakeCompleter)
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{}
	d := Delivery{BotDBID: "bot1", Provider: p, Message: provider.InboundMessage{Sender: "wxid_a", ContextToken: "ctx-1"}}

	allDelivered, pending := s.deliverToolProducts(context.Background(), d, []aiinternal.ToolCallResult{{
		Name:     "write_doc",
		Files:    []aiinternal.FileData{{FileName: "旅行计划.md", ContentType: "text/markdown", Data: []byte("# hi")}},
		Delivery: ainternal.ToolCallDelivery{UserFacing: true, Explanation: "我整理好了，你直接看这个文档就行。"},
		Diagnostics: ainternal.ToolCallDiagnostics{LogFields: map[string]string{"delivery_kind": "file"}},
	}})
	if !allDelivered {
		t.Fatal("expected successful media delivery")
	}
	msgs := p.messages()
	if len(msgs) != 1 || msgs[0].Data == nil || msgs[0].FileName != "旅行计划.md" {
		t.Fatalf("expected only the file message first, got %+v", msgs)
	}
	if len(pending) != 1 || pending[0] != "我整理好了，你直接看这个文档就行。" {
		t.Fatalf("pending explanations = %#v, want the canned sentence deferred", pending)
	}
}
```

- `TestDeliverToolProducts_SendsImagesThenExplanation`：同样改为断言图先发、说明进 pending。
- `TestDeliverToolProducts_SavesExplanationToHistory`：改为针对**文本轮**（web_search）验证立即说明入历史（`TestDeliverToolProducts_TextOnlySendsExplanationDirectly` 追加历史断言）；媒体轮的 canned 入历史由 `flushPendingExplanations` 路径测试覆盖（见下）。
- `TestDeliverToolProducts_ReturnsFalseWhenMediaSendFails` / `_FileSendFails`：保持（媒体失败 → `allDelivered=false`）。
- `TestDeliverToolProducts_ReturnsFalseWhenExplanationSendFails`：针对文本轮保持。
- 新增 `TestFlushPendingExplanations_SendsAndSaves`：

```go
func TestFlushPendingExplanations_SendsAndSaves(t *testing.T) {
	db := testSinkStore(t)
	eng := persona.New(db, nil, fakeCompleter)
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{}
	d := Delivery{BotDBID: "bot1", Provider: p, Message: provider.InboundMessage{Sender: "wxid_a", ContextToken: "ctx-1"}}

	s.flushPendingExplanations(context.Background(), d, []string{"我先把图发你，你看看是不是这个感觉。"})
	msgs := p.messages()
	if len(msgs) != 1 || msgs[0].Text != "我先把图发你，你看看是不是这个感觉。" {
		t.Fatalf("expected explanation sent, got %+v", msgs)
	}
	history, err := db.ListMessagesBySender("bot1", "wxid_a", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range history {
		if m.Direction == "outbound" && strings.Contains(string(m.ItemList), "我先把图发你") {
			found = true
		}
	}
	if !found {
		t.Fatalf("flushed explanation not saved to history: %+v", history)
	}
}
```

`internal/sink/humanize_test.go` 的 `TestReply_PersonaToolProductsDeliveredDirectly`：改为 mock LLM 第一次返回 write_doc tool_calls、第二次返回 content「文档给你整理好了，直接看这个就行。」，断言：文件先发、续轮文本随后发、LLM 恰被调用 2 次。

- [ ] **Step 2: 运行并确认失败**

Run: `go test ./internal/sink -run 'TestDeliverToolProducts|TestFlushPendingExplanations|TestReply_PersonaToolProductsDeliveredDirectly' -count=1`
Expected: FAIL（编译失败：`deliverToolProducts` 返回值不匹配 / `flushPendingExplanations undefined`）。

- [ ] **Step 3: 最小实现**

`internal/sink/ai.go`：

- `deliverToolProducts` 签名改为 `func (s *AI) deliverToolProducts(ctx context.Context, d Delivery, results []ai.ToolCallResult) (allDelivered bool, pendingExplanations []string)`：
  - 文本轮（无 Files 且无 Images）：`UserFacing && Explanation != ""` 时立即 `SendHumanized` + 存历史（`explained` 记录）；发送失败 → `allDelivered=false`。
  - 媒体轮（有 Files 或 Images）：**不立即发说明**，把 `tr.Delivery.Explanation`（非空时）append 进 `pendingExplanations`；文件/图片发送失败 → `allDelivered=false`。
  - 统一日志保留（`explained_to_user` 对媒体轮记 false——说明延后，由 flush 路径另行记录，可忽略该字段在媒体轮的值）。
- `shouldSkipToolLLM` 改为：persona + `UserFacing` + 非空 `Explanation` **且无媒体** 才算「全直发」（媒体轮返回 false，继续 LLM）：

```go
		if s.Persona != nil && s.Persona.IsPersonaTool(tr.Name) &&
			tr.Delivery.UserFacing && strings.TrimSpace(tr.Delivery.Explanation) != "" &&
			len(tr.Files) == 0 && len(tr.Images) == 0 {
			continue
		}
```

- 新增：

```go
// flushPendingExplanations 在续轮无文本/失败时，把媒体轮延后的 canned 说明补发给用户并入库。
func (s *AI) flushPendingExplanations(ctx context.Context, d Delivery, pending []string) {
	cfg := s.resolveGlobalConfig()
	for _, exp := range pending {
		if err := s.SendHumanized(ctx, d.Provider, d.Message.Sender, exp, d.Message.ContextToken, cfg); err != nil {
			slog.Error("ai tool delivery: fallback explanation send failed", "bot", d.BotDBID, "err", err)
			continue
		}
		itemList, _ := json.Marshal([]map[string]any{{"type": "text", "text": exp}})
		s.Store.SaveMessage(&store.Message{
			BotID: d.BotDBID, Direction: "outbound", ToUserID: d.Message.Sender, MessageType: 2, ItemList: itemList,
		})
	}
}
```

（`runAIRound` 中已调用 `flushPendingExplanations`，Task 1 的占位实现被本任务替换为真实实现。）

- [ ] **Step 4: 运行并确认通过**

Run: `go test ./internal/sink -count=1 && go test ./internal/persona ./internal/ai -count=1`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/sink/ai.go internal/sink/ai_test.go internal/sink/humanize_test.go
git commit -m "feat(agent): media rounds defer canned explanation to LLM refinement"
```

## Task 6: 被拦后 AI 现场重定

**Files:**
- Modify: `internal/sink/ai.go`（新增 `ComposeReschedule`）
- Modify: `internal/bot/manager.go`（gate 分支接线）
- Modify: `internal/sink/ai_test.go`、`internal/bot/proactive_test.go`

- [ ] **Step 1: 写失败测试**

`internal/sink/ai_test.go`：

```go
func TestComposeReschedule_ReturnsNewTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": "2026-08-12 09:30"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()
	db := testSinkStore(t)
	for k, v := range map[string]string{
		"ai.api_key": "test-key", "ai.base_url": srv.URL, "ai.model": "test-model",
	} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}
	s := &AI{Store: db}
	ts, ok, err := s.ComposeReschedule(context.Background(), "bot1", "wxid_a", "想你了", "静默时段 23:00-08:00")
	if err != nil || !ok {
		t.Fatalf("ComposeReschedule = (%d, %v, %v), want ok", ts, ok, err)
	}
	if ts <= time.Now().Unix() {
		t.Fatalf("rescheduled fire_at %d should be in the future", ts)
	}
}

func TestComposeReschedule_Declines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": "【不发】"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()
	db := testSinkStore(t)
	for k, v := range map[string]string{
		"ai.api_key": "test-key", "ai.base_url": srv.URL, "ai.model": "test-model",
	} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}
	s := &AI{Store: db}
	_, ok, err := s.ComposeReschedule(context.Background(), "bot1", "wxid_a", "想你了", "静默时段")
	if err != nil {
		t.Fatalf("ComposeReschedule: %v", err)
	}
	if ok {
		t.Fatal("expected decline when AI returns 【不发】")
	}
}
```

- [ ] **Step 2: 运行并确认失败**

Run: `go test ./internal/sink -run 'TestComposeReschedule' -count=1`
Expected: FAIL（`ComposeReschedule undefined`）。

- [ ] **Step 3: 实现 `ComposeReschedule`**

`internal/sink/ai.go`：

```go
// ComposeReschedule 在被防打扰闸门拦截后，请 AI 现场重定一个更合适的时间。
// 返回新的 fire_at（Unix 秒）；AI 放弃时 ok=false。
func (s *AI) ComposeReschedule(ctx context.Context, botID, sender, prompt, reason string) (int64, bool, error) {
	cfg := s.resolveConfig("")
	if cfg.APIKey == "" {
		return 0, false, fmt.Errorf("ai not configured")
	}
	userText := fmt.Sprintf("原计划：%s。\n刚才到点想发但被拦下了：%s。\n请结合角色性格挑一个更合适的时间（格式 YYYY-MM-DD HH:MM）；若觉得不该再发，直接回复%s。", prompt, reason, EmptyReplyMarker)
	messages := ai.BuildMessagesForSender(ctx, cfg, s.Store, botID, sender, userText)
	result, err := ai.CompleteMessages(ctx, cfg, messages, nil)
	if err != nil {
		return 0, false, err
	}
	content := strings.TrimSpace(result.Content)
	if content == "" || strings.Contains(content, EmptyReplyMarker) {
		return 0, false, nil
	}
	t, err := persona.ParseTime(content)
	if err != nil {
		return 0, false, nil // 无法解析视为放弃，不产生错误日志噪音
	}
	return t.Unix(), true, nil
}
```

（`persona` 需新增导出 `ParseTime(s string) (time.Time, error)`（现有 `parseTime` 的直接导出），`internal/persona/schedule.go` 内部改用 `ParseTime`。`internal/sink/ai.go` 已 import persona 包，无需新增 import。）

```go
// ParseTime 解析 ISO 格式或 YYYY-MM-DD HH:MM 为本地时间。
func ParseTime(s string) (time.Time, error) { return parseTime(s) }
```

- [ ] **Step 4: manager 接线（TDD）**

`internal/bot/proactive_test.go` 增加：gate 命中（`origin=proactive`、配置 `ai.proactive_enabled=false` 或静默时段命中）时，`processDueScheduledMessage` 调用 `ComposeReschedule` 并登记新 pending、原记录 cancelled、`RescheduleCount+1`。测试用 httptest mock AI（同 `TestComposeReschedule_ReturnsNewTime` 配置方式）构造 `sink.AI`，`Manager{store, aiSink, instances}`，断言：
- 新 pending 存在且 `FireAt == 返回时间`、`RescheduleCount == 1`；
- 原记录 `status == "cancelled"`、`fail_reason == "重定"`。

先跑失败（当前 gate 分支直接 cancel，无新 pending），再实现：

`internal/bot/manager.go` gate 分支（替换 :277-281）：

```go
	if g := gateProactive(msg, lastUserMsgAt, now, cfg, sentLog); !g.Allow {
		if msg.Origin == "proactive" && msg.RescheduleCount < maxProactiveReschedules {
			newAt, ok, rerr := m.aiSink.ComposeReschedule(ctx, msg.BotID, msg.Sender, msg.Prompt, g.Reason)
			switch {
			case rerr == nil && ok:
				m.store.CancelScheduledMessageWithReason(msg.ID, "重定")
				nm := &store.ScheduledMessage{
					BotID: msg.BotID, Sender: msg.Sender, FireAt: newAt,
					Tolerance: msg.Tolerance, Prompt: msg.Prompt, Origin: "proactive",
					Status: "pending", RescheduleCount: msg.RescheduleCount + 1,
				}
				if aerr := m.store.AddScheduledMessage(nm); aerr != nil {
					slog.Error("proactive reschedule add failed", "bot", msg.BotID, "err", aerr)
				} else {
					logScheduledResult(msg.BotID, msg.Sender, msg.Origin, "rescheduled", "", "", nm.RescheduleCount)
					return
				}
			case rerr == nil && !ok:
				m.store.CancelScheduledMessageWithReason(msg.ID, "AI 放弃重定")
				logScheduledResult(msg.BotID, msg.Sender, msg.Origin, "cancelled", "ai-decline", "AI 放弃重定", msg.RescheduleCount)
				return
			default:
				slog.Error("proactive reschedule failed", "bot", msg.BotID, "err", rerr)
			}
		}
		m.store.CancelScheduledMessageWithReason(msg.ID, g.Reason)
		logScheduledResult(msg.BotID, msg.Sender, msg.Origin, "gated", "gate", g.Reason, msg.RescheduleCount)
		return
	}
```

新增常量：`const maxProactiveReschedules = 3`（manager.go）。

- [ ] **Step 5: 运行并确认通过**

Run: `go test ./internal/sink -run TestComposeReschedule -count=1 && go test ./internal/bot -run 'TestProcessDueScheduledMessage' -count=1 && go test ./internal/bot ./internal/sink ./internal/persona -count=1`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/sink/ai.go internal/sink/ai_test.go internal/bot/manager.go internal/bot/proactive_test.go
git commit -m "feat(agent): reschedule gated proactive messages via AI"
```

## Task 7: 主动合成升级（融合 + promise 自省 + 产物 + 【稍后】/热聊）

**Files:**
- Modify: `internal/sink/ai.go`（`ComposeScheduledMessage` 升级、`ProactiveDeferMarker`、`parseProactiveOutcome`）
- Modify: `internal/bot/manager.go`（合成结果解析：发送/【不发】/【稍后】；promise 自省落库；`ComposeScheduledMessage` 新签名调用）
- Modify: `internal/sink/ai_test.go`、`internal/bot/proactive_test.go`

- [ ] **Step 1: 写失败测试**

`internal/sink/ai_test.go`：

```go
func TestComposeScheduledMessage_GeneratesImageWithRefinedExplanation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if calls.Load() == 1 {
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{"role": "assistant", "tool_calls": []map[string]any{
						{"id": "call_1", "type": "function", "function": map[string]any{"name": "gen_image", "arguments": `{"prompt":"雨后彩虹"}`}},
					}},
					"finish_reason": "tool_calls",
				}},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": "刚画了一张，你看看这个感觉对不对"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()
	db := testSinkStore(t)
	for k, v := range map[string]string{
		"ai.api_key": "test-key", "ai.base_url": srv.URL, "ai.model": "test-model", "ai.humanize_wechat": "false",
	} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}
	eng := persona.New(db, nil, fakeCompleter)
	eng.SetImageProvider(&fakeProactiveImage{}) // 见下：sink 测试包内实现 persona.ImageProvider
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{}

	text, outcome, err := s.ComposeScheduledMessage(context.Background(), "bot1", "wxid_a", "想给你画张彩虹", "你们刚聊过", p)
	if err != nil {
		t.Fatalf("ComposeScheduledMessage: %v", err)
	}
	if outcome != ComposeOutcomeText {
		t.Fatalf("outcome = %v, want ComposeOutcomeText", outcome)
	}
	if !strings.Contains(text, "刚画了一张") {
		t.Fatalf("final text = %q, want the refined explanation", text)
	}
	if calls.Load() != 2 {
		t.Fatalf("LLM calls = %d, want 2 (tool round + refined explanation)", calls.Load())
	}
	msgs := p.messages()
	if len(msgs) < 1 || msgs[0].Data == nil {
		t.Fatalf("expected the generated image delivered first, got %+v", msgs)
	}
}
```

（`fakeProactiveImage` 为 `internal/sink/ai_test.go` 内新增的实现 `persona.ImageProvider` 接口的桩：`Generate(ctx, prompt, negativePrompt string, opts persona.ImageOptions) ([]byte, error)` 返回一个合法 PNG 魔数切片。`persona.ImageProvider`/`persona.ImageOptions` 均已导出，sink 测试包可直接引用。注意：合成的最终文本由 manager 调用 `SendHumanized` 发出，`ComposeScheduledMessage` 只返回文本；单测里 `p.messages()` 只含工具直发的图片。）

`internal/bot/proactive_test.go`：

```go
func TestParseProactiveOutcome_Defer(t *testing.T) {
	action, minutes, reason := parseProactiveOutcome("【稍后 10 分钟】")
	if action != proactiveDefer || minutes != 10 || reason != "" {
		t.Fatalf("defer parse = (%v, %d, %q)", action, minutes, reason)
	}
	action, minutes, reason = parseProactiveOutcome("【稍后】")
	if action != proactiveDefer || minutes != proactiveDeferDefaultMinutes {
		t.Fatalf("defer default = (%v, %d, %q)", action, minutes, reason)
	}
	action, _, reason = parseProactiveOutcome("【不发】聊过了")
	if action != proactiveDecline || reason == "" {
		t.Fatalf("decline parse = (%v, %q)", action, reason)
	}
	action, _, _ = parseProactiveOutcome("好的，我这就发")
	if action != proactiveSend {
		t.Fatalf("send parse = %v", action)
	}
}
```

- [ ] **Step 2: 运行并确认失败**

Run: `go test ./internal/sink -run TestComposeScheduledMessage_UsesToolsAndFusion -count=1 && go test ./internal/bot -run TestParseProactiveOutcome_Defer -count=1`
Expected: FAIL（`ComposeScheduledMessage` 签名不符 / `parseProactiveOutcome` undefined）。

- [ ] **Step 3: 实现**

`internal/sink/ai.go`：

```go
// ProactiveDeferMarker：AI 表示想等几分钟再发。
const ProactiveDeferMarker = "【稍后】"

// composeOutcome 描述主动合成结果。
type composeOutcome int

// ComposeOutcome* 导出给 manager 判断结果类型。
const (
	ComposeOutcomeText composeOutcome = iota // Text 为最终要发的文本
	ComposeOutcomeDelivered                  // 产物+说明已直发（无需再发文本）
	ComposeOutcomeEmpty                      // AI 放弃（无文本、无直发）
)

// ComposeScheduledMessage 到点合成主动消息：融合存储 prompt 与当前语境，
// 带 persona 工具集（可发文本/文件/图），返回最终文本与结果类型。
// p 是发送所需的 provider（产物/说明直发要用）。
func (s *AI) ComposeScheduledMessage(ctx context.Context, botID, sender, prompt, gapNote string, p provider.Provider) (string, composeOutcome, error) {
	cfg := s.resolveConfig("")
	if cfg.APIKey == "" {
		return "", ComposeOutcomeEmpty, fmt.Errorf("ai not configured")
	}
	userText := prompt + "\n\n（这是一条到点的主动联系。"
	if gapNote != "" {
		userText += "\n" + gapNote + "。"
	}
	userText += "先结合你们最近的对话把这条提示融成此刻合适的话再回。若觉得现在不适合发，可回" + EmptyReplyMarker + "；若想等几分钟再发，可回" + ProactiveDeferMarker + "（可写分钟数，如" + ProactiveDeferMarker + " 10 分钟）。）"
	messages := ai.BuildMessagesForSender(ctx, cfg, s.Store, botID, sender, userText)
	tools := s.collectTools(botID)
	d := Delivery{BotDBID: botID, Provider: p, Message: provider.InboundMessage{Sender: sender}}
	res, err := s.runAIRound(ctx, d, cfg, messages, tools, nil, nil, nil)
	if err != nil {
		return "", ComposeOutcomeEmpty, err
	}
	switch res.Outcome {
	case roundOutcomeDelivered:
		return "", ComposeOutcomeDelivered, nil
	case roundOutcomeEmpty:
		return "", ComposeOutcomeEmpty, nil
	default:
		return strings.TrimSpace(res.Text), ComposeOutcomeText, nil
	}
}
```

`internal/bot/manager.go`：

```go
// 主动结果动作。
type proactiveAction int

const (
	proactiveSend proactiveAction = iota
	proactiveDecline
	proactiveDefer
)

const proactiveDeferDefaultMinutes = 5

// parseProactiveOutcome 解析合成文本：发送 / 【不发】(附理由) / 【稍后 N 分钟】。
func parseProactiveOutcome(text string) (action proactiveAction, deferMinutes int, reason string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return proactiveDecline, 0, "AI 决定不发"
	}
	if strings.Contains(trimmed, EmptyReplyMarker) {
		reason := strings.TrimSpace(strings.ReplaceAll(trimmed, EmptyReplyMarker, ""))
		if reason == "" {
			reason = "AI 决定不发"
		}
		return proactiveDecline, 0, reason
	}
	if strings.Contains(trimmed, sink.ProactiveDeferMarker) {
		mins := proactiveDeferDefaultMinutes
		re := regexp.MustCompile(`(\d+)\s*分钟`)
		if m := re.FindStringSubmatch(trimmed); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && n <= 60 {
				mins = n
			}
		}
		return proactiveDefer, mins, ""
	}
	return proactiveSend, 0, ""
}
```

（`manager.go` 需新增 import：`regexp`、`strconv`；`sink` 包引用需确认无循环依赖——manager 已在用 `sink.AI`/`sink.EmptyReplyMarker`，无碍。）

`manager.go` 合成段替换（:293-317）：

```go
	ctx := context.Background()
	text, outcome, err := m.aiSink.ComposeScheduledMessage(ctx, msg.BotID, msg.Sender, msg.Prompt, gapNote, inst.Provider)
	if err != nil {
		slog.Error("proactive composition failed", "bot", msg.BotID, "id", msg.ID, "err", err)
		m.store.MarkScheduledMessageFailed(msg.ID, "AI 生成失败")
		logScheduledResult(msg.BotID, msg.Sender, msg.Origin, "failed", "compose", err.Error(), msg.RescheduleCount)
		return
	}

	switch outcome {
	case sink.ComposeOutcomeDelivered:
		// 产物+说明已直发；消息本体即为已交付内容。
		m.store.MarkScheduledMessageSent(msg.ID, now.Unix())
		logScheduledResult(msg.BotID, msg.Sender, msg.Origin, "sent", "", "", msg.RescheduleCount)
		return
	case sink.ComposeOutcomeEmpty:
		m.store.CancelScheduledMessageWithReason(msg.ID, "AI 决定不发")
		logScheduledResult(msg.BotID, msg.Sender, msg.Origin, "cancelled", "ai-decline", "AI 决定不发", msg.RescheduleCount)
		return
	}

	action, deferMinutes, reason := parseProactiveOutcome(text)
	switch action {
	case proactiveDecline:
		m.store.CancelScheduledMessageWithReason(msg.ID, reason)
		logScheduledResult(msg.BotID, msg.Sender, msg.Origin, "cancelled", "ai-decline", reason, msg.RescheduleCount)
		return
	case proactiveDefer:
		if msg.RescheduleCount >= maxProactiveReschedules {
			m.store.CancelScheduledMessageWithReason(msg.ID, "重排超限")
			logScheduledResult(msg.BotID, msg.Sender, msg.Origin, "cancelled", "defer-limit", "重排超限", msg.RescheduleCount)
			return
		}
		m.store.CancelScheduledMessageWithReason(msg.ID, "稍后")
		nm := &store.ScheduledMessage{
			BotID: msg.BotID, Sender: msg.Sender, FireAt: now.Add(time.Duration(deferMinutes) * time.Minute).Unix(),
			Tolerance: msg.Tolerance, Prompt: msg.Prompt, Origin: msg.Origin,
			Status: "pending", RescheduleCount: msg.RescheduleCount + 1,
		}
		if aerr := m.store.AddScheduledMessage(nm); aerr != nil {
			slog.Error("proactive defer add failed", "bot", msg.BotID, "err", aerr)
			return
		}
		logScheduledResult(msg.BotID, msg.Sender, msg.Origin, "rescheduled", "", "", nm.RescheduleCount)
		return
	}

	if err := m.aiSink.SendHumanized(ctx, inst.Provider, msg.Sender, text, "", cfg); err != nil {
		slog.Error("proactive send failed", "bot", msg.BotID, "id", msg.ID, "err", err)
		m.store.MarkScheduledMessageFailed(msg.ID, "发送失败")
		logScheduledResult(msg.BotID, msg.Sender, msg.Origin, "failed", "send", err.Error(), msg.RescheduleCount)
		return
	}
	m.store.MarkScheduledMessageSent(msg.ID, now.Unix())
	itemList, _ := json.Marshal([]map[string]any{{"type": "text", "text": text}})
	m.store.SaveMessage(&store.Message{
		BotID: msg.BotID, Direction: "outbound", ToUserID: msg.Sender, MessageType: 2, ItemList: itemList,
	})
	logScheduledResult(msg.BotID, msg.Sender, msg.Origin, "sent", "", "", msg.RescheduleCount)
```

（`parseProactiveOutcome`/`proactiveAction`/`proactiveDeferDefaultMinutes` 定义在 manager.go；`EmptyReplyMarker`/`ProactiveDeferMarker`/`ComposeOutcome*` 由 sink 导出。）

**promise 自省**：promise 同样走上面的合成与解析——AI 回 `【不发】` 即 `proactiveDecline` → cancel（`fail_reason=reason`，如「AI 判断已实现」）；gapNote 对 promise 也在 `ComposeScheduledMessage` 的融合提示中体现（现有调用处 `gapNote` 仅 proactive 传，promise 传空串即可，最近对话上下文由 `BuildMessagesForSender` 提供）。

- [ ] **Step 4: 运行并确认通过**

Run: `go test ./internal/sink -run 'TestComposeScheduledMessage|TestComposeReschedule' -count=1 && go test ./internal/bot -run 'TestParseProactiveOutcome|TestProcessDueScheduledMessage' -count=1 && go test ./internal/sink ./internal/bot ./internal/persona ./internal/ai -count=1`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/sink/ai.go internal/sink/ai_test.go internal/bot/manager.go internal/bot/proactive_test.go
git commit -m "feat(agent): proactive composition with fusion, tools, decline and defer"
```

## Task 8: 全链路回归与验收

**Files:**
- Modify: 相关测试（如需要）

- [ ] **Step 1: 补端到端回归**

`internal/bot/proactive_test.go` 增加：完整 manager 触发链路（promise + mock AI 返回正常文本）→ 断言消息发出、状态 sent、统一日志 result=sent；再补一条「proactive 被拦 → AI 返回新时间 → 新 pending」的链路（若 Task 6 已覆盖可标注跳过）。

- [ ] **Step 2: 全量验证**

Run:

```bash
go test ./... -count=1
go build ./...
```

Expected: 全部 PASS；`go build` 仅第三方 silk C 告警。

- [ ] **Step 3: 记录验收结果**

```text
- schedule_message/cancel_schedule 结果带统一 Diagnostics，进 persona delivery 日志
- 每次触发（sent/gated/rescheduled/failed/cancelled/self-check）落统一结构化日志
- 被拦 proactive：AI 现场重定新时间，reschedule_count 防死循环（上限 3）
- promise 必发，事件已实现时 AI 可【不发】自省并留痕
- 主动合成：融合 prompt 与当前语境，可调全部 persona 工具，产物直发
- 媒体轮说明由 LLM 续轮微调，LLM 空/失败用 canned 兜底
- 热聊触发：AI 可改口 / 【不发】 / 【稍后 N 分钟】淡带一句
```

- [ ] **Step 4: 最终提交**

```bash
git add internal/bot internal/sink internal/persona internal/store
git commit -m "test(agent): cover proactive delivery end to end"
```
