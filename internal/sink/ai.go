package sink

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/openilink/openilink-hub/internal/ai"
	appdelivery "github.com/openilink/openilink-hub/internal/app"
	"github.com/openilink/openilink-hub/internal/persona"
	"github.com/openilink/openilink-hub/internal/provider"
	"github.com/openilink/openilink-hub/internal/storage"
	"github.com/openilink/openilink-hub/internal/store"
)

const typingTimeout = 30 * time.Second
const maxImageBytes = 20 * 1024 * 1024 // 20MB

// BotModelSyncer allows the AI sink to sync an in-memory bot's model after a
// /model switch without importing the bot package (which would create a cycle).
type BotModelSyncer interface {
	SetBotAIModel(botDBID, model string)
}

// AI calls an OpenAI-compatible chat completion API and sends the reply
// back through the bot. Supports tool calling via installed App tools.
type AI struct {
	Store      store.Store
	AppDisp    *appdelivery.Dispatcher
	Storage    storage.Store
	BotManager BotModelSyncer
	Persona    *persona.Engine // optional persona capability engine
}

func (s *AI) Name() string { return "ai" }

func (s *AI) Handle(d Delivery) {
	if !d.AIEnabled {
		return
	}
	if d.MsgType != "text" && d.MsgType != "image" {
		return
	}
	if d.MsgType == "text" && d.Content == "" {
		return
	}
	// Skip messages targeted at specific apps (commands and @mentions).
	// For image messages, d.Content may be a placeholder; the real caption
	// is checked after extraction in reply().
	if d.MsgType == "text" {
		trimmed := strings.TrimSpace(d.Content)
		if strings.HasPrefix(trimmed, "@") {
			return
		}
		if strings.HasPrefix(trimmed, "/") {
			s.handleCommand(d, trimmed)
			return
		}
	}
	s.reply(d)
}

func (s *AI) handleCommand(d Delivery, cmd string) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 || parts[0] != "/model" {
		return
	}

	global, _ := s.Store.ListConfigByPrefix("ai.")
	availableRaw := global["ai.available_models"]
	var available []string
	if availableRaw != "" {
		if err := json.Unmarshal([]byte(availableRaw), &available); err != nil {
			slog.Warn("ai: malformed ai.available_models config", "err", err)
		}
	}

	sendText := func(text string) {
		d.Provider.Send(context.Background(), provider.OutboundMessage{
			Recipient:    d.Message.Sender,
			Text:         text,
			ContextToken: d.Message.ContextToken,
		})
	}

	if len(parts) == 1 {
		// List models
		if len(available) == 0 {
			sendText("没有可用的模型列表，请联系管理员配置。")
			return
		}
		current := d.AIModel
		if current == "" {
			current = global["ai.model"]
		}
		var lines []string
		for _, m := range available {
			mark := "  "
			if m == current {
				mark = "✓ "
			}
			lines = append(lines, mark+m)
		}
		sendText("可用模型：\n" + strings.Join(lines, "\n"))
		return
	}

	// Switch model
	requested := parts[1]
	valid := false
	for _, m := range available {
		if m == requested {
			valid = true
			break
		}
	}
	if !valid {
		sendText("模型不在可用列表中，请用 /model 查看可用模型。")
		return
	}

	if err := s.Store.UpdateBotAIModel(d.BotDBID, requested); err != nil {
		sendText("切换失败，请稍后重试。")
		return
	}
	if s.BotManager != nil {
		s.BotManager.SetBotAIModel(d.BotDBID, requested)
	}
	sendText("已切换到模型：" + requested)
}

func (s *AI) resolveConfig(botModel string) store.AIConfig {
	cfg := s.resolveGlobalConfig()
	if botModel != "" {
		cfg.Model = botModel
	}
	return cfg
}

// ProactiveDeferMarker：AI 表示想等几分钟再发。
const ProactiveDeferMarker = "【稍后】"

// composeOutcome 描述主动合成结果。
type composeOutcome int

// ComposeOutcome* 导出给 manager 判断结果类型。
const (
	ComposeOutcomeText      composeOutcome = iota // Text 为最终要发的文本
	ComposeOutcomeDelivered                       // 产物+说明已直发（无需再发文本）
	ComposeOutcomeEmpty                           // AI 放弃（无文本、无直发）
)

