package persona

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/openilink/openilink-hub/internal/ai"
	"github.com/openilink/openilink-hub/internal/store"
)

// ImageOptions carries generation options.
type ImageOptions struct {
	Size string // "square" | "landscape" | "portrait"
}

// ImageProvider generates an image from a prompt. The implementation is
// pluggable: the OpenAI-compatible backend is the default; a perchance-backed
// implementation can be swapped in once its request flow is reverse-engineered
// (design §9.1).
type ImageProvider interface {
	Generate(ctx context.Context, prompt, negativePrompt string, opts ImageOptions) ([]byte, error)
}

// defaultNegativePrompt is always merged into the negative prompt (design §9.1).
const defaultNegativePrompt = "bad hands, extra fingers, missing fingers, fused fingers, mutated hands, deformed"
const maxGeneratedImages = 4

// r18Keywords gates only the gen_image tool: a non-owner whose prompt (or
// negative prompt) contains any of these gets a polite decline instead of a
// generated image. Conservative by design; it does not filter chat replies.
var r18Keywords = []string{
	"nude", "naked", "sex", "nsfw", "porn", "pornographic", "erotic", "lewd", "hentai",
	"裸体", "裸照", "色情", "性交", "性爱", "情色", "露骨", "做爱",
}

// containsR18Keyword reports whether either prompt field hits an R18 keyword.
// Both are scanned lowercased so English hits are case-insensitive.
func containsR18Keyword(prompt, negativePrompt string) bool {
	hay := strings.ToLower(prompt + "\n" + negativePrompt)
	for _, kw := range r18Keywords {
		if strings.Contains(hay, kw) {
			return true
		}
	}
	return false
}

// isOwnerOfBot reports whether sender is the bot's unique owner (SetOwner
// guarantees at most one per bot). Store errors fail closed (not owner).
func isOwnerOfBot(e *Engine, botID, sender string) bool {
	contacts, err := e.Store.ListContacts(botID)
	if err != nil {
		return false
	}
	for _, c := range contacts {
		if c.IsOwner {
			return c.Sender == sender
		}
	}
	return false
}

// --- OpenAI-compatible images API (default / fallback) ---

type openaiImageProvider struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewOpenAIImageProvider builds the fallback image provider from the global
// "ai.*" configuration.
func NewOpenAIImageProvider(cfg store.AIConfig) ImageProvider {
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := cfg.Model
	if model == "" {
		model = "gpt-4o-mini"
	}
	return &openaiImageProvider{
		baseURL: strings.TrimRight(base, "/"),
		apiKey:  cfg.APIKey,
		model:   model,
		client:  &http.Client{Timeout: 90 * time.Second},
	}
}

func (p *openaiImageProvider) Generate(ctx context.Context, prompt, negativePrompt string, opts ImageOptions) ([]byte, error) {
	endpoint := p.baseURL + "/images/generations"
	reqBody := map[string]any{
		"model":           p.model,
		"prompt":          prompt,
		"n":               1,
		"response_format": "b64_json",
		"size":            openAISize(opts.Size),
	}
	// Many OpenAI-compatible backends (e.g. SiliconFlow) accept a negative
	// prompt as an extension; providers that don't support it ignore it.
	if strings.TrimSpace(negativePrompt) != "" {
		reqBody["negative_prompt"] = negativePrompt
	}
	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("image api request failed: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 25*1024*1024))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("image api returned %d: %s", resp.StatusCode, truncate(string(data), 500))
	}
	var result struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("image api response parse failed: %s", truncate(string(data), 500))
	}
	if result.Error != nil {
		return nil, fmt.Errorf("image api error: %s", result.Error.Message)
	}
	if len(result.Data) == 0 {
		return nil, fmt.Errorf("image api returned empty data")
	}
	if result.Data[0].B64JSON != "" {
		raw := result.Data[0].B64JSON
		if idx := strings.Index(raw, ","); idx > 0 && strings.HasPrefix(raw, "data:") {
			raw = raw[idx+1:]
		}
		img, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("image api base64 decode failed: %w", err)
		}
		return img, nil
	}
	if result.Data[0].URL != "" {
		return downloadImage(ctx, p.client, result.Data[0].URL)
	}
	return nil, fmt.Errorf("image api returned no image")
}

func openAISize(size string) string {
	switch size {
	case "landscape":
		return "1792x1024"
	case "portrait":
		return "1024x1792"
	default:
		return "1024x1024"
	}
}

