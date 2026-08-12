// Package character parses SillyTavern V2 character cards (JSON or PNG-embedded)
// and extracts persona fields plus the embedded lorebook (character_book).
package character

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Card is the parsed representation of a SillyTavern V2 character card.
type Card struct {
	Spec                    string
	SpecVersion             string
	Name                    string
	Description             string
	Personality             string
	Scenario                string
	FirstMes                string
	MesExample              string
	SystemPrompt            string
	PostHistoryInstructions string
	Creator                 string
	CharacterVersion        string
	Tags                    []string
	// Avatar is the PNG-embedded avatar image (nil when the card is plain JSON
	// or the file carries no embedded image).
	Avatar []byte
	// WorldEntries are the lorebook entries from character_book.
	WorldEntries []WorldEntry
	// Raw is the original card JSON (V2 data object) for lossless round-trip.
	Raw json.RawMessage
}

// WorldEntry mirrors one lorebook entry from character_book.
type WorldEntry struct {
	Keys        []string
	Content     string
	Enabled     bool
	Priority    int
	Name        string
	Comment     string
	Secondary   []string
	Constant    bool
	CaseSensitive bool
}

// ParsedData is the internal shape of the card's "data" object (V2 spec).
type ParsedData struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Personality string            `json:"personality"`
	Scenario    string            `json:"scenario"`
	FirstMes    string            `json:"first_mes"`
	MesExample  string            `json:"mes_example"`
	SystemPrompt string           `json:"system_prompt"`
	PostHistoryInstructions string `json:"post_history_instructions"`
	Creator     string            `json:"creator"`
	CharacterVersion string       `json:"character_version"`
	Tags        []string          `json:"tags"`
	CharacterBook *CharacterBook  `json:"character_book"`
}

// CharacterBook is the V2 embedded lorebook.
type CharacterBook struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Entries     []BookEntry       `json:"entries"`
}

type BookEntry struct {
	Keys          []string `json:"keys"`
	SecondaryKeys []string `json:"secondary_keys"`
	Content       string   `json:"content"`
	Enabled       bool     `json:"enabled"`
	InsertionOrder int     `json:"insertion_order"`
	Priority      int      `json:"priority"`
	Name          string   `json:"name"`
	Comment       string   `json:"comment"`
	Constant      bool     `json:"constant"`
	CaseSensitive bool     `json:"case_sensitive"`
}

var (
	ErrNotACard    = errors.New("not a valid character card")
	ErrInvalidJSON = errors.New("character card JSON is invalid")
)

// pngSignature is the fixed 8-byte PNG magic.
var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// Parse parses a character card from raw bytes, auto-detecting PNG-embedded
// cards (SillyTavern export format) vs plain V2 JSON. For PNG cards the
// embedded image is returned as the avatar.
func Parse(data []byte) (*Card, error) {
	if len(data) >= len(pngSignature) && bytes.Equal(data[:len(pngSignature)], pngSignature) {
		return parsePNG(data)
	}
	return parseJSON(data)
}

