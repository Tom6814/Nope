package persona

import (
	"encoding/json"
	"fmt"

	"github.com/openilink/openilink-hub/internal/store"
)

// ResolveAIConfig assembles the global AIConfig from the "ai.*" KV channel.
// It is shared by the AI sink and the persona engine so the persona tools see
// exactly the same configuration as the reply path.
func ResolveAIConfig(s store.ConfigStore) store.AIConfig {
	global, _ := s.ListConfigByPrefix("ai.")
	if global["ai.api_key"] == "" {
		return store.AIConfig{}
	}
	var cfg store.AIConfig
	cfg.Source = "builtin"
	cfg.BaseURL = global["ai.base_url"]
	cfg.APIKey = global["ai.api_key"]
	cfg.Model = global["ai.model"]
	cfg.SystemPrompt = global["ai.system_prompt"]
	cfg.HideThinking = global["ai.hide_thinking"] == "true"
	cfg.StripMarkdown = global["ai.strip_markdown"] == "true"
	cfg.CharacterID = global["ai.character_id"]
	cfg.MemoryScope = global["ai.memory_scope"]
	if global["ai.humanize_wechat"] == "" || global["ai.humanize_wechat"] == "true" {
		cfg.HumanizeWechat = true // default on
	}
	if global["ai.proactive_enabled"] == "" || global["ai.proactive_enabled"] == "true" {
		cfg.ProactiveEnabled = true // default on
	}
	cfg.ProactiveQuietHours = global["ai.proactive_quiet_hours"]
	if cfg.ProactiveQuietHours == "" {
		cfg.ProactiveQuietHours = "23:00-08:00"
	}
	if v := global["ai.proactive_daily_limit"]; v != "" {
		fmt.Sscanf(v, "%d", &cfg.ProactiveDailyLimit)
	}
	if cfg.ProactiveDailyLimit <= 0 {
		cfg.ProactiveDailyLimit = 3
	}
	if v := global["ai.proactive_min_interval"]; v != "" {
		fmt.Sscanf(v, "%d", &cfg.ProactiveMinInterval)
	}
	if cfg.ProactiveMinInterval <= 0 {
		cfg.ProactiveMinInterval = 7200
	}
	if v := global["ai.memory_consolidate_threshold"]; v != "" {
		fmt.Sscanf(v, "%d", &cfg.MemoryConsolidateThreshold)
	}
	if cfg.MemoryConsolidateThreshold <= 0 {
		cfg.MemoryConsolidateThreshold = 40
	}
	if v := global["ai.max_history"]; v != "" {
		fmt.Sscanf(v, "%d", &cfg.MaxHistory)
	}
	if v := global["ai.custom_headers"]; v != "" {
		cfg.CustomHeaders = parseCustomHeaders(v)
	}
	return cfg
}

// parseCustomHeaders parses custom headers from JSON, supporting the array
// format [["key","value"],...] (from the frontend) and object format.
func parseCustomHeaders(raw string) map[string]string {
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
	var m map[string]string
	if json.Unmarshal([]byte(raw), &m) == nil && len(m) > 0 {
		return m
	}
	return nil
}
