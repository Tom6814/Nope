# Agent Delivery Phase One Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `web_search`、`write_doc`、`gen_image` 的结果以角色化、微信化的方式直接回到聊天里，并把失败细节写进日志。

**Architecture:** 继续复用现有 `sink.AI -> persona tool -> sink 发送` 主链路，不新增第二套投递系统。先在 `ToolCallResult` 上补齐统一交付语义和日志字段，再分别收敛搜索、文档、图片的工具输出，最后由 `internal/sink/ai.go` 统一发送结果本体和自然说明。

**Tech Stack:** Go, existing OpenAI-compatible tool-calling flow, current persona engine, existing sink/provider send path, Go testing package.

---

## File Map

- Modify: `internal/ai/chat.go`
  - 扩展 `ToolCallResult`，加入统一交付和日志元信息
- Modify: `internal/persona/search.go`
  - 把搜索结果从“来源列表摘要”改成“结论文本 + 失败元信息”
- Modify: `internal/persona/doc.go`
  - 让文档结果稳定返回 `.md` 文件和默认自然说明
- Modify: `internal/persona/image.go`
  - 让图片结果支持用户指定数量 / 模型默认判断 / 安全上限 / 默认自然说明
- Modify: `internal/persona/persona.go`
  - 记录 persona 工具执行日志，透传统一元信息
- Modify: `internal/sink/ai.go`
  - 统一发送文本、文件、图片；统一日志；统一失败解释与轻量后处理
- Modify: `internal/persona/persona_test.go`
  - 增加搜索 / 文档 / 图片结果语义测试
- Modify: `internal/sink/ai_test.go`
  - 增加工具结果统一交付测试
- Modify: `internal/sink/humanize_test.go`
  - 增加自然说明与结果本体发送顺序测试

## Task 1: 扩展统一交付结构

**Files:**
- Modify: `internal/ai/chat.go`
- Test: `internal/sink/ai_test.go`

- [ ] **Step 1: 写失败测试，锁定 ToolCallResult 的统一交付语义**

```go
func TestContinueWithToolResults_PreservesDeliveryMetadata(t *testing.T) {
	results := []ai.ToolCallResult{{
		ID:               "call_1",
		Name:             "web_search",
		Content:          "杭州今天有雨",
		UserFacing:       true,
		UserExplanation:  "我刚查了下，今天会下雨。",
		LogFields:        map[string]string{"delivery_kind": "text", "error_stage": ""},
	}}

	if !results[0].UserFacing {
		t.Fatal("expected delivery result to be marked user-facing")
	}
	if results[0].UserExplanation == "" {
		t.Fatal("expected default user explanation")
	}
	if results[0].LogFields["delivery_kind"] != "text" {
		t.Fatal("expected delivery kind metadata")
	}
}
```

- [ ] **Step 2: 运行测试并确认失败**

Run: `go test ./internal/sink -run TestContinueWithToolResults_PreservesDeliveryMetadata -count=1`

Expected: FAIL，报 `ToolCallResult` 缺少 `UserFacing`、`UserExplanation`、`LogFields` 等字段。

- [ ] **Step 3: 最小实现 `ToolCallResult` 的新字段**

```go
type ToolCallResult struct {
	ID              string
	Name            string
	Content         string
	Images          []ImageData
	Files           []FileData
	Async           bool
	UserFacing      bool
	UserExplanation string
	ErrorStage      string
	ErrorMessage    string
	LogFields       map[string]string
}
```

- [ ] **Step 4: 运行测试并确认通过**

