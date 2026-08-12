# 控制台 S1：定时消息管理页 + 统计概览 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在控制台 bot 详情页新增「定时/主动消息」管理区块（列表 + 统计 + 取消）。

**Architecture:** 后端补 2 个接口（取消、统计），前端 bot 详情页加一个卡片区块，数据源用 stats.recent。

**Tech Stack:** Go（net/http + store）、React（Vite + shadcn/ui）、pnpm。

---

## Task 1: 后端取消接口

**Files:**
- Modify: `internal/api/character_handler.go`（新增 handler）、`internal/api/router.go`（挂路由）
- Test: `internal/api/api_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/api/api_test.go` 增加（复用既有 `TestScheduledMessages_RequireBotOwnership` 的 env 构造模式）：

```go
func TestScheduledMessages_Cancel(t *testing.T) {
	env := newTestEnv(t)
	_, ownerCookie, botID := env.createBotForUser(t)
	otherUser, otherCookie := env.createUser(t)

	msg := &store.ScheduledMessage{
		BotID: botID, Sender: "wxid_a", FireAt: time.Now().Add(60).Unix(),
		Tolerance: 600, Prompt: "x", Origin: "promise", Status: "pending",
	}
	if err := env.store.AddScheduledMessage(msg); err != nil {
		t.Fatal(err)
	}

	// 1) 非归属 → 404
	resp := doJSON(t, env.ts, http.MethodDelete, "/api/bots/"+botID+"/scheduled-messages/"+msg.ID, nil, withCookie(otherCookie))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-owned cancel status = %d, want 404", resp.StatusCode)
	}

	// 2) 成功取消 pending → 200，状态变 cancelled，reason=控制台取消
	resp = doJSON(t, env.ts, http.MethodDelete, "/api/bots/"+botID+"/scheduled-messages/"+msg.ID, nil, withCookie(ownerCookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200", resp.StatusCode)
	}
	all, _ := env.store.ListScheduledMessages(botID, 10)
	for _, m := range all {
		if m.ID == msg.ID {
			if m.Status != "cancelled" {
				t.Fatalf("status = %q, want cancelled", m.Status)
			}
			if m.FailReason != "控制台取消" {
				t.Fatalf("fail_reason = %q, want 控制台取消", m.FailReason)
			}
		}
	}

	// 3) 已取消 → 409
	resp = doJSON(t, env.ts, http.MethodDelete, "/api/bots/"+botID+"/scheduled-messages/"+msg.ID, nil, withCookie(ownerCookie))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("re-cancel status = %d, want 409", resp.StatusCode)
	}
	_ = otherUser
}
```

（先确认 `newTestEnv`/`createBotForUser`/`createUser` 的实际签名与既有测试一致，`otherUser` 变量若不需要则删掉。）

- [ ] **Step 2: 运行并确认失败**

Run: `go test ./internal/api -run TestScheduledMessages_Cancel -count=1`
Expected: FAIL（404 route not found）。

- [ ] **Step 3: 实现 handler + 路由**

`internal/api/character_handler.go` 新增：

```go
// DELETE /api/bots/{id}/scheduled-messages/{msgID} — cancel a pending scheduled message
func (s *Server) handleCancelScheduledMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msgID := r.PathValue("msgID")
	all, err := s.Store.ListScheduledMessages(id, 500)
	if err != nil {
		jsonError(w, "failed to load scheduled messages", http.StatusInternalServerError)
		return
	}
	var found *store.ScheduledMessage
	for i := range all {
		if all[i].ID == msgID {
			found = &all[i]
			break
		}
	}
	if found == nil || found.BotID != id {
		jsonError(w, "not found or not owned", http.StatusNotFound)
		return
	}
	if found.Status != "pending" {
		jsonError(w, "只能取消待发送的消息", http.StatusConflict)
		return
	}
	if err := s.Store.CancelScheduledMessageWithReason(msgID, "控制台取消"); err != nil {
		jsonError(w, "cancel failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
```

