package ai

import "sort"

// Per-layer size limits for the system segment (design §S6 H2). Thresholds are
// conservative on purpose: normal data stays untouched, only oversized content
// is cut. All limits are measured in runes, not bytes, so CJK text is not
// penalized ~3x.
const (
	// maxCardFieldRunes caps each character-card persona field
	// (description / personality / scenario).
	maxCardFieldRunes = 2000
	// maxWorldEntryRunes caps a single lorebook entry's content.
	maxWorldEntryRunes = 1000
	// maxRelationLines caps how many relationship-edge lines are resident.
	maxRelationLines = 30
	// maxBriefRunes caps a single contact-brief line.
	maxBriefRunes = 200
	// maxMesExampleRunes caps the dialogue-style example block.
	maxMesExampleRunes = 800
	// maxSystemRunes is the total-budget guardrail for all system layers
	// combined. The rules layer is exempt from shrinking.
	maxSystemRunes = 12000
)

// truncateRunes truncates s to at most max runes, appending "…" when
// truncated. Multi-byte characters are never split.
func truncateRunes(s string, max int) string {
	if max < 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// systemLayer is one composed system-segment layer with the metadata the
// total-budget guardrail needs.
type systemLayer struct {
	text        string
	fixed       bool // rules layer — never truncated
	truncatable bool // participates in the total-budget guardrail
	prio        int  // truncation order; smaller = truncated first (bottom-up)
}

// enforceSystemBudget is the total guardrail for the system segment: when the
// composed layers together exceed maxSystemRunes it shrinks layers bottom-up
// (system prompt → character → world book → relation skeleton), never touching
// the rules layer (fixed). Per-layer limits already bound each layer; this is
// the last-resort safety net.
func enforceSystemBudget(layers []systemLayer) []systemLayer {
	total := 0
	for _, l := range layers {
		total += len([]rune(l.text))
	}
	if total <= maxSystemRunes {
		return layers
	}

	// Truncatable layers, ordered by priority (bottom-up).
	mutable := make([]int, 0, len(layers))
	for i, l := range layers {
		if l.truncatable {
			mutable = append(mutable, i)
		}
	}
	sort.SliceStable(mutable, func(a, b int) bool {
		return layers[mutable[a]].prio < layers[mutable[b]].prio
	})

	over := total - maxSystemRunes
	for _, i := range mutable {
		if over <= 0 {
			break
		}
		cur := len([]rune(layers[i].text))
		if cur == 0 {
			continue
		}
		if cur <= over {
			over -= cur
			layers[i].text = ""
			continue
		}
		// Partial truncation keeping exactly `cur - over` runes including the
		// ellipsis, so the total lands on the budget exactly.
		keep := cur - over
		layers[i].text = truncateRunes(layers[i].text, keep-1)
		over = 0
	}
	return layers
}
