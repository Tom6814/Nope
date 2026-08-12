package persona

import (
	"context"
	"log/slog"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/openilink/openilink-hub/internal/ai"
)

// handleWriteDoc produces a text/markdown document, persists it to object
// storage when configured (design §9.2), and returns it as a file result so
// the AI sink delivers it as a file message.
func handleWriteDoc(ctx context.Context, e *Engine, botID, sender string, args map[string]any) ai.ToolCallResult {
	title := strings.TrimSpace(strArg(args, "title"))
	content := strArg(args, "content")
	if title == "" || strings.TrimSpace(content) == "" {
		return ai.ToolCallResult{
			Content: "你想让我整理的文档还差标题或内容，补全后我就能直接写给你。",
			Delivery: ai.ToolCallDelivery{
				UserFacing:  true,
				Explanation: "标题或内容还不完整，你补一下我马上给你整理成文档。",
			},
			Diagnostics: ai.ToolCallDiagnostics{
				ErrorStage:   "validate",
				ErrorMessage: "title and content required",
				LogFields: map[string]string{
					"delivery_kind": "file",
					"result_count":  "0",
				},
			},
		}
	}
	title = normalizeDocTitle(title)
	ct := "text/markdown"

	if e.Storage != nil {
		key := "docs/" + botID + "/" + title
		if _, err := e.Storage.Put(ctx, key, ct, []byte(content)); err != nil {
			slog.Warn("persona: write_doc storage put failed", "key", key, "err", err)
		}
	}

	return ai.ToolCallResult{
		Content: "文档已经整理好了。",
		Files:   []ai.FileData{{Data: []byte(content), FileName: title, ContentType: ct}},
		Delivery: ai.ToolCallDelivery{
			UserFacing:  true,
			Explanation: "我整理好了，你直接看这个文档就行。",
		},
		Diagnostics: ai.ToolCallDiagnostics{
			LogFields: map[string]string{
				"delivery_kind": "file",
				"result_count":  "1",
			},
		},
	}
}

func normalizeDocTitle(title string) string {
	title = strings.TrimSpace(title)
	title = strings.ReplaceAll(title, "\\", "/")
	title = strings.TrimSpace(path.Base(title))
	if ext := filepath.Ext(title); ext != "" {
		title = strings.TrimSuffix(title, ext)
	}
	title = sanitizeDocTitleStem(title)
	if title == "" {
		title = "document"
	}
	return title + ".md"
}

func sanitizeDocTitleStem(title string) string {
	var b strings.Builder
	sep := false
	for _, r := range strings.TrimSpace(title) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			sep = false
		case r == '-' || r == '_':
			if b.Len() > 0 && !sep {
				b.WriteRune(r)
				sep = true
			}
		case unicode.IsSpace(r), r == '.', r == ':', r == '/', r == '\\', r == '?', r == '%', r == '*', r == '|', r == '"', r == '<', r == '>':
			if b.Len() > 0 && !sep {
				b.WriteRune('-')
				sep = true
			}
		default:
			if b.Len() > 0 && !sep {
				b.WriteRune('-')
				sep = true
			}
		}
	}
	return strings.Trim(b.String(), " .-_")
}