`internal/api/router.go`（protected 区，:169 附近）：

```go
	protected.HandleFunc("DELETE /api/bots/{id}/scheduled-messages/{msgID}", s.handleCancelScheduledMessage)
```

- [ ] **Step 4: 运行并确认通过**

Run: `go test ./internal/api -run TestScheduledMessages -count=1 && go test ./internal/api -count=1`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/api/character_handler.go internal/api/router.go internal/api/api_test.go
git commit -m "feat(api): cancel pending scheduled messages from console"
```

## Task 2: 后端统计接口

**Files:**
- Modify: `internal/api/character_handler.go`、`internal/api/router.go`
- Test: `internal/api/api_test.go`

- [ ] **Step 1: 写失败测试**

```go
func TestScheduledMessages_Stats(t *testing.T) {
	env := newTestEnv(t)
	_, ownerCookie, botID := env.createBotForUser(t)
	now := time.Now().Unix()
	msgs := []*store.ScheduledMessage{
		{BotID: botID, Sender: "wxid_a", FireAt: now + 60, Tolerance: 600, Prompt: "p", Origin: "promise", Status: "pending"},
		{BotID: botID, Sender: "wxid_b", FireAt: now - 60, Tolerance: 600, Prompt: "p", Origin: "proactive", Status: "sent"},
		{BotID: botID, Sender: "wxid_c", FireAt: now - 120, Tolerance: 600, Prompt: "p", Origin: "promise", Status: "failed"},
		{BotID: botID, Sender: "wxid_d", FireAt: now - 180, Tolerance: 600, Prompt: "p", Origin: "proactive", Status: "cancelled"},
	}
	for _, m := range msgs {
		if err := env.store.AddScheduledMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	resp := doJSON(t, env.ts, http.MethodGet, "/api/bots/"+botID+"/scheduled-messages/stats?days=7", nil, withCookie(ownerCookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", resp.StatusCode)
	}
	var stats struct {
		Total    int            `json:"total"`
		ByStatus map[string]int `json:"by_status"`
		ByOrigin map[string]int `json:"by_origin"`
		Recent   []json.RawMessage `json:"recent"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if stats.Total != 4 {
		t.Fatalf("total = %d, want 4", stats.Total)
	}
	if stats.ByStatus["pending"] != 1 || stats.ByStatus["sent"] != 1 || stats.ByStatus["failed"] != 1 || stats.ByStatus["cancelled"] != 1 {
		t.Fatalf("by_status = %v", stats.ByStatus)
	}
	if stats.ByOrigin["promise"] != 2 || stats.ByOrigin["proactive"] != 2 {
		t.Fatalf("by_origin = %v", stats.ByOrigin)
	}
	if len(stats.Recent) != 4 {
		t.Fatalf("recent = %d, want 4", len(stats.Recent))
	}
}
```

（注意 `AddScheduledMessage` 会重置 `CreatedAt=now`，所以 4 条都落在近 7 天窗口内。）

- [ ] **Step 2: 运行并确认失败**

Run: `go test ./internal/api -run TestScheduledMessages_Stats -count=1`
Expected: FAIL（404）。

- [ ] **Step 3: 实现 handler + 路由**

`internal/api/character_handler.go`：

```go
// GET /api/bots/{id}/scheduled-messages/stats?days=N — counts over recent N days (default 7, max 30)
func (s *Server) handleScheduledMessageStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 30 {
			days = n
		}
	}
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	all, err := s.Store.ListScheduledMessages(id, 500)
	if err != nil {
		jsonError(w, "failed to load scheduled messages", http.StatusInternalServerError)
		return
	}
	byStatus := map[string]int{}
	byOrigin := map[string]int{}
	var recent []store.ScheduledMessage
	total := 0
	for _, m := range all {
		inWindow := m.CreatedAt >= since
		if m.SentAt != nil && *m.SentAt >= since {
			inWindow = true
		}
		if !inWindow {
			continue
		}
		total++
		byStatus[m.Status]++
		byOrigin[m.Origin]++
		if len(recent) < 10 {
			recent = append(recent, m)
		}
	}
	writeJSON(w, map[string]any{
		"total":     total,
		"by_status": byStatus,
		"by_origin": byOrigin,
		"recent":    recent,
	})
}
```

路由：`protected.HandleFunc("GET /api/bots/{id}/scheduled-messages/stats", s.handleScheduledMessageStats)`（放在 `GET .../scheduled-messages` 之后）。确认 `strconv`、`time`、`json`、`store` 已 import（character_handler.go 大概率已有 time；缺则补）。

- [ ] **Step 4: 运行并确认通过**

Run: `go test ./internal/api -run TestScheduledMessages -count=1 && go test ./internal/api -count=1 && go build ./...`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/api/character_handler.go internal/api/router.go internal/api/api_test.go
git commit -m "feat(api): scheduled-message stats endpoint"
```

## Task 3: 前端 bot 详情页「定时/主动消息」区块

**Files:**
- Modify: `web/src/lib/api.ts`
- Modify: `web/src/pages/bot-detail.tsx`

- [ ] **Step 1: api.ts 加方法**

在 `web/src/lib/api.ts` 的 scheduled-messages 区（:247 附近）追加：

```ts
  listScheduledMessages: (botId: string) => request<any[]>(`/api/bots/${botId}/scheduled-messages`),
  scheduledMessageStats: (botId: string, days = 7) =>
    request<{ total: number; by_status: Record<string, number>; by_origin: Record<string, number>; recent: any[] }>(
      `/api/bots/${botId}/scheduled-messages/stats?days=${days}`,
    ),
  cancelScheduledMessage: (botId: string, msgId: string) =>
    request<{ ok: boolean }>(`/api/bots/${botId}/scheduled-messages/${msgId}`, { method: "DELETE" }),
```

- [ ] **Step 2: bot-detail.tsx 加区块组件**

在文件内新增 `ScheduledMessagesCard` 组件（`botId: string` prop），并在主组件渲染 `<ScheduledMessagesCard botId={id} />`（放在应用列表区块之后）：

要点：
- 用 `useState`/`useEffect` 调 `api.scheduledMessageStats(botId)`；loading 用 Skeleton；失败显示错误 + 重试按钮。
- 统计摘要行：`pending`/`sent`/`failed`/`cancelled` 四个计数（`by_status`），badge 展示（sent=绿、failed=红、cancelled=灰、pending=蓝）。
- 列表：Table 展示 `recent`：触发时间（`new Date(f.fire_at * 1000).toLocaleString()`）、来源 Badge（promise/proactive）、状态 Badge、原因（`f.fail_reason || "—"`）、重排次数（`f.reschedule_count ?? 0`）、发送时间（`f.sent_at` 换算）。
- 取消：pending 行显示「取消」按钮，`api.cancelScheduledMessage(botId, m.id)` 后重新拉 stats；用 `useConfirm`（文件已 import）确认。
- 空态：`recent` 为空时显示「还没有定时/主动消息」。
- 组件代码遵循文件内现有 Card/Badge/Button/Table 风格（shadcn），不引入新依赖。

- [ ] **Step 3: 构建验证**

Run: `cd web && pnpm build`
Expected: 构建成功（无 TS 报错）。若 `pnpm` 不可用，报告并尝试 `pnpm install` 后再 build；若网络受限无法 install，则用 `npx tsc --noEmit` 或报告阻塞。

- [ ] **Step 4: 提交**

```bash
git add web/src/lib/api.ts web/src/pages/bot-detail.tsx
git commit -m "feat(console): scheduled messages card with stats and cancel"
```

## Task 4: 全量验证

- [ ] **Step 1**: `go test ./... -count=1`、`go build ./...`、`cd web && pnpm build` 全部通过。
- [ ] **Step 2**: 检查 `git status` 干净；确认无遗留调试代码。
- [ ] **Step 3**: 提交剩余文件（如有）。
