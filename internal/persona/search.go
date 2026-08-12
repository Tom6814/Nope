package persona

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openilink/openilink-hub/internal/ai"
)

// SearchResult is one web search hit.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// Searcher is the pluggable web search backend for the web_search tool.
type Searcher interface {
	Search(ctx context.Context, query string) ([]SearchResult, error)
}

// duckduckgoSearcher uses the DuckDuckGo Instant Answer API (no API key).
// Operators can replace it with their own Searcher implementation.
type duckduckgoSearcher struct {
	client *http.Client
}

// NewDuckDuckGoSearcher builds the default key-free search backend.
func NewDuckDuckGoSearcher() Searcher {
	return &duckduckgoSearcher{client: &http.Client{Timeout: 15 * time.Second}}
}

func (d *duckduckgoSearcher) Search(ctx context.Context, query string) ([]SearchResult, error) {
	endpoint := "https://api.duckduckgo.com/?q=" + url.QueryEscape(query) + "&format=json&no_html=1&skip_disambig=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "OpeniLink-Hub/1.0")
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("search returned %d", resp.StatusCode)
	}

	var out struct {
		AbstractText  string `json:"AbstractText"`
		AbstractURL   string `json:"AbstractURL"`
		Heading       string `json:"Heading"`
		RelatedTopics []struct {
			Text     string `json:"Text"`
			FirstURL string `json:"FirstURL"`
			Topics   []struct {
				Text     string `json:"Text"`
				FirstURL string `json:"FirstURL"`
			} `json:"Topics"`
		} `json:"RelatedTopics"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("search response parse failed: %w", err)
	}
	var results []SearchResult
	if out.AbstractText != "" {
		results = append(results, SearchResult{Title: out.Heading, URL: out.AbstractURL, Snippet: out.AbstractText})
	}
	for _, rt := range out.RelatedTopics {
		if rt.Text != "" {
			results = append(results, SearchResult{Title: firstWord(rt.Text), URL: rt.FirstURL, Snippet: rt.Text})
		}
		for _, t := range rt.Topics {
			if t.Text != "" {
				results = append(results, SearchResult{Title: firstWord(t.Text), URL: t.FirstURL, Snippet: t.Text})
			}
		}
		if len(results) >= 5 {
			break
		}
	}
	return results, nil
}

func firstWord(s string) string {
	if idx := strings.Index(s, " - "); idx > 0 {
		return s[:idx]
	}
	if len(s) > 80 {
		return s[:80]
	}
	return s
}

// handleWebSearch runs a search and returns a compact digest for the LLM.
func handleWebSearch(ctx context.Context, e *Engine, botID, sender string, args map[string]any) ai.ToolCallResult {
	if e.Search == nil {
		return textSearchResult("我这边现在还查不了网页。", "我这边现在还查不了网页。", "search", "no search backend configured", 0)
	}
	query := strings.TrimSpace(strArg(args, "query"))
	if query == "" {
		return textSearchResult("你想让我查什么，给我个关键词。", "你想让我查什么，给我个关键词。", "search", "query required", 0)
	}
	results, err := e.Search.Search(ctx, query)
	if err != nil {
		return textSearchResult("我刚查了一下，但这会儿网页那边有点卡，没顺利查到。", "我刚查了一下，但这会儿网页那边有点卡，没顺利查到。", "search", err.Error(), 0)
	}
	if len(results) == 0 {
		return textSearchResult("我刚搜了下，暂时没找到特别靠谱的结果。", "我刚搜了下，暂时没找到特别靠谱的结果。", "", "", 0)
	}
	content, explanation := summarizeSearchResults(results)
	return textSearchResult(content, explanation, "", "", min(len(results), 5))
}

func textSearchResult(content, explanation, errorStage, errorMessage string, resultCount int) ai.ToolCallResult {
	if strings.TrimSpace(explanation) == "" {
		explanation = content
	}
	return ai.ToolCallResult{
		Content: content,
		Delivery: ai.ToolCallDelivery{
			UserFacing:  true,
			Explanation: explanation,
		},
		Diagnostics: ai.ToolCallDiagnostics{
			ErrorStage:   errorStage,
			ErrorMessage: errorMessage,
			LogFields: map[string]string{
				"delivery_kind": "text",
				"result_count":  strconv.Itoa(resultCount),
			},
		},
	}
}

func summarizeSearchResults(results []SearchResult) (string, string) {
	var parts []string
	for _, r := range results {
		text := normalizeSearchConclusionPart(stripSearchCardTitlePrefix(r.Snippet, r.Title))
		if text == "" {
			text = normalizeSearchConclusionPart(r.Title)
		}
		if text == "" || containsString(parts, text) {
			continue
		}
		parts = append(parts, text)
		if len(parts) == 2 {
			break
		}
	}
	if len(parts) == 0 {
		text := "我刚查了下，暂时只看到一点零散信息。"
		return text, text
	}
	explanation := "我刚查了下，" + parts[0]
	if len(parts) == 1 {
		return explanation, explanation
	}
	content := explanation + "。补充看到，" + strings.Join(parts[1:], "；")
	return content, explanation
}

func stripSearchCardTitlePrefix(snippet, title string) string {
	snippet = strings.TrimSpace(snippet)
	title = strings.TrimSpace(title)
	if snippet == "" || title == "" {
		return snippet
	}
	prefix := title + " - "
	if strings.HasPrefix(snippet, prefix) {
		return strings.TrimSpace(snippet[len(prefix):])
	}
	return snippet
}

var searchConclusionURLPattern = regexp.MustCompile(`https?://[^\s\p{Han}，。！？；：（）【】「」《》、]+|www\.[^\s\p{Han}，。！？；：（）【】「」《》、]+`)

func normalizeSearchConclusionPart(s string) string {
	s = searchConclusionURLPattern.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.NewReplacer(
		" ，", "，",
		" 。", "。",
		" ；", "；",
		" ：", "：",
		" ！", "！",
		" ？", "？",
		" ,", ",",
		" .", ".",
		" ;", ";",
		" :", ":",
		" !", "!",
		" ?", "?",
	).Replace(s)
	s = strings.ReplaceAll(s, "：，", "：")
	s = strings.ReplaceAll(s, ":,", ":")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.TrimRight(s, "。！？；;,. ")
	return s
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
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
