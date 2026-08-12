package ai

import (
	"encoding/json"
	"strings"

	"github.com/openilink/openilink-hub/internal/store"
)

// matchWorldEntries scans the recent conversation text for lorebook trigger
// keywords and returns the matched content, ordered by priority. Non-matching
// entries are excluded so the world book only appears when relevant
// (soft-trigger, design §5.2).
func matchWorldEntries(entries []store.WorldEntry, recentText string) string {
	if len(entries) == 0 {
		return ""
	}
	needle := strings.ToLower(recentText)
	var hits []string
	for _, e := range entries {
		if !e.Enabled {
			continue
		}
		var keys []string
		if json.Unmarshal([]byte(e.Keys), &keys) != nil {
			continue
		}
		if matchesAny(needle, keys) {
			hits = append(hits, truncateRunes(e.Content, maxWorldEntryRunes))
		}
	}
	if len(hits) == 0 {
		return ""
	}
	// Entries are pre-sorted by priority DESC from the store; dedupe by content.
	seen := make(map[string]bool)
	var out []string
	for _, h := range hits {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	// Cap total to protect the context window.
	return strings.Join(out[:min(len(out), 8)], "\n---\n")
}

// matchesAny reports whether the haystack contains any keyword (case-insensitive).
func matchesAny(haystack string, keywords []string) bool {
	for _, k := range keywords {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if strings.Contains(haystack, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