func downloadImage(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid image url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("image download returned %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 25*1024*1024))
}

// --- perchance provider (design §9.1) ---
//
// Request flow reverse-engineered from the public perchance text-to-image
// plugin and the community "perchance" Python API (verifyUser → generate →
// downloadTemporaryImage). The wire contract is unofficial and may change with
// perchance's frontend; the OpenAI-compatible provider remains the default and
// gen_image falls back to it on any perchance failure.
//
//   1. GET  {base}/verifyUser?thread=0&__cacheBust=<rand>
//          → page content contains `"userKey":"<key>"` (or "too_many_requests").
//   2. POST {base}/generate?userKey=<key>&requestId=aiImageCompletion<rand>&__cacheBust=<rand>
//          body: {generatorName:"ai-image-generator", channel:"ai-text-to-image-generator",
//                 subChannel:"public", prompt, negativePrompt, seed, resolution, guidanceScale}
//          → {imageId, fileExtension, maybeNsfw, ...}
//   3. GET  {base}/downloadTemporaryImage?imageId=<imageId> → image bytes.

type perchanceImageProvider struct {
	baseURL string
	client  *http.Client
}

// NewPerchanceImageProvider builds the perchance-backed provider.
func NewPerchanceImageProvider() ImageProvider {
	return &perchanceImageProvider{
		baseURL: "https://image-generation.perchance.org/api",
		client:  &http.Client{Timeout: 90 * time.Second},
	}
}

func (p *perchanceImageProvider) Generate(ctx context.Context, prompt, negativePrompt string, opts ImageOptions) ([]byte, error) {
	key, err := p.fetchUserKey(ctx)
	if err != nil {
		return nil, err
	}
	imageID, err := p.submitGenerate(ctx, key, prompt, negativePrompt, opts.Size)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.baseURL+"/downloadTemporaryImage?imageId="+url.QueryEscape(imageID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("perchance image download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("perchance image download returned %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 25*1024*1024))
}

func (p *perchanceImageProvider) fetchUserKey(ctx context.Context) (string, error) {
	bust := fmt.Sprintf("%d", time.Now().UnixNano()%1000000)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.baseURL+"/verifyUser?thread=0&__cacheBust="+bust, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; OpeniLink-Hub/1.0)")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("perchance verifyUser failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	content := string(body)

	if strings.Contains(content, "too_many_requests") {
		return "", fmt.Errorf("perchance rate limit exceeded")
	}
	idx := strings.Index(content, `"userKey":"`)
	if idx < 0 {
		return "", fmt.Errorf("perchance: userKey not found (anti-bot challenge?); falling back")
	}
	rest := content[idx+len(`"userKey":"`):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return "", fmt.Errorf("perchance: malformed userKey")
	}
	key := rest[:end]
	if key == "" {
		return "", fmt.Errorf("perchance: empty userKey")
	}
	return key, nil
}

