package character

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func TestParse_PlainV2JSON(t *testing.T) {
	card, err := Parse([]byte(v2CardJSON("小爱", "温柔的女友")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if card.Name != "小爱" {
		t.Errorf("name = %q", card.Name)
	}
	if card.Description != "温柔的女友" {
		t.Errorf("description = %q", card.Description)
	}
	if card.Avatar != nil {
		t.Error("plain JSON should have no avatar")
	}
}

func TestParse_InvalidJSON(t *testing.T) {
	if _, err := Parse([]byte(`{"not":"a card"}`)); err == nil {
		t.Error("expected error for non-card JSON")
	}
	if _, err := Parse([]byte(`{invalid`)); err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestParse_PNGEmbeddedCard(t *testing.T) {
	cardJSON := v2CardJSON("PNG小妹", "从 PNG 里来")
	// Build a minimal PNG: signature + IHDR + tEXt("chara"→base64 JSON) + IEND.
	png := buildPNGWithText("chara", base64.StdEncoding.EncodeToString([]byte(cardJSON)))

	card, err := Parse(png)
	if err != nil {
		t.Fatalf("Parse(PNG): %v", err)
	}
	if card.Name != "PNG小妹" {
		t.Errorf("name = %q", card.Name)
	}
	// Avatar = the whole PNG image bytes.
	if len(card.Avatar) == 0 {
		t.Error("PNG-embedded card should expose the image as avatar")
	}
	if !bytes.Equal(card.Avatar, png) {
		t.Error("avatar should be the source PNG")
	}
}

func TestParse_PNGWithoutCard(t *testing.T) {
	png := buildPNGWithText("Title", "not a card")
	if _, err := Parse(png); err == nil {
		t.Error("expected error for PNG without character card text chunk")
	}
}

func TestComposePersonaText(t *testing.T) {
	card, _ := Parse([]byte(v2CardJSON("小爱", "温柔的女友")))
	text := ComposePersonaText(card)
	if !bytes.Contains([]byte(text), []byte("你是小爱")) {
		t.Errorf("persona text missing name: %s", text)
	}
	if !bytes.Contains([]byte(text), []byte("温柔的女友")) {
		t.Errorf("persona text missing description: %s", text)
	}

	// Empty card → empty text (layer skipped).
	card2 := &Card{Name: "x"}
	if ComposePersonaText(card2) != "" {
		t.Error("empty card should render empty persona text")
	}
}

// v2CardJSON builds a minimal SillyTavern V2 envelope.
func v2CardJSON(name, desc string) string {
	envelope := map[string]any{
		"spec":         "chara_card_v2",
		"spec_version": "2.0",
		"data": map[string]any{
			"name":        name,
			"description": desc,
			"personality": "粘人",
			"scenario":    "刚认识不久",
		},
	}
	b, _ := json.Marshal(envelope)
	return string(b)
}

// buildPNGWithText builds a structurally valid PNG containing one tEXt chunk.
func buildPNGWithText(keyword, text string) []byte {
	var buf bytes.Buffer
	buf.Write(pngSignature)

	// IHDR chunk (13 bytes data, minimal valid header).
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], 1)  // width
	binary.BigEndian.PutUint32(ihdr[4:8], 1)  // height
	ihdr[8] = 8                                // bit depth
	ihdr[9] = 6                                // color type RGBA
	writeChunk(&buf, "IHDR", ihdr)

	// tEXt chunk: keyword\0text
	textData := append([]byte(keyword), 0)
	textData = append(textData, []byte(text)...)
	writeChunk(&buf, "tEXt", textData)

	writeChunk(&buf, "IEND", nil)
	return buf.Bytes()
}

func writeChunk(buf *bytes.Buffer, chunkType string, data []byte) {
	binary.Write(buf, binary.BigEndian, uint32(len(data)))
	buf.WriteString(chunkType)
	buf.Write(data)
	// CRC (zeros acceptable for tests that only parse).
	binary.Write(buf, binary.BigEndian, uint32(0))
}