Run: `go test ./internal/sink -run TestContinueWithToolResults_PreservesDeliveryMetadata -count=1`

Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/ai/chat.go internal/sink/ai_test.go
git commit -m "refactor(agent): extend tool delivery result metadata"
```

## Task 2: 收敛 `web_search` 为“像人查一下后的结论”

**Files:**
- Modify: `internal/persona/search.go`
- Modify: `internal/persona/persona_test.go`
- Test: `internal/persona/persona_test.go`

- [ ] **Step 1: 写失败测试，锁定搜索结果不再返回来源列表**

```go
func TestWebSearch_ReturnsConclusionStyleText(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	e.SetSearcher(fakeSearcher{
		results: []SearchResult{{
			Title:   "杭州天气",
			URL:     "https://example.com/weather",
			Snippet: "今天杭州有阵雨，最高 28 度。",
		}},
	})

	res := e.Execute(context.Background(), botID, "wxid_a", "web_search", json.RawMessage(`{"query":"杭州天气"}`))
	if strings.Contains(res.Content, "https://") {
		t.Fatal("expected no source url in conclusion-style reply")
	}
	if !res.UserFacing {
		t.Fatal("expected search result to be marked user-facing")
	}
	if res.LogFields["delivery_kind"] != "text" {
		t.Fatal("expected text delivery metadata")
	}
}
```

- [ ] **Step 2: 运行测试并确认失败**

Run: `go test ./internal/persona -run TestWebSearch_ReturnsConclusionStyleText -count=1`

Expected: FAIL，因为当前实现会把标题、摘要、URL 全部拼出来，而且没有统一交付元信息。

- [ ] **Step 3: 最小实现搜索结论文本与失败元信息**

```go
func handleWebSearch(ctx context.Context, e *Engine, botID, sender string, args map[string]any) ai.ToolCallResult {
	query := strings.TrimSpace(strArg(args, "query"))
	if query == "" {
		return ai.ToolCallResult{
			Content:      "我这边没拿到要查的内容。",
			UserFacing:   true,
			ErrorStage:   "search",
			ErrorMessage: "query required",
			LogFields:    map[string]string{"delivery_kind": "text"},
		}
	}
	results, err := e.Search.Search(ctx, query)
	if err != nil {
		return ai.ToolCallResult{
			Content:      "我这边刚刚浏览器有点抽，没顺利查出来。",
			UserFacing:   true,
			ErrorStage:   "search",
			ErrorMessage: err.Error(),
			LogFields:    map[string]string{"delivery_kind": "text"},
		}
	}
	conclusion := summarizeSearchResults(query, results)
	return ai.ToolCallResult{
		Content:         conclusion,
		UserFacing:      true,
		UserExplanation: conclusion,
		LogFields:       map[string]string{"delivery_kind": "text"},
	}
}
```

- [ ] **Step 4: 运行测试并确认通过**

Run: `go test ./internal/persona -run TestWebSearch_ReturnsConclusionStyleText -count=1`

Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/persona/search.go internal/persona/persona_test.go
git commit -m "feat(agent): return search results as human-style conclusions"
```

## Task 3: 收敛 `write_doc` 为“直接发 md 文件 + 一句自然说明”

**Files:**
- Modify: `internal/persona/doc.go`
- Modify: `internal/persona/persona_test.go`
- Test: `internal/persona/persona_test.go`

- [ ] **Step 1: 写失败测试，锁定文档返回文件与自然说明**

```go
func TestWriteDoc_ReturnsMarkdownFileAndExplanation(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	res := e.Execute(context.Background(), botID, "wxid_a", "write_doc", json.RawMessage(`{
		"title":"旅行计划",
		"content":"# 周末旅行\n\n- 带雨伞"
	}`))

	if len(res.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(res.Files))
	}
	if res.Files[0].FileName != "旅行计划.md" {
		t.Fatalf("file name = %q", res.Files[0].FileName)
	}
	if res.UserExplanation == "" {
		t.Fatal("expected a natural follow-up sentence")
	}
	if res.LogFields["delivery_kind"] != "file" {
		t.Fatal("expected file delivery metadata")
	}
}
```

- [ ] **Step 2: 运行测试并确认失败**

Run: `go test ./internal/persona -run TestWriteDoc_ReturnsMarkdownFileAndExplanation -count=1`

Expected: FAIL，因为当前实现虽然返回了文件，但没有统一的自然说明与日志字段。

- [ ] **Step 3: 最小实现文档交付结构**

```go
return ai.ToolCallResult{
	Content:         "文档已整理好。",
	UserFacing:      true,
	UserExplanation: "我整理好了，你直接看这个文档就行。",
	Files: []ai.FileData{{
		Data:        []byte(content),
		FileName:    title,
		ContentType: ct,
	}},
	LogFields: map[string]string{"delivery_kind": "file"},
}
```

- [ ] **Step 4: 运行测试并确认通过**

