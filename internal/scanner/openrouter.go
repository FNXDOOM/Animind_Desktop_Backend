package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"animind-backend-go/internal/config"
)

// OpenRouter is the optional last-resort parser for filenames no deterministic rule
// matched; a guess is only accepted above a confidence floor, or it corrupts the catalog.

const openRouterEndpoint = "https://openrouter.ai/api/v1/chat/completions"

// Guess is one LLM parse of a path.
type Guess struct {
	Title       *string
	Season      *int
	Episode     *float64
	ContentType ContentType
	Confidence  float64
	Reason      string
}

// Input is what the model is asked about.
type Input struct {
	RelativePath       string
	SiblingFileNames   []string
	DeterministicGuess *Guess
}

// OpenRouterClient calls the model behind OpenRouter's chat completions API.
type OpenRouterClient struct {
	cfg    *config.Config
	http   *http.Client
	logger *slog.Logger

	mu          sync.Mutex
	warnedNoKey bool
}

// NewOpenRouter builds the client.
func NewOpenRouter(cfg *config.Config, logger *slog.Logger) *OpenRouterClient {
	return &OpenRouterClient{
		cfg:    cfg,
		http:   &http.Client{Timeout: cfg.Metadata.OpenRouter.Timeout},
		logger: logger,
	}
}

// responseSchema is a strict JSON schema, so the model's answer needs no parse-then-guess step.
var responseSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"title", "season", "episode", "contentType", "confidence", "reason"},
	"properties": map[string]any{
		"title":       map[string]any{"type": []string{"string", "null"}},
		"season":      map[string]any{"type": []string{"integer", "null"}},
		"episode":     map[string]any{"type": []string{"number", "null"}},
		"contentType": map[string]any{"type": "string", "enum": []string{"tv", "ova", "ona", "special", "movie", "unknown"}},
		"confidence":  map[string]any{"type": "number"},
		"reason":      map[string]any{"type": "string"},
	},
}

const systemPrompt = "You are a strict anime file scanner. You extract structured metadata from noisy anime release paths. Return JSON that matches the schema."

// Infer parses a path with the model, returning nil when disabled, on failure, or below
// the configured confidence floor.
func (c *OpenRouterClient) Infer(ctx context.Context, input Input) *Guess {
	if !c.cfg.Metadata.OpenRouter.Enabled {
		return nil
	}
	if c.cfg.Metadata.OpenRouter.APIKey == "" {
		c.mu.Lock()
		if !c.warnedNoKey {
			c.warnedNoKey = true
			c.logger.Warn("OPENROUTER_API_KEY is not set; scanner LLM fallback disabled")
		}
		c.mu.Unlock()
		return nil
	}

	payload := map[string]any{
		"model":       c.cfg.Metadata.OpenRouter.Model,
		"temperature": 0,
		"max_tokens":  250,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": buildPrompt(input)},
		},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "anime_scan_guess",
				"strict": true,
				"schema": responseSchema,
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Metadata.OpenRouter.APIKey)
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.Metadata.OpenRouter.SiteURL != "" {
		req.Header.Set("HTTP-Referer", c.cfg.Metadata.OpenRouter.SiteURL)
	}
	if c.cfg.Metadata.OpenRouter.AppName != "" {
		req.Header.Set("X-Title", c.cfg.Metadata.OpenRouter.AppName)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.logger.Warn("scanner LLM inference failed", slog.String("error", err.Error()))
		return nil
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		c.logger.Warn("scanner LLM inference rejected",
			slog.Int("status", resp.StatusCode), slog.String("detail", truncate(string(raw), 200)))
		return nil
	}

	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &completion); err != nil || len(completion.Choices) == 0 {
		return nil
	}

	var parsed struct {
		Title       *string  `json:"title"`
		Season      *float64 `json:"season"`
		Episode     *float64 `json:"episode"`
		ContentType string   `json:"contentType"`
		Confidence  float64  `json:"confidence"`
		Reason      string   `json:"reason"`
	}
	content := strings.TrimSpace(completion.Choices[0].Message.Content)
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		// Some deployments wrap the object in prose; recover the braces before giving up.
		start := strings.Index(content, "{")
		end := strings.LastIndex(content, "}")
		if start < 0 || end <= start || json.Unmarshal([]byte(content[start:end+1]), &parsed) != nil {
			return nil
		}
	}

	guess := &Guess{ContentType: normalizeContentType(parsed.ContentType), Reason: parsed.Reason}
	guess.Confidence = math.Max(0, math.Min(1, parsed.Confidence))
	if parsed.Title != nil {
		if trimmed := strings.TrimSpace(*parsed.Title); trimmed != "" {
			guess.Title = &trimmed
		}
	}
	guess.Season = positiveInt(parsed.Season)
	guess.Episode = nonNegativeNumber(parsed.Episode)
	return guess
}

// buildPrompt renders the question; sibling names give the model the season's episode
// range, which disambiguates "01" from "10".
func buildPrompt(input Input) string {
	siblings := input.SiblingFileNames
	if len(siblings) > 40 {
		siblings = siblings[:40]
	}
	deterministic := any(nil)
	if input.DeterministicGuess != nil {
		deterministic = map[string]any{
			"title":       input.DeterministicGuess.Title,
			"episode":     input.DeterministicGuess.Episode,
			"season":      input.DeterministicGuess.Season,
			"contentType": string(input.DeterministicGuess.ContentType),
		}
	}
	payload := map[string]any{
		"task": "Extract show title, season number, episode number and content type from this media file path.",
		"instructions": []string{
			"Use the outermost folder as the show title unless it is a season marker.",
			"Treat a leading release-group tag in square brackets as noise.",
			"Prefer an explicit SxxEyy or Episode NN marker over a trailing number.",
			"Return null when a field is genuinely unknown instead of guessing.",
			"contentType must be one of tv, ova, ona, special, movie, unknown.",
			"confidence is your self-assessed probability between 0 and 1.",
		},
		"relativePath":       input.RelativePath,
		"siblingFileNames":   siblings,
		"deterministicGuess": deterministic,
	}
	out, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"relativePath":%q}`, input.RelativePath)
	}
	return string(out)
}

func normalizeContentType(raw string) ContentType {
	switch ContentType(strings.ToLower(strings.TrimSpace(raw))) {
	case ContentTypeTV, ContentTypeOVA, ContentTypeONA, ContentTypeSpecial, ContentTypeMovie:
		return ContentType(strings.ToLower(strings.TrimSpace(raw)))
	default:
		return ContentTypeUnknown
	}
}

func positiveInt(value *float64) *int {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		return nil
	}
	rounded := int(math.Floor(*value))
	return &rounded
}

func nonNegativeNumber(value *float64) *float64 {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		return nil
	}
	return value
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var _ = time.Second