// parseJSON parses a plain SillyTavern V2 JSON card.
func parseJSON(data []byte) (*Card, error) {
	var envelope struct {
		Spec    string          `json:"spec"`
		Version string          `json:"spec_version"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	raw := envelope.Data
	if len(raw) == 0 {
		// Some exports put the data object at top level without the envelope.
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(data, &probe); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
		}
		if _, ok := probe["name"]; ok {
			raw = data
		} else {
			return nil, ErrNotACard
		}
	}
	return buildCard(envelope.Spec, envelope.Version, raw, nil)
}

// parsePNG extracts the character card JSON from the PNG's text chunks and
// returns the embedded image as the avatar.
func parsePNG(data []byte) (*Card, error) {
	// Walk chunks: 8-byte signature, then (length, type, data, crc).
	pos := 8
	var cardJSON []byte
	for pos+8 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		chunkType := string(data[pos+4 : pos+8])
		chunkStart := pos + 8
		chunkEnd := chunkStart + length
		if chunkEnd+4 > len(data) {
			break
		}
		chunkData := data[chunkStart:chunkEnd]
		switch chunkType {
		case "tEXt":
			// keyword \x00 text
			if idx := bytes.IndexByte(chunkData, 0); idx >= 0 {
				keyword := string(chunkData[:idx])
				text := chunkData[idx+1:]
				if isCardKeyword(keyword) {
					cardJSON = text
				}
			}
		case "iTXt":
			// keyword \x00 compression flag \x00 compression method \x00 language \x00 translated \x00 text
			if idx := bytes.IndexByte(chunkData, 0); idx >= 0 {
				keyword := string(chunkData[:idx])
				if isCardKeyword(keyword) {
					rest := chunkData[idx+1:]
					if len(rest) >= 2 {
						text := rest[2:] // skip compression flag + method
						// skip language\0translated\0
						if li := bytes.IndexByte(text, 0); li >= 0 {
							text = text[li+1:]
							if li2 := bytes.IndexByte(text, 0); li2 >= 0 {
								text = text[li2+1:]
							}
						}
						cardJSON = text
					}
				}
			}
		case "zTXt":
			// keyword \x00 compression method \x00 compressed text (zlib)
			if idx := bytes.IndexByte(chunkData, 0); idx >= 0 {
				keyword := string(chunkData[:idx])
				if isCardKeyword(keyword) {
					// Not decompressing zTXt — falls through; most exporters use tEXt/iTXt.
					_ = chunkData
				}
			}
		}
		pos = chunkEnd + 4 // skip CRC
		if chunkType == "IEND" {
			break
		}
	}
	if len(cardJSON) == 0 {
		return nil, ErrNotACard
	}
	// The text chunk is base64-encoded JSON (SillyTavern PNG format).
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(cardJSON)))
	if err != nil {
		// Fall back to plain JSON text.
		decoded = cardJSON
	}
	return parseJSONWithAvatar(decoded, data)
}

func parseJSONWithAvatar(jsonData []byte, avatar []byte) (*Card, error) {
	card, err := parseJSON(jsonData)
	if err != nil {
		return nil, err
	}
	card.Avatar = avatar
	return card, nil
}

func isCardKeyword(k string) bool {
	k = strings.ToLower(strings.TrimSpace(k))
	switch k {
	case "chara", "chara_card_v2", "chara_card_v1", "ccv3", "ccv2", "ccv1", "character":
		return true
	}
	return strings.Contains(k, "chara")
}

func buildCard(spec, version string, raw json.RawMessage, avatar []byte) (*Card, error) {
	var d ParsedData
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	if d.Name == "" {
		return nil, ErrNotACard
	}
	card := &Card{
		Spec:                    spec,
		SpecVersion:             version,
		Name:                    d.Name,
		Description:             d.Description,
		Personality:             d.Personality,
		Scenario:                d.Scenario,
		FirstMes:                d.FirstMes,
		MesExample:              d.MesExample,
		SystemPrompt:            d.SystemPrompt,
		PostHistoryInstructions: d.PostHistoryInstructions,
		Creator:                 d.Creator,
		CharacterVersion:        d.CharacterVersion,
		Tags:                    d.Tags,
		Avatar:                  avatar,
		Raw:                     raw,
	}
	if d.CharacterBook != nil {
		for _, e := range d.CharacterBook.Entries {
			enabled := e.Enabled
			if !enabled {
				// SillyTavern exports default enabled=true; treat absent as enabled.
				enabled = true
			}
			card.WorldEntries = append(card.WorldEntries, WorldEntry{
				Keys:          e.Keys,
				Content:       e.Content,
				Enabled:       enabled,
				Priority:      e.Priority,
				Name:          e.Name,
				Comment:       e.Comment,
				Secondary:     e.SecondaryKeys,
				Constant:      e.Constant,
				CaseSensitive: e.CaseSensitive,
			})
		}
	}
	return card, nil
}

// ComposePersonaText renders the character card's persona layer: "你是<name>，…"
// It merges description / personality / scenario into natural background text.
func ComposePersonaText(c *Card) string {
	if c == nil {
		return ""
	}
	var parts []string
	if c.Description != "" {
		parts = append(parts, c.Description)
	}
	if c.Personality != "" {
		parts = append(parts, "性格："+c.Personality)
	}
	if c.Scenario != "" {
		parts = append(parts, "当前场景："+c.Scenario)
	}
	body := strings.Join(parts, "\n")
	if body == "" {
		return ""
	}
	return fmt.Sprintf("你是%s。\n%s", c.Name, body)
}