// ComposeScheduledMessage 到点合成主动消息：融合存储 prompt 与当前语境，
// 带 persona 工具集（可发文本/文件/图），返回最终文本与结果类型。
// p 是发送所需的 provider（产物/说明直发要用）。
func (s *AI) ComposeScheduledMessage(ctx context.Context, botID, sender, prompt, gapNote string, p provider.Provider) (string, composeOutcome, error) {
	cfg := s.resolveConfig("")
	if cfg.APIKey == "" {
		return "", ComposeOutcomeEmpty, fmt.Errorf("ai not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	userText := prompt + "\n\n（这是一条到点的主动联系。"
	if gapNote != "" {
		userText += "\n" + gapNote + "。"
	}
	userText += "先结合你们最近的对话把这条提示融成此刻合适的话再回。若觉得现在不适合发，可回" + EmptyReplyMarker + "；若想等几分钟再发，可回" + ProactiveDeferMarker + "（可写分钟数，如" + ProactiveDeferMarker + " 10 分钟）。）"
	messages := ai.BuildMessagesForSender(ctx, cfg, s.Store, botID, sender, userText)
	tools := s.collectTools(botID)
	if len(tools) == 0 && s.Persona != nil {
		// AppDisp 未挂载（如单测）时 collectTools 提前返回 nil；
		// 退回 persona 工具集，保证主动合成至少带上人设工具。
		tools = s.Persona.Tools()
	}
	d := Delivery{BotDBID: botID, Provider: p, Message: provider.InboundMessage{Sender: sender}}
	res, err := s.runAIRound(ctx, d, cfg, messages, tools, nil, nil, nil)
	if err != nil {
		// runAIRound 在"产物已直发 + 全部 canned 兜底说明已补发"的续轮失败时
		// 返回 (roundOutcomeDelivered, err)。此时用户已收到内容，必须如实上报
		// Delivered，否则 manager 会把已送达的消息标记为失败（Task 7）。
		if res.Outcome == roundOutcomeDelivered {
			return "", ComposeOutcomeDelivered, err
		}
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

// rescheduleTimeRe matches the first datetime-looking substring in a chatty
// AI reply (YYYY-MM-DD[ T]HH:MM). persona.ParseTime requires the whole string
// to match, so the substring must be extracted first (Task 6).
var rescheduleTimeRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}`)

// ComposeReschedule 在被防打扰闸门拦截后，请 AI 现场重定一个更合适的时间。
// 返回新的 fire_at（Unix 秒）；AI 放弃时 ok=false。
func (s *AI) ComposeReschedule(ctx context.Context, botID, sender, prompt, reason string) (int64, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
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
	// AI replies are often chatty ("那就明天上午10点吧 2026-08-13 10:00") —
	// persona.ParseTime needs the whole string to match, so extract the first
	// datetime substring first and fall back to the full content (exact match).
	parseTarget := content
	if m := rescheduleTimeRe.FindString(content); m != "" {
		parseTarget = m
	}
	t, err := persona.ParseTime(parseTarget)
	if err != nil {
		return 0, false, nil // 无法解析视为放弃，不产生错误日志噪音
	}
	return t.Unix(), true, nil
}

// EmptyReplyMarker is the sentinel the AI may reply with to decline a
// proactive message (design §8.2 second opinion / §11.3 refusal right).
const EmptyReplyMarker = "【不发】"

func (s *AI) reply(d Delivery) {
	cfg := s.resolveConfig(d.AIModel)
	if cfg.APIKey == "" {
		slog.Warn("ai reply skipped: no api key", "bot", d.BotDBID)
		return
	}

	// Start trace span
	var span *store.SpanBuilder
	if d.Tracer != nil && d.RootSpan != nil {
		span = d.Tracer.StartChild(d.RootSpan, "ai_completion", store.SpanKindClient, map[string]any{
			"ai.model":  cfg.Model,
			"ai.source": cfg.Source,
			"reply.to":  d.Message.Sender,
		})
	}

	ctx := context.Background()
	sender := d.Message.Sender

	// Typing indicator
	var typingTicket string
	if d.Message.ContextToken != "" {
		if bcfg, err := d.Provider.GetConfig(ctx, sender, d.Message.ContextToken); err == nil && bcfg.TypingTicket != "" {
			typingTicket = bcfg.TypingTicket
			d.Provider.SendTyping(ctx, sender, typingTicket, true)
			go func() {
				time.Sleep(typingTimeout)
				d.Provider.SendTyping(context.Background(), sender, typingTicket, false)
			}()
		}
	}

	// Collect tools from installed apps
	tools := s.collectTools(d.BotDBID)
	if span != nil && len(tools) > 0 {
		span.SetAttr("ai.tools_count", len(tools))
	}

	// Download images from current message if it's an image type
	var currentImages []ai.ImageData
	text := d.Content
	if d.MsgType == "image" {
		text = "" // extract real text from items, not the "[image]" placeholder
		for _, item := range d.Message.Items {
			if item.Type == "text" && item.Text != "" {
				text = item.Text
			}
			if item.Type == "image" && item.Media != nil && item.Media.EncryptQueryParam != "" {
				data, err := d.Provider.DownloadMedia(ctx, item.Media)
				if err != nil {
					slog.Warn("ai: download image failed", "bot", d.BotDBID, "err", err)
					continue
				}
				if len(data) == 0 {
					continue
				}
				if len(data) > maxImageBytes {
					slog.Warn("ai: image too large, skipping", "bot", d.BotDBID, "size", len(data))
					continue
				}
				currentImages = append(currentImages, ai.ImageData{
					Data:        data,
					ContentType: http.DetectContentType(data),
				})
			}
		}
		if len(currentImages) == 0 && text == "" {
			return
		}
		// Check extracted caption for command/@mention prefixes
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "@") {
			return
		}
	}

	// Create media resolver for history images
	var resolver ai.MediaResolver
	if s.Storage != nil {
		resolver = func(ctx context.Context, key string) ([]byte, error) {
			return s.Storage.Get(ctx, key)
		}
	}

	// Build messages for conversation context (reused across tool-call rounds)
	messages := ai.BuildMessagesPersona(ctx, cfg, s.Store, d.BotDBID, d.Channel.ID, sender, text, currentImages, resolver)

	var usage usageTally
	notify := func(status string) {
		d.Provider.Send(ctx, provider.OutboundMessage{Recipient: sender, Text: status})
	}
	res, err := s.runAIRound(ctx, d, cfg, messages, tools, notify, span, &usage)
	s.setTokenUsage(span, d.RootSpan, usage.Prompt, usage.Completion, usage.Total, usage.Cached, usage.Reasoning)
	if err != nil {
		slog.Error("ai completion failed", "bot", d.BotDBID, "err", err)
		if span != nil {
			span.SetStatus(store.StatusError, err.Error())
			span.End()
		}
		s.stopTyping(d, typingTicket)
		// 兜底已交付（roundOutcomeDelivered）时用户已收到产物 + canned 说明，
		// 不再补发失败提示，避免二次打扰（Task 5 issue 2）。
		if res.Outcome != roundOutcomeDelivered {
			s.sendErrorNotice(d, sender)
		}
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

	s.stopTyping(d, typingTicket)

	// §11.1 (design): thinking/reasoning is NEVER externalized, regardless of
	// any config. It stays only as a trace attribute for debugging.
	if thinking != "" && span != nil {
		span.SetAttr("ai.thinking_length", len(thinking))
	}

	// StripMarkdown runs on the reply only (thinking is never sent anyway).
	if cfg.StripMarkdown {
		reply = ai.StripMarkdown(reply)
	}

	if reply == "" {
		if span != nil {
			span.SetAttr("reply.content", "(empty)")
			span.End()
		}
		return
	}

	if span != nil {
		span.SetAttr("reply.content", reply)
	}

	// §10.2 (design): humanized split-send — multi-line replies are delivered
	// as short WeChat-style bursts with pauses, unless humanize_wechat is off.
	if err := s.SendHumanized(ctx, d.Provider, sender, reply, d.Message.ContextToken, cfg); err != nil {
		slog.Error("ai reply send failed", "bot", d.BotDBID, "err", err)
		if span != nil {
			span.SetStatus(store.StatusError, "send failed: "+err.Error())
			span.End()
		}
		return
	}

	if span != nil {
		span.End()
	}

	// Save only the content (not thinking) to message history to avoid polluting context
	itemList, _ := json.Marshal([]map[string]any{{"type": "text", "text": reply}})
	s.Store.SaveMessage(&store.Message{
		BotID:       d.BotDBID,
		Direction:   "outbound",
		ToUserID:    sender,
		MessageType: 2,
		ItemList:    itemList,
	})
}

// roundOutcome 描述一轮 AI 回合的最终状态。
type roundOutcome int

const (
	roundOutcomeText      roundOutcome = iota // Text 里有最终要发给用户的文本
	roundOutcomeDelivered                     // 产物+说明已直发，无需再发文本
	roundOutcomeEmpty                         // AI 未产出文本且无直发（视为放弃）
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
func (s *AI) runAIRound(ctx context.Context, d Delivery, cfg store.AIConfig, messages []ai.Message, tools []ai.Tool, notify func(string), parentSpan *store.SpanBuilder, usage *usageTally) (roundResult, error) {
	result, err := ai.CompleteMessages(ctx, cfg, messages, tools)
	if err != nil {
		return roundResult{Outcome: roundOutcomeEmpty}, err
	}
	if usage != nil {
		accumulateUsage(usage, result.Usage)
	}

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

	var pending []string
	// cumulativeAllDelivered 跨轮累计：只要有任何一轮的产品（媒体/文件/直发说明）
	// 未送达用户，续轮失败时就不算"已交付"，让 reply() 补发失败提示。
	cumulativeAllDelivered := true
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

		allDelivered, newPending := s.deliverToolProducts(ctx, d, toolResults)
		cumulativeAllDelivered = cumulativeAllDelivered && allDelivered
		pending = append(pending, newPending...)
		if s.shouldSkipToolLLM(toolResults) && cumulativeAllDelivered {
			// 纯异步轮和纯文本轮 pending 为空；跨轮累计的媒体轮 canned 说明
			// 必须在提前返回前补发，否则会被静默丢弃（Task 5 issue 1）。
			s.flushPendingExplanations(ctx, d, pending)
			return roundResult{Outcome: roundOutcomeDelivered}, nil
		}

		llmResults := toolResultsForContinuation(toolResults)
		result, messages, err = ai.ContinueWithToolResults(ctx, cfg, messages, llmResults, tools)
		if err != nil {
			// 续轮失败：仅当用户确实收到了产品 + 全部 canned 兜底说明时才按已交付
			// 上报，避免 reply() 补发误导性提示（Task 5 issue 2）；否则返回 Empty，
			// 由 reply() 补发失败提示。len(pending)==0 时必不可能是 Delivered。
			flushed := s.flushPendingExplanations(ctx, d, pending)
			if cumulativeAllDelivered && len(pending) > 0 && flushed == len(pending) {
				return roundResult{Outcome: roundOutcomeDelivered}, err
			}
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

// SendHumanized delivers a reply the way a real person sends WeChat messages:
// split by newlines into short bursts with small pauses between them
// (design §10.2). Splitting only affects the delivery presentation — the
// history is still saved as one full reply. Caps keep bursts from stalling:
// at most maxHumanizeParts parts, each pause at most 1.5s and the total
// pause budget at most 6s (design §10.3). It is shared by the reply path and
// the proactive message ticker.
const (
	maxHumanizeParts     = 6
	maxHumanizePauseSecs = 6.0
)

func (s *AI) SendHumanized(ctx context.Context, p provider.Provider, recipient, reply, contextToken string, cfg store.AIConfig) error {
	if !cfg.HumanizeWechat {
		_, err := p.Send(ctx, provider.OutboundMessage{Recipient: recipient, Text: reply, ContextToken: contextToken})
		return err
	}

	lines := splitReplyLines(reply)
	if len(lines) == 1 {
		_, err := p.Send(ctx, provider.OutboundMessage{Recipient: recipient, Text: lines[0], ContextToken: contextToken})
		return err
	}
	// Merge excessive parts so the burst never stalls the conversation.
	if len(lines) > maxHumanizeParts {
		merged := append([]string{}, lines[:maxHumanizeParts-1]...)
		merged = append(merged, strings.Join(lines[maxHumanizeParts-1:], "\n"))
		lines = merged
	}
	totalPause := 0.0
	for i, line := range lines {
		if i > 0 {
			pause := 0.8
			if totalPause+pause > maxHumanizePauseSecs {
				pause = maxHumanizePauseSecs - totalPause
			}
			if pause > 0 {
				totalPause += pause
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(pause * float64(time.Second))):
				}
			}
		}
		if _, err := p.Send(ctx, provider.OutboundMessage{Recipient: recipient, Text: line, ContextToken: contextToken}); err != nil {
			return err
		}
	}
	return nil
}

// splitReplyLines splits a reply into burst lines (one send per non-empty
// line, per design §10.1: short sentences sent separately).
func splitReplyLines(reply string) []string {
	raw := strings.Split(reply, "\n")
	var out []string
	for _, l := range raw {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// collectTools gathers all tools from enabled app installations on this bot.
func (s *AI) collectTools(botID string) []ai.Tool {
	if s.AppDisp == nil {
		return nil
	}
	installations, err := s.Store.ListInstallationsByBot(botID)
	if err != nil {
		slog.Error("ai: list installations failed", "bot", botID, "err", err)
		return nil
	}

	var tools []ai.Tool
	for _, inst := range installations {
		if !inst.Enabled {
			continue
		}
		app, err := s.Store.GetApp(inst.AppID)
		if err != nil {
			continue
		}
		var appTools []store.AppTool
		json.Unmarshal(app.Tools, &appTools)
		for _, t := range appTools {
			if t.Name == "" {
				continue
			}
			params := t.Parameters
			params = ensureObjectSchema(params)
			// Use installation ID as prefix for unique routing
			tools = append(tools, ai.Tool{
				Type: "function",
				Function: ai.ToolFunction{
					Name:        inst.ID + "__" + t.Name,
					Description: fmt.Sprintf("[%s] %s", inst.AppName, t.Description),
					Parameters:  params,
				},
			})
		}
	}
	// Persona tools are always available to the AI (in-process engine).
	if s.Persona != nil {
		tools = append(tools, s.Persona.Tools()...)
	}
	return tools
}

// ensureObjectSchema normalises a tool parameters value into a valid
// OpenAI-compatible JSON Schema ("type":"object").  It handles:
//   - empty / literal "null"  → default empty-object schema
//   - bare properties map (no "type"/"properties" keys) → wrapped
//   - per-property "required":true → hoisted to top-level "required" array
//   - already well-formed → cleaned of per-property "required" if present
func ensureObjectSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}

	// Determine whether this is already a schema (has "type") or a bare
	// properties map (keys are property names like "text", "size").
	_, hasType := m["type"]

	var propsRaw map[string]json.RawMessage
	if hasType {
		// Already a schema – extract properties to clean them
		if p, ok := m["properties"]; ok {
			json.Unmarshal(p, &propsRaw)
		}
	} else {
		// Bare properties map – the top-level keys are property names
		propsRaw = m
	}

	// Hoist per-property "required":true to a top-level array and strip it.
	var required []string
	cleanedProps := make(map[string]any, len(propsRaw))
	for name, propRaw := range propsRaw {
		var prop map[string]any
		if err := json.Unmarshal(propRaw, &prop); err != nil {
			cleanedProps[name] = propRaw
			continue
		}
		if req, ok := prop["required"]; ok {
			if b, isBool := req.(bool); isBool && b {
				required = append(required, name)
			}
			delete(prop, "required")
		}
		cleanedProps[name] = prop
	}

	schema := map[string]any{
		"type":       "object",
		"properties": cleanedProps,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	out, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return out
}

// executeToolCall delivers a tool call to the corresponding app and returns the result.
func (s *AI) executeToolCall(ctx context.Context, d Delivery, tc ai.ToolCallRequest, parentSpan *store.SpanBuilder) ai.ToolCallResult {
	// Persona tools run in-process with the delivery context (bot, sender)
	// filled in by the engine — the AI never supplies these itself.
	name := tc.Name
	if s.Persona != nil && s.Persona.IsPersonaTool(name) {
		var span *store.SpanBuilder
		if d.Tracer != nil && parentSpan != nil {
			span = d.Tracer.StartChild(parentSpan, "tool_call:"+name, store.SpanKindClient, map[string]any{
				"tool.name": name,
				"tool.args": string(tc.Arguments),
			})
		}
		result := s.Persona.Execute(ctx, d.BotDBID, d.Message.Sender, name, tc.Arguments)
		result.ID = tc.ID
		if result.Name == "" {
			result.Name = name
		}
		if span != nil {
			span.SetAttr("tool.result", truncateStr(result.Content, 500))
			span.End()
		}
		return result
	}

	// Parse "installationID__tool_name" format
	instID := ""
	toolName := name
	if idx := strings.Index(name, "__"); idx >= 0 {
		instID = name[:idx]
		toolName = name[idx+2:]
	}

	// Create child span for this tool call
	var span *store.SpanBuilder
	if d.Tracer != nil && parentSpan != nil {
		span = d.Tracer.StartChild(parentSpan, "tool_call:"+toolName, store.SpanKindClient, map[string]any{
			"tool.name": toolName,
			"tool.args": string(tc.Arguments),
		})
	}

	// Parse arguments
	var args map[string]any
	json.Unmarshal(tc.Arguments, &args)

	// Find the installation by ID
	installation, err := s.Store.GetInstallation(instID)
	if err != nil || installation == nil || !installation.Enabled || installation.BotID != d.BotDBID {
		errMsg := fmt.Sprintf("tool %q not found", toolName)
		slog.Warn("ai tool call: installation not found", "bot", d.BotDBID, "inst", instID, "tool", toolName)
		if span != nil {
			span.EndWithError(errMsg)
		}
		return ai.ToolCallResult{ID: tc.ID, Name: tc.Name, Content: errMsg}
	}

	if span != nil {
		span.SetAttr("app.name", installation.AppName)
	}

	// Build event (same format as command events).
	// sender is the real user; sender.role indicates AI Agent initiated the call.
	senderInfo := map[string]any{"id": d.Message.Sender, "role": "agent"}
	var groupInfo any
	if d.Message.GroupID != "" {
		groupInfo = map[string]any{"id": d.Message.GroupID}
	}
	event := appdelivery.NewEvent("command", map[string]any{
		"command": toolName,
		"text":    "",
		"args":    args,
		"sender":  senderInfo,
		"group":   groupInfo,
	})
	if d.Tracer != nil {
		event.TraceID = d.Tracer.TraceID()
	}

	// Deliver to app
	result := s.AppDisp.DeliverWithRetry(installation, event)

	if result == nil {
		if span != nil {
			span.EndWithError("no response")
		}
		return ai.ToolCallResult{ID: tc.ID, Name: tc.Name, Content: "tool returned no response"}
	}

	if span != nil {
		span.SetAttr("http.status_code", result.StatusCode)
		span.SetAttr("tool.result", truncateStr(result.Reply, 500))
	}

	// Handle async replies: app will push the result later via Bot API.
	if result.ReplyAsync {
		if span != nil {
			span.SetAttr("tool.reply_async", true)
			span.End()
		}
		return ai.ToolCallResult{ID: tc.ID, Name: tc.Name, Content: "result pending, will be delivered asynchronously", Async: true}
	}

	// Handle image replies: send image to user directly. App image rounds still
	// continue with an LLM continuation round — only fully-delivered persona
	// rounds skip it (design §6.3).
	if result.ReplyType == "image" {
		images := s.resolveToolMedia(ctx, d.BotDBID, result)
		// Only include images that were actually delivered to the user.
		delivered := s.sendMediaToUser(ctx, d, images)
		if span != nil {
			span.SetAttr("tool.reply_type", result.ReplyType)
			span.End()
		}
		content := result.Reply
		if content == "" && len(delivered) == 0 {
			content = fmt.Sprintf("tool returned HTTP %d with no content", result.StatusCode)
		}
		return ai.ToolCallResult{ID: tc.ID, Name: tc.Name, Content: content, Images: delivered}
	}

	if span != nil {
		span.End()
	}

	content := result.Reply
	if content == "" {
		content = fmt.Sprintf("tool returned HTTP %d with no content", result.StatusCode)
	}
	return ai.ToolCallResult{ID: tc.ID, Name: tc.Name, Content: content}
}

// sendMediaToUser sends resolved images directly to the user via the provider.
// Returns only images that were successfully sent.
func (s *AI) sendMediaToUser(ctx context.Context, d Delivery, images []ai.ImageData) []ai.ImageData {
	sender := d.Message.Sender
	var delivered []ai.ImageData
	for _, img := range images {
		ct := img.ContentType
		fileName := "image.jpg"
		if strings.HasPrefix(ct, "image/png") {
			fileName = "image.png"
		} else if strings.HasPrefix(ct, "image/gif") {
			fileName = "image.gif"
		} else if strings.HasPrefix(ct, "image/webp") {
			fileName = "image.webp"
		}
		_, err := d.Provider.Send(ctx, provider.OutboundMessage{
			Recipient: sender, Data: img.Data, FileName: fileName,
		})
		if err != nil {
			slog.Error("ai tool media: send to user failed", "bot", d.BotDBID, "err", err)
			continue
		}
		delivered = append(delivered, img)
		itemList, _ := json.Marshal([]map[string]any{{"type": "image", "file_name": fileName}})
		mediaStatus := ""
		mediaKeys := json.RawMessage(`{}`)
		if s.Storage != nil {
			ext := ".jpg"
			if strings.HasPrefix(ct, "image/png") {
				ext = ".png"
			} else if strings.HasPrefix(ct, "image/gif") {
				ext = ".gif"
			} else if strings.HasPrefix(ct, "image/webp") {
				ext = ".webp"
			}
			now := time.Now()
			key := fmt.Sprintf("%s/%s/ai_%d%s", d.BotDBID, now.Format("2006/01/02"), now.UnixMilli(), ext)
			if _, err := s.Storage.Put(ctx, key, ct, img.Data); err == nil {
				mediaStatus = "ready"
				mediaKeys, _ = json.Marshal(map[string]string{"0": key})
			}
		}
		s.Store.SaveMessage(&store.Message{
			BotID: d.BotDBID, Direction: "outbound", ToUserID: sender, MessageType: 2,
			ItemList: itemList, MediaStatus: mediaStatus, MediaKeys: mediaKeys,
		})
	}
	return delivered
}

// sendFilesToUser delivers file products (e.g. write_doc) to the user and
// records them as outbound messages. Returns the number of files
// successfully sent.
func (s *AI) sendFilesToUser(ctx context.Context, d Delivery, files []ai.FileData) int {
	sender := d.Message.Sender
	sent := 0
	for _, f := range files {
		if len(f.Data) == 0 {
			continue
		}
		_, err := d.Provider.Send(ctx, provider.OutboundMessage{
			Recipient: sender, Data: f.Data, FileName: f.FileName,
		})
		if err != nil {
			slog.Error("ai tool file: send to user failed", "bot", d.BotDBID, "err", err)
			continue
		}
		sent++
		itemList, _ := json.Marshal([]map[string]any{{"type": "file", "file_name": f.FileName}})
		s.Store.SaveMessage(&store.Message{
			BotID: d.BotDBID, Direction: "outbound", ToUserID: sender, MessageType: 2,
			ItemList: itemList,
		})
	}
	return sent
}

// deliverToolProducts is the unified tool delivery layer (design §8, §11.2):
// for persona tool results it delivers the product body first (files, then
// images) and emits one delivery log line with the tool diagnostics.
// Text-only rounds follow with the natural companion explanation immediately
// (SendHumanized + history). Media rounds defer the canned explanation: the
// product goes out now and the explanation is returned via
// pendingExplanations so the LLM continuation round can produce a
// context-refined one, with the canned sentence as the fallback (design §6.3).
// App (non-persona) results are never re-delivered here — executeToolCall
// already sent their image replies via sendMediaToUser.
// It reports whether every persona result that required delivery reached the
// user: product media (files/images) that failed to send and immediate
// explanation sends that failed both make it return false; results that need
// no explanation don't affect the result. On failure the caller falls back to
// the LLM continuation round so the user still gets a reply.
func (s *AI) deliverToolProducts(ctx context.Context, d Delivery, results []ai.ToolCallResult) (allDelivered bool, pendingExplanations []string) {
	allDelivered = true
	for _, tr := range results {
		if s.Persona == nil || !s.Persona.IsPersonaTool(tr.Name) {
			continue
		}
		if len(tr.Files) > 0 {
			if sent := s.sendFilesToUser(ctx, d, tr.Files); sent < len(tr.Files) {
				allDelivered = false
			}
		}
		if len(tr.Images) > 0 {
			delivered := s.sendMediaToUser(ctx, d, tr.Images)
			if len(delivered) < len(tr.Images) {
				allDelivered = false
			}
		}
		explained := false
		textOnly := len(tr.Files) == 0 && len(tr.Images) == 0
		if tr.Delivery.UserFacing && strings.TrimSpace(tr.Delivery.Explanation) != "" {
			if textOnly {
				// Text-only rounds deliver the explanation immediately and
				// persist it to message history so the conversation context
				// matches what the user actually saw (mirrors the final-reply
				// save path).
				err := s.SendHumanized(ctx, d.Provider, d.Message.Sender, tr.Delivery.Explanation, d.Message.ContextToken, s.resolveGlobalConfig())
				if err != nil {
					slog.Error("ai tool delivery: explanation send failed", "bot", d.BotDBID, "tool", tr.Name, "err", err)
					allDelivered = false
				} else {
					explained = true
					itemList, _ := json.Marshal([]map[string]any{{"type": "text", "text": tr.Delivery.Explanation}})
					s.Store.SaveMessage(&store.Message{
						BotID:       d.BotDBID,
						Direction:   "outbound",
						ToUserID:    d.Message.Sender,
						MessageType: 2,
						ItemList:    itemList,
					})
				}
			} else {
				// Media rounds defer the canned explanation to the fallback
				// flush path: do NOT send it now, do NOT save history yet.
				pendingExplanations = append(pendingExplanations, tr.Delivery.Explanation)
			}
		}
		slog.Info("persona delivery",
			"tool_name", tr.Name,
			"bot_id", d.BotDBID,
			"sender", d.Message.Sender,
			"delivery_kind", tr.Diagnostics.LogFields["delivery_kind"],
			"result_count", tr.Diagnostics.LogFields["result_count"],
			"success", tr.Diagnostics.ErrorMessage == "",
			"error_stage", tr.Diagnostics.ErrorStage,
			"error_message", tr.Diagnostics.ErrorMessage,
			"explained_to_user", explained)
	}
	return allDelivered, pendingExplanations
}

// flushPendingExplanations 在续轮无文本/失败时，把媒体轮延后的 canned 说明
// 补发给用户并入库（canned 兜底，design §6.3）。返回成功发送的条数，调用方
// 据此判断兜底说明是否真正送达用户（未达则应由 reply() 补发失败提示）。
func (s *AI) flushPendingExplanations(ctx context.Context, d Delivery, pending []string) int {
	cfg := s.resolveGlobalConfig()
	flushed := 0
	for _, exp := range pending {
		if err := s.SendHumanized(ctx, d.Provider, d.Message.Sender, exp, d.Message.ContextToken, cfg); err != nil {
			slog.Error("ai tool delivery: fallback explanation send failed", "bot", d.BotDBID, "err", err)
			continue
		}
		flushed++
		itemList, _ := json.Marshal([]map[string]any{{"type": "text", "text": exp}})
		s.Store.SaveMessage(&store.Message{
			BotID: d.BotDBID, Direction: "outbound", ToUserID: d.Message.Sender, MessageType: 2, ItemList: itemList,
		})
	}
	return flushed
}

// shouldSkipToolLLM reports whether the tool round was fully handled by direct
// delivery and the LLM continuation can be skipped (design §11.2, §6.3): every
// result must be async (the app pushes the result later) or a persona result
// fully delivered directly — a text-only round whose product (text) plus one
// natural companion explanation reach the user, so no LLM continuation round
// is needed. Media rounds (files/images) are never fully delivered here: the
// canned explanation is deferred and the LLM continuation produces the
// context-refined wording, so they return false.
func (s *AI) shouldSkipToolLLM(results []ai.ToolCallResult) bool {
	if len(results) == 0 {
		return true
	}
	for _, tr := range results {
		if tr.Async {
			continue
		}
		if s.Persona != nil && s.Persona.IsPersonaTool(tr.Name) &&
			tr.Delivery.UserFacing && strings.TrimSpace(tr.Delivery.Explanation) != "" &&
			len(tr.Files) == 0 && len(tr.Images) == 0 {
			continue
		}
		return false
	}
	return true
}

// resolveToolMedia resolves image data from a tool's media reply (base64 or URL).
func (s *AI) resolveToolMedia(ctx context.Context, botID string, result *appdelivery.DeliveryResult) []ai.ImageData {
	var data []byte
	var err error

	if result.ReplyBase64 != "" {
		b64 := result.ReplyBase64
		if idx := strings.Index(b64, ","); idx > 0 && strings.HasPrefix(b64, "data:") {
			b64 = b64[idx+1:]
		}
		data, err = base64.StdEncoding.DecodeString(b64)
		if err != nil {
			// Retry without padding (common in browser/JS encoders)
			data, err = base64.RawStdEncoding.DecodeString(b64)
			if err != nil {
				slog.Error("ai tool media: base64 decode failed", "bot", botID, "err", err)
				return nil
			}
		}
		if len(data) > maxImageBytes {
			slog.Error("ai tool media: base64 data too large", "bot", botID, "size", len(data))
			return nil
		}
	} else if result.ReplyURL != "" {
		u, err := url.Parse(result.ReplyURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			slog.Error("ai tool media: invalid url scheme", "bot", botID, "url", result.ReplyURL)
			return nil
		}
		dlCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, result.ReplyURL, nil)
		if err != nil {
			slog.Error("ai tool media: bad url", "bot", botID, "url", result.ReplyURL, "err", err)
			return nil
		}
		resp, err := safeHTTPClient.Do(req)
		if err != nil {
			slog.Error("ai tool media: download failed", "bot", botID, "url", result.ReplyURL, "err", err)
			return nil
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			slog.Error("ai tool media: download returned non-200", "bot", botID, "url", result.ReplyURL, "status", resp.StatusCode)
			return nil
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, int64(maxImageBytes)+1))
		if err != nil {
			slog.Error("ai tool media: read failed", "bot", botID, "err", err)
			return nil
		}
		if len(data) > maxImageBytes {
			slog.Error("ai tool media: download too large", "bot", botID, "size", len(data))
			return nil
		}
	} else {
		return nil
	}

	if len(data) == 0 {
		return nil
	}

	ct := http.DetectContentType(data)
	if !strings.HasPrefix(ct, "image/") {
		slog.Warn("ai tool media: not an image", "bot", botID, "ct", ct)
		return nil
	}

	return []ai.ImageData{{
		Data:        data,
		ContentType: ct,
	}}
}

func (s *AI) stopTyping(d Delivery, ticket string) {
	if ticket != "" {
		d.Provider.SendTyping(context.Background(), d.Message.Sender, ticket, false)
	}
}

// sendErrorNotice sends a user-visible error message when AI completion fails.
// The detailed error is already logged via slog.Error at each call site;
// only a generic message is shown to the user to avoid leaking internal URLs
// or API response bodies.
func (s *AI) sendErrorNotice(d Delivery, recipient string) {
	if _, sendErr := d.Provider.Send(context.Background(), provider.OutboundMessage{
		Recipient:    recipient,
		Text:         "⚠️ AI 回复失败，请稍后重试。",
		ContextToken: d.Message.ContextToken,
	}); sendErr != nil {
		slog.Error("ai error notice send failed", "bot", d.BotDBID, "err", sendErr)
	}
}

func (s *AI) resolveGlobalConfig() store.AIConfig {
	return persona.ResolveAIConfig(s.Store)
}

// parseCustomHeaders parses custom headers from JSON. Supports both array
// format [["key","value"],...] (from frontend) and object format {"key":"value"}.
func parseCustomHeaders(raw string) map[string]string {
	// Try array format first: [["k","v"],...]
	var arr [][2]string
	if json.Unmarshal([]byte(raw), &arr) == nil {
		m := make(map[string]string, len(arr))
		for _, kv := range arr {
			if kv[0] != "" {
				m[kv[0]] = kv[1]
			}
		}
		if len(m) > 0 {
			return m
		}
		return nil
	}
	// Fall back to object format: {"k":"v",...}
	var m map[string]string
	if json.Unmarshal([]byte(raw), &m) == nil && len(m) > 0 {
		return m
	}
	return nil
}

// safeHTTPClient blocks connections to private/internal IPs at the dial level,
// preventing SSRF via redirects and DNS rebinding.
var safeHTTPClient = &http.Client{
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("ssrf: invalid addr %q", addr)
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("ssrf: dns lookup failed: %w", err)
			}
			for _, ip := range ips {
				if ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsLinkLocalMulticast() {
					return nil, fmt.Errorf("ssrf: blocked private ip %s for host %s", ip.IP, host)
				}
			}
			// Connect to the first allowed IP
			d := &net.Dialer{}
			return d.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	},
}

func (s *AI) setTokenUsage(span, rootSpan *store.SpanBuilder, prompt, completion, total, cached, reasoning int) {
	if total <= 0 {
		return
	}
	for _, sp := range []*store.SpanBuilder{span, rootSpan} {
		if sp == nil {
			continue
		}
		sp.SetAttr("ai.tokens.prompt", prompt)
		sp.SetAttr("ai.tokens.completion", completion)
		sp.SetAttr("ai.tokens.total", total)
		if cached > 0 {
			sp.SetAttr("ai.tokens.cached", cached)
		}
		if reasoning > 0 {
			sp.SetAttr("ai.tokens.reasoning", reasoning)
		}
	}
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func toolResultsForContinuation(results []ai.ToolCallResult) []ai.ToolCallResult {
	llmResults := make([]ai.ToolCallResult, 0, len(results))
	for _, tr := range results {
		if tr.Async || len(tr.Images) > 0 || len(tr.Files) > 0 {
			tr.Images = nil
			tr.Files = nil
		}
		llmResults = append(llmResults, tr)
	}
	return llmResults
}