Run: `go test ./internal/persona -run TestWriteDoc_ReturnsMarkdownFileAndExplanation -count=1`

Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/persona/doc.go internal/persona/persona_test.go
git commit -m "feat(agent): deliver markdown docs with natural follow-up"
```

## Task 4: 收敛 `gen_image` 为“按需求回图 + 默认自然说明 + 数量上限”

**Files:**
- Modify: `internal/persona/image.go`
- Modify: `internal/persona/persona_test.go`
- Test: `internal/persona/persona_test.go`

- [ ] **Step 1: 写失败测试，锁定图片结果的数量与交付元信息**

```go
func TestGenImage_RespectsRequestedCountAndSetsExplanation(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	e.SetImageProvider(fakeImageProvider{
		images: [][]byte{[]byte("img-1"), []byte("img-2")},
	})

	res := e.Execute(context.Background(), botID, "wxid_a", "gen_image", json.RawMessage(`{
		"prompt":"雨夜街道",
		"count":2
	}`))

	if len(res.Images) != 2 {
		t.Fatalf("images = %d, want 2", len(res.Images))
	}
	if res.UserExplanation == "" {
		t.Fatal("expected a natural follow-up sentence")
	}
	if res.LogFields["delivery_kind"] != "image" {
		t.Fatal("expected image delivery metadata")
	}
}
```

- [ ] **Step 2: 运行测试并确认失败**

Run: `go test ./internal/persona -run TestGenImage_RespectsRequestedCountAndSetsExplanation -count=1`

Expected: FAIL，因为当前 provider 只生成 1 张，且没有数量语义、说明文本和统一日志字段。

- [ ] **Step 3: 最小实现图片数量解析与安全上限**

```go
const maxGeneratedImages = 4

func resolveImageCount(args map[string]any) int {
	n := intArg(args, "count")
	if n <= 0 {
		return 1
	}
	if n > maxGeneratedImages {
		return maxGeneratedImages
	}
	return n
}

func handleGenImage(ctx context.Context, e *Engine, botID, sender string, args map[string]any) ai.ToolCallResult {
	count := resolveImageCount(args)
	images, err := generateImages(ctx, e.imageProvider(), prompt, negativePrompt, opts, count)
	if err != nil {
		return ai.ToolCallResult{
			Content:      "我这边刚刚出图没跑顺。",
			UserFacing:   true,
			ErrorStage:   "generate",
			ErrorMessage: err.Error(),
			LogFields:    map[string]string{"delivery_kind": "image"},
		}
	}
	return ai.ToolCallResult{
		Content:         "图给你发过去了。",
		UserFacing:      true,
		UserExplanation: "我先把图发你，你看看是不是这个感觉。",
		Images:          images,
		LogFields:       map[string]string{"delivery_kind": "image"},
	}
}
```

- [ ] **Step 4: 运行测试并确认通过**

Run: `go test ./internal/persona -run TestGenImage_RespectsRequestedCountAndSetsExplanation -count=1`

Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/persona/image.go internal/persona/persona_test.go
git commit -m "feat(agent): deliver generated images with count support"
```

## Task 5: 统一发送层与日志

**Files:**
- Modify: `internal/sink/ai.go`
- Modify: `internal/sink/ai_test.go`
- Modify: `internal/sink/humanize_test.go`
- Test: `internal/sink/ai_test.go`
- Test: `internal/sink/humanize_test.go`

- [ ] **Step 1: 写失败测试，锁定“结果本体先发，再补一句自然说明”**

```go
func TestReply_SendsFileThenExplanation(t *testing.T) {
	p := &recordingProvider{}
	s := &AI{}
	d := Delivery{
		BotDBID:  "bot1",
		Provider: p,
		Message:  provider.InboundMessage{Sender: "wxid_a", ContextToken: "ctx-1"},
	}

	s.deliverToolProducts(context.Background(), d, []ai.ToolCallResult{{
		Name:            "write_doc",
		UserFacing:      true,
		UserExplanation: "我整理好了，你直接看这个就行。",
		Files: []ai.FileData{{
			FileName:    "旅行计划.md",
			ContentType: "text/markdown",
			Data:        []byte("# hi"),
		}},
		LogFields: map[string]string{"delivery_kind": "file"},
	}})

	texts := p.texts()
	if len(texts) != 1 || texts[0] != "我整理好了，你直接看这个就行。" {
		t.Fatalf("unexpected follow-up texts: %#v", texts)
	}
}
```