func (p *perchanceImageProvider) submitGenerate(ctx context.Context, key, prompt, negativePrompt, size string) (string, error) {
	resolution := "768x768"
	switch size {
	case "portrait":
		resolution = "512x768"
	case "landscape":
		resolution = "768x512"
	}
	bust := fmt.Sprintf("%d", time.Now().UnixNano()%1000000)
	requestID := fmt.Sprintf("aiImageCompletion%d", time.Now().UnixNano()%1_000_000_000)
	genURL := fmt.Sprintf("%s/generate?userKey=%s&requestId=%s&__cacheBust=%s",
		p.baseURL, url.QueryEscape(key), requestID, bust)

	body, _ := json.Marshal(map[string]any{
		"generatorName":  "ai-image-generator",
		"channel":        "ai-text-to-image-generator",
		"subChannel":     "public",
		"prompt":         prompt,
		"negativePrompt": negativePrompt,
		"seed":           -1,
		"resolution":     resolution,
		"guidanceScale":  7.0,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, genURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("perchance generate failed: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("perchance generate returned %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var result struct {
		ImageID string `json:"imageId"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("perchance generate parse failed: %s", truncate(string(data), 300))
	}
	if result.Error != "" {
		return "", fmt.Errorf("perchance generate error: %s", result.Error)
	}
	if result.ImageID == "" {
		return "", fmt.Errorf("perchance generate returned no imageId")
	}
	return result.ImageID, nil
}

// imageProvider returns the configured image provider, lazily creating the
// OpenAI-compatible fallback from the current AI config when none was set
// explicitly (so gen_image works as soon as the AI is configured).
func (e *Engine) imageProvider() ImageProvider {
	if e.Images != nil {
		return e.Images
	}
	return e.openAIFallback()
}

// openAIFallback builds the OpenAI-compatible provider from current config,
// or nil when the AI is not configured yet.
func (e *Engine) openAIFallback() ImageProvider {
	cfg := ResolveAIConfig(e.Store)
	if cfg.APIKey == "" {
		return nil
	}
	return NewOpenAIImageProvider(cfg)
}

// handleGenImage generates an image and returns it as a tool image result,
// which the AI sink delivers to the user (and, after the §11.2 change, lets the
// AI add one companion sentence). If a custom provider (e.g. perchance) fails,
// the OpenAI-compatible fallback is tried (design §12: 降级不崩溃).
func handleGenImage(ctx context.Context, e *Engine, botID, sender string, args map[string]any) ai.ToolCallResult {
	provider := e.imageProvider()
	if provider == nil {
		return imageFailureResult("我这边现在还出不了图，你晚点再让我试试。", "generate", "image provider unavailable")
	}
	prompt := strings.TrimSpace(strArg(args, "prompt"))
	if prompt == "" {
		return imageFailureResult("你想让我画什么，给我一句描述我就能开始。", "validate", "prompt required")
	}
	// S3: R18 关键词预检 — 非 owner 命中敏感词（含 negative_prompt）时拒绝，
	// 仅作用于画图工具；owner 放行。
	if !isOwnerOfBot(e, botID, sender) && containsR18Keyword(prompt, strArg(args, "negative_prompt")) {
		return imageFailureResult("这个画面还是不给别人画比较好。", "policy", "r18 prompt blocked for non-owner")
	}
	neg := strings.TrimSpace(strArg(args, "negative_prompt"))
	if neg != "" {
		neg = neg + ", "
	}
	neg += defaultNegativePrompt
	opts := ImageOptions{Size: strArg(args, "size")}
	count := resolveImageCount(args)

	images, err := generateImages(ctx, provider, prompt, neg, opts, count)
	if err != nil && e.Images != nil {
		// 配置的 provider 失效（如 perchance 限流/反爬）→ 降级 OpenAI 兜底。
		originalErr := err
		if fb := e.openAIFallback(); fb != nil {
			if fallbackImages, fbErr := generateImages(ctx, fb, prompt, neg, opts, count); fbErr == nil {
				images = fallbackImages
				err = nil
			} else {
				err = originalErr
			}
		}
	}
	if err != nil {
		return imageFailureResult("我刚刚试着画了下，这会儿没顺利出图。", "generate", err.Error())
	}
	return imageSuccessResult(images)
}

func resolveImageCount(args map[string]any) int {
	count := intArg(args, "count")
	if count <= 0 {
		return 1
	}
	if count > maxGeneratedImages {
		return maxGeneratedImages
	}
	return count
}

func generateImages(ctx context.Context, provider ImageProvider, prompt, negativePrompt string, opts ImageOptions, count int) ([]ai.ImageData, error) {
	images := make([]ai.ImageData, 0, count)
	for i := 0; i < count; i++ {
		img, err := provider.Generate(ctx, prompt, negativePrompt, opts)
		if err != nil {
			return nil, err
		}
		if len(img) == 0 {
			return nil, fmt.Errorf("image provider returned empty image")
		}
		ct := http.DetectContentType(img)
		images = append(images, ai.ImageData{
			Data:        img,
			ContentType: ct,
		})
	}
	return images, nil
}

func imageSuccessResult(images []ai.ImageData) ai.ToolCallResult {
	explanation := "我先把图发你，你看看是不是这个感觉。"
	if len(images) > 1 {
		explanation = "我先给你发几张，你看看更偏哪种感觉。"
	}
	return ai.ToolCallResult{
		Content: "图给你发过去了。",
		Images:  images,
		Delivery: ai.ToolCallDelivery{
			UserFacing:  true,
			Explanation: explanation,
		},
		Diagnostics: ai.ToolCallDiagnostics{
			LogFields: map[string]string{
				"delivery_kind": "image",
				"result_count":  itoa(len(images)),
			},
		},
	}
}

func imageFailureResult(content, errorStage, errorMessage string) ai.ToolCallResult {
	return ai.ToolCallResult{
		Content: content,
		Delivery: ai.ToolCallDelivery{
			UserFacing:  true,
			Explanation: content,
		},
		Diagnostics: ai.ToolCallDiagnostics{
			ErrorStage:   errorStage,
			ErrorMessage: errorMessage,
			LogFields: map[string]string{
				"delivery_kind": "image",
				"result_count":  "0",
			},
		},
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
