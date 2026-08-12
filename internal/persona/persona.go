// Package persona implements the "like a real person" capability layer:
// memory write tools, proactive message scheduling, relationship-tree tools and
// agent tools (image / search / doc). It is wired into the AI sink so persona
// tools are always available to the LLM; execution stays in-process and silent
// (no "🔧 调用" status spam, no user-visible intermediate steps).
package persona

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/openilink/openilink-hub/internal/ai"
	"github.com/openilink/openilink-hub/internal/storage"
	"github.com/openilink/openilink-hub/internal/store"
)

// Completer calls the OpenAI-compatible chat completion API. ai.CompleteMessages
// satisfies it; wiring an instance avoids importing the ai package's internals
// while keeping the engine testable with a fake.
type Completer func(ctx context.Context, cfg store.AIConfig, messages []ai.Message, tools []ai.Tool) (*ai.CompletionResult, error)

// Engine executes persona tool calls for the AI sink.
type Engine struct {
	Store    store.Store
	Storage  storage.Store // optional; used for write_doc persistence
	Images   ImageProvider // optional image generator (gen_image)
	Search   Searcher      // optional search backend (web_search)
	Complete Completer

	// now is injectable for tests.
	now func() time.Time
}

// New creates a persona engine.
func New(s store.Store, st storage.Store, complete Completer) *Engine {
	return &Engine{
		Store:    s,
		Storage:  st,
		Complete: complete,
		now:      time.Now,
	}
}

// SetImageProvider configures the image generator.
func (e *Engine) SetImageProvider(p ImageProvider) { e.Images = p }

// SetSearcher configures the web search backend.
func (e *Engine) SetSearcher(s Searcher) { e.Search = s }

// IsPersonaTool reports whether a tool name belongs to the persona engine.
func (e *Engine) IsPersonaTool(name string) bool {
	_, ok := toolHandlers[name]
	return ok
}

// ToolNames returns the sorted list of persona tool names.
func (e *Engine) ToolNames() []string {
	var out []string
	for name := range toolHandlers {
		out = append(out, name)
	}
	return out
}

// Execute runs a persona tool with the delivery context (botID, sender) filled
// in by the caller — the AI never supplies these itself.
func (e *Engine) Execute(ctx context.Context, botID, sender, toolName string, args json.RawMessage) ai.ToolCallResult {
	handler, ok := toolHandlers[toolName]
	if !ok {
		return ai.ToolCallResult{Content: "unknown persona tool: " + toolName}
	}
	var argMap map[string]any
	json.Unmarshal(args, &argMap)

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	start := time.Now()
	res := handler(ctx, e, botID, sender, argMap)
	if res.Name == "" {
		res.Name = toolName
	}
	res.Diagnostics.SanitizeLogFields()
	slog.Debug("persona tool executed", "tool", toolName, "bot", botID, "sender", sender,
		"duration_ms", time.Since(start).Milliseconds())
	return res
}

// toolHandler executes one persona tool with already-parsed args.
type toolHandler func(ctx context.Context, e *Engine, botID, sender string, args map[string]any) ai.ToolCallResult

var toolHandlers = map[string]toolHandler{
	"remember":         handleRemember,
	"schedule_message": handleScheduleMessage,
	"cancel_schedule":  handleCancelSchedule,
	"lookup_contact":   handleLookupContact,
	"upsert_contact":   handleUpsertContact,
	"upsert_relation":  handleUpsertRelation,
	"set_owner":        handleSetOwner,
	"web_search":       handleWebSearch,
	"write_doc":        handleWriteDoc,
	"gen_image":        handleGenImage,
}

// strArg returns a string argument.
func strArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func jsonRaw(s string) json.RawMessage { return json.RawMessage(s) }

// intArg returns an int argument.
func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

// boolArg returns a bool argument.
func boolArg(args map[string]any, key string) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return false
}