- [ ] **Step 2: 运行测试并确认失败**

Run: `go test ./internal/sink -run TestReply_SendsFileThenExplanation -count=1`

Expected: FAIL，因为当前文件直发逻辑和自然说明逻辑是分散的，没有统一 `deliverToolProducts` 之类的交付入口。

- [ ] **Step 3: 最小实现统一发送与日志**

```go
func (s *AI) deliverToolProducts(ctx context.Context, d Delivery, results []ai.ToolCallResult) {
	for _, tr := range results {
		if len(tr.Files) > 0 {
			s.sendFilesToUser(ctx, d, tr.Files)
		}
		if len(tr.Images) > 0 {
			s.sendImagesToUser(ctx, d, tr.Images)
		}
		if tr.UserFacing && strings.TrimSpace(tr.UserExplanation) != "" {
			_ = s.SendHumanized(ctx, d.Provider, d.Message.Sender, tr.UserExplanation, d.Message.ContextToken, s.resolveGlobalConfig())
		}
		slog.Info("persona delivery",
			"tool_name", tr.Name,
			"bot_id", d.BotDBID,
			"sender", d.Message.Sender,
			"delivery_kind", tr.LogFields["delivery_kind"],
			"success", tr.ErrorMessage == "",
			"error_stage", tr.ErrorStage,
			"error_message", tr.ErrorMessage,
			"explained_to_user", tr.UserFacing && tr.UserExplanation != "",
		)
	}
}
```

- [ ] **Step 4: 跑受影响测试并确认通过**

Run:

```bash
go test ./internal/sink -run 'TestReply_SendsFileThenExplanation|TestSendHumanized_PreservesContextToken' -count=1
go test ./internal/persona -run 'TestWebSearch_ReturnsConclusionStyleText|TestWriteDoc_ReturnsMarkdownFileAndExplanation|TestGenImage_RespectsRequestedCountAndSetsExplanation' -count=1
```

Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/sink/ai.go internal/sink/ai_test.go internal/sink/humanize_test.go
git commit -m "feat(agent): unify tool delivery and logging flow"
```

## Task 6: 全链路回归

**Files:**
- Modify: `internal/sink/ai_test.go`
- Modify: `internal/persona/persona_test.go`

- [ ] **Step 1: 写失败测试，锁定搜索 / 文档 / 图片三条链路的一致性**

```go
func TestToolDelivery_UsesConsistentUserFacingFlow(t *testing.T) {
	results := []ai.ToolCallResult{
		{Name: "web_search", UserFacing: true, UserExplanation: "我刚查了下，大概是这样。", LogFields: map[string]string{"delivery_kind": "text"}},
		{Name: "write_doc", UserFacing: true, UserExplanation: "我整理好了，你直接看这个就行。", Files: []ai.FileData{{FileName: "a.md", Data: []byte("x")}}, LogFields: map[string]string{"delivery_kind": "file"}},
		{Name: "gen_image", UserFacing: true, UserExplanation: "图我先发你。", Images: []ai.ImageData{{Data: []byte("png"), ContentType: "image/png"}}, LogFields: map[string]string{"delivery_kind": "image"}},
	}
	if len(results) != 3 {
		t.Fatal("expected three delivery samples")
	}
}
```

- [ ] **Step 2: 运行测试并确认失败**

Run: `go test ./internal/sink -run TestToolDelivery_UsesConsistentUserFacingFlow -count=1`

Expected: FAIL，直到三条链路都统一到相同的交付约定。

- [ ] **Step 3: 运行完整验证并修正最后的接口不一致**

Run:

```bash
go test ./internal/persona ./internal/sink ./internal/ai -count=1
go test ./... -count=1
go build ./...
```

Expected: 全部 PASS；如有字段名、解释语义、发送顺序不一致，统一收敛到前面任务定义。

- [ ] **Step 4: 最终提交**

```bash
git add internal/persona internal/sink internal/ai
git commit -m "test(agent): cover phase-one delivery flow end to end"
```

- [ ] **Step 5: 记录验收结果**

```text
- web_search 直接回结论，不列来源
- write_doc 直接回 .md 文件，再补一句自然说明
- gen_image 直接回图片，再补一句自然说明
- 失败前台自然解释，后台日志可查
```
