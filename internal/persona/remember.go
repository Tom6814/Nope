package persona

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/openilink/openilink-hub/internal/ai"
	"github.com/openilink/openilink-hub/internal/store"
)

// handleRemember records a memory for (botID, sender) and lazily triggers an
// async consolidation when the memory count exceeds the configured threshold.
func handleRemember(ctx context.Context, e *Engine, botID, sender string, args map[string]any) ai.ToolCallResult {
	content := strings.TrimSpace(strArg(args, "content"))
	if content == "" {
		return ai.ToolCallResult{Content: "remember: content required"}
	}
	category := strArg(args, "category")
	if category == "" {
		category = "fact"
	}
	importance := intArg(args, "importance")
	if importance < 1 || importance > 5 {
		importance = 1
	}
	m := &store.Memory{
		BotID:      botID,
		Sender:     sender,
		Category:   category,
		Content:    content,
		Importance: importance,
	}
	if exp := strArg(args, "expires_at"); exp != "" {
		if t, err := parseTime(exp); err == nil {
			unix := t.Unix()
			m.ExpiresAt = &unix
		}
	}
	if err := e.Store.AddMemory(m); err != nil {
		slog.Error("persona: remember failed", "bot", botID, "sender", sender, "err", err)
		return ai.ToolCallResult{Content: "remember failed"}
	}
	e.maybeConsolidate(botID, sender)
	return ai.ToolCallResult{Content: "ok"}
}

// maybeConsolidate lazily triggers consolidation when the memory count for a
// scope crosses the threshold. It never blocks the conversation.
func (e *Engine) maybeConsolidate(botID, sender string) {
	cfg := ResolveAIConfig(e.Store)
	threshold := cfg.MemoryConsolidateThreshold
	if threshold <= 0 {
		threshold = 40
	}
	count, err := e.Store.CountMemories(botID, sender)
	if err != nil || count < threshold {
		return
	}
	go e.consolidate(botID, sender)
}

// recent window: the most recent 15 memories OR anything within 7 days is
// "recent" and is hard-protected against DELETE/COMPRESS (design §7.2).
const (
	recentCount = 15
	recentDays  = 7
)

// consolidate asks the AI to prune/summarize the stale region of a memory
// stream. The recent window is code-enforced untouchable: even if the AI asks
// to delete a recent memory, the request is ignored (we do not fully trust AI
// output).
func (e *Engine) consolidate(botID, sender string) {
	if e.Complete == nil {
		return
	}
	mems, err := e.Store.ListMemories(botID, sender)
	if err != nil || len(mems) == 0 {
		return
	}
	now := time.Now()
	recentSet := make(map[string]bool)
	var stale []store.Memory
	for i, m := range mems {
		if i < recentCount || (m.CreatedAt > 0 && now.Sub(time.Unix(m.CreatedAt, 0)) < recentDays*24*time.Hour) {
			recentSet[m.ID] = true
			continue
		}
		stale = append(stale, m)
	}
	if len(stale) == 0 {
		return
	}

	cfg := ResolveAIConfig(e.Store)
	if cfg.APIKey == "" {
		return
	}

	var b strings.Builder
	b.WriteString("以下是某个人（sender: ")
	b.WriteString(sender)
	b.WriteString("）的旧记忆。请你整理它们，只处理下面列出的记忆，输出 JSON 数组，每项为：\n")
	b.WriteString(`{"action":"DELETE|COMPRESS|KEEP","ids":["..."],"new_content":"(仅 COMPRESS 时，把这几条提炼合并成一条)"}` + "\n\n")
	b.WriteString("规则：过期的小事 DELETE；多条同主题的陈旧记忆 COMPRESS 成一条；其余 KEEP。\n")
	for _, m := range stale {
		exp := ""
		if m.ExpiresAt != nil {
			exp = " expires_at=" + time.Unix(*m.ExpiresAt, 0).Format("2006-01-02")
		}
		b.WriteString("- id=" + m.ID + " cat=" + m.Category + " imp=" + itoa(m.Importance) + exp + " : " + m.Content + "\n")
	}
	prompt := b.String()

	result, err := e.Complete(context.Background(), cfg, []ai.Message{
		{Role: "system", Content: "你是记忆整理助手，输出严格 JSON，不要多余文字。"},
		{Role: "user", Content: prompt},
	}, nil)
	if err != nil || result == nil {
		slog.Warn("persona: consolidation completion failed", "bot", botID, "err", err)
		return
	}
	e.applyConsolidation(mems, recentSet, result.Content)
}

func (e *Engine) applyConsolidation(all []store.Memory, recentSet map[string]bool, raw string) {
	raw = strings.TrimSpace(raw)
	start, end := strings.Index(raw, "["), strings.LastIndex(raw, "]")
	if start < 0 || end <= start {
		slog.Warn("persona: consolidation output not JSON array")
		return
	}
	var ops []struct {
		Action     string   `json:"action"`
		IDs        []string `json:"ids"`
		NewContent string   `json:"new_content"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &ops); err != nil {
		slog.Warn("persona: consolidation output parse failed", "err", err)
		return
	}

	byID := make(map[string]store.Memory, len(all))
	for _, m := range all {
		byID[m.ID] = m
	}

	for _, op := range ops {
		switch strings.ToUpper(op.Action) {
		case "DELETE":
			for _, id := range op.IDs {
				// Hard protection: never delete recent memories.
				if recentSet[id] {
					continue
				}
				if _, ok := byID[id]; ok {
					e.Store.DeleteMemory(id)
				}
			}
		case "COMPRESS":
			var targets []store.Memory
			recentTouched := false
			for _, id := range op.IDs {
				if recentSet[id] {
					recentTouched = true
					continue
				}
				if m, ok := byID[id]; ok {
					targets = append(targets, m)
				}
			}
			if recentTouched || len(targets) < 2 || strings.TrimSpace(op.NewContent) == "" {
				continue
			}
			merged := store.Memory{
				BotID:      targets[0].BotID,
				Sender:     targets[0].Sender,
				Category:   "detail",
				Content:    op.NewContent,
				Importance: 2,
			}
			if err := e.Store.AddMemory(&merged); err != nil {
				continue
			}
			for _, m := range targets {
				e.Store.DeleteMemory(m.ID)
			}
		}
	}
	slog.Info("persona: consolidation applied", "ops", len(ops))
}

func itoa(n int) string { return strconv.Itoa(n) }

// parseTime parses common date/time formats into a time.Time.
func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"2006/01/02 15:04",
		"2006/01/02",
		"01-02 15:04",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	// Fall back to relative seconds.
	if n, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(n), nil
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q", s)
}
