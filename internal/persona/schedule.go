package persona

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/openilink/openilink-hub/internal/ai"
	"github.com/openilink/openilink-hub/internal/store"
)

// ParseTime 解析 ISO 格式或 YYYY-MM-DD HH:MM 为本地时间。
func ParseTime(s string) (time.Time, error) { return parseTime(s) }

// handleScheduleMessage registers a future message. Origin distinguishes user
// promises (always fire) from AI-initiated proactive messages (gated by
// anti-disturbance rules; also de-duplicated per (bot, sender)).
func handleScheduleMessage(ctx context.Context, e *Engine, botID, sender string, args map[string]any) ai.ToolCallResult {
	fireAtStr := strArg(args, "fire_at")
	prompt := strArg(args, "prompt")
	if fireAtStr == "" || prompt == "" {
		return ai.ToolCallResult{
			Content: "schedule_message: fire_at and prompt required",
			Diagnostics: ai.ToolCallDiagnostics{
				ErrorStage:   "validate",
				ErrorMessage: "fire_at and prompt required",
				LogFields: map[string]string{
					"delivery_kind": "schedule",
					"result_count":  "0",
				},
			},
		}
	}
	t, err := ParseTime(fireAtStr)
	if err != nil {
		return ai.ToolCallResult{
			Content: "schedule_message: cannot parse fire_at, use ISO format or YYYY-MM-DD HH:MM",
			Diagnostics: ai.ToolCallDiagnostics{
				ErrorStage:   "validate",
				ErrorMessage: "invalid fire_at",
				LogFields: map[string]string{
					"delivery_kind": "schedule",
					"result_count":  "0",
				},
			},
		}
	}
	origin := strArg(args, "origin")
	if origin != "proactive" {
		origin = "promise"
	}
	tolerance := int64(intArg(args, "tolerance"))
	if tolerance <= 0 {
		tolerance = 600
	}

	// Proactive dedup: at most one pending proactive per (bot, sender);
	// registering a new one replaces the old pending one (§8.4).
	if origin == "proactive" {
		e.replacePendingProactive(botID, sender)
	}

	m := &store.ScheduledMessage{
		BotID:     botID,
		Sender:    sender,
		FireAt:    t.Unix(),
		Tolerance: tolerance,
		Prompt:    prompt,
		Origin:    origin,
		Status:    "pending",
	}
	if err := e.Store.AddScheduledMessage(m); err != nil {
		slog.Error("persona: schedule_message failed", "bot", botID, "err", err)
		return ai.ToolCallResult{
			Content: "schedule failed",
			Diagnostics: ai.ToolCallDiagnostics{
				ErrorStage:   "store",
				ErrorMessage: err.Error(),
				LogFields: map[string]string{
					"delivery_kind": "schedule",
					"result_count":  "0",
				},
			},
		}
	}
	return ai.ToolCallResult{
		Content: fmt.Sprintf("已登记，id=%s，origin=%s，fire_at=%s", m.ID, origin, t.Format("2006-01-02 15:04")),
		Diagnostics: ai.ToolCallDiagnostics{LogFields: map[string]string{
			"delivery_kind": "schedule",
			"result_count":  "1",
			"origin":        origin,
			"fire_at":       t.Format("2006-01-02 15:04"),
		}},
	}
}

// replacePendingProactive cancels any existing pending proactive message for
// the same (bot, sender) before registering a new one.
func (e *Engine) replacePendingProactive(botID, sender string) {
	pending, err := e.Store.ListPendingByScope(botID, sender, "proactive")
	if err != nil {
		return
	}
	for _, m := range pending {
		e.Store.CancelScheduledMessage(m.ID)
	}
}

// handleCancelSchedule cancels a scheduled message by id.
func handleCancelSchedule(ctx context.Context, e *Engine, botID, sender string, args map[string]any) ai.ToolCallResult {
	id := strArg(args, "id")
	if id == "" {
		return ai.ToolCallResult{
			Content: "cancel_schedule: id required",
			Diagnostics: ai.ToolCallDiagnostics{
				ErrorStage:   "validate",
				ErrorMessage: "id required",
				LogFields: map[string]string{
					"delivery_kind": "schedule",
					"result_count":  "0",
				},
			},
		}
	}
	for _, origin := range []string{"promise", "proactive"} {
		pending, err := e.Store.ListPendingByScope(botID, sender, origin)
		if err != nil {
			return ai.ToolCallResult{
				Content: "cancel failed",
				Diagnostics: ai.ToolCallDiagnostics{
					ErrorStage:   "store",
					ErrorMessage: err.Error(),
					LogFields: map[string]string{
						"delivery_kind": "schedule",
						"result_count":  "0",
					},
				},
			}
		}
		for _, m := range pending {
			if m.ID == id {
				if err := e.Store.CancelScheduledMessage(id); err != nil {
					return ai.ToolCallResult{
						Content: "cancel failed",
						Diagnostics: ai.ToolCallDiagnostics{
							ErrorStage:   "store",
							ErrorMessage: err.Error(),
							LogFields: map[string]string{
								"delivery_kind": "schedule",
								"result_count":  "0",
							},
						},
					}
				}
				return ai.ToolCallResult{
					Content: "已取消",
					Diagnostics: ai.ToolCallDiagnostics{LogFields: map[string]string{
						"delivery_kind": "schedule",
						"result_count":  "1",
					}},
				}
			}
		}
	}
	return ai.ToolCallResult{
		Content: "cancel_schedule: not found",
		Diagnostics: ai.ToolCallDiagnostics{
			ErrorStage:   "cancel",
			ErrorMessage: "not found",
			LogFields: map[string]string{
				"delivery_kind": "schedule",
				"result_count":  "0",
			},
		},
	}
}
