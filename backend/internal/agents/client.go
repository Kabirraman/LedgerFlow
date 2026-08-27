// Package agents implements LEDGERFLOW's four-agent layer (SRS 7, 8).
//
// Two rules shape every file here:
//
//  1. Model output is an untrusted recommendation. It is parsed into a closed
//     typed struct, every enum is checked against an allow-list, and every
//     monetary figure is recomputed from trusted database facts rather than read
//     from generated text (SRS 19.2).
//  2. The layer fails closed. A timeout, an unparseable response or a
//     low-confidence answer produces a deterministic fallback, NO_ACTION or
//     ESCALATE — never an unguarded action (SRS 20.4).
package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// Client calls the Gemini generateContent endpoint and returns raw JSON text.
type Client interface {
	// Generate returns the model's JSON response for a prompt, constrained to
	// responseSchema. Name identifies the model for audit records.
	Generate(ctx context.Context, systemPrompt, userPrompt string, responseSchema any) ([]byte, error)
	// Name is the configured model identifier.
	Name() string
	// Enabled reports whether a real model is reachable. When false, callers go
	// straight to the deterministic path instead of burning a timeout per case.
	Enabled() bool
}

// GeminiClient is the HTTP implementation.
type GeminiClient struct {
	http        *http.Client
	apiKey      string
	model       string
	baseURL     string
	temperature float64
	maxTokens   int
	maxRetries  int
	observer    func(model string, latency time.Duration, err error)
}

// GeminiConfig configures the client.
type GeminiConfig struct {
	APIKey      string
	Model       string
	BaseURL     string
	Temperature float64
	MaxTokens   int
	MaxRetries  int
	Timeout     time.Duration
}

// NewGeminiClient builds a client. An empty API key yields a disabled client
// rather than an error: the system is required to run end-to-end without AI
// credentials, using the deterministic path (SRS 20.4, 24.3).
func NewGeminiClient(cfg GeminiConfig) *GeminiClient {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	model := cfg.Model
	if model == "" {
		model = "gemini-2.0-flash"
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "https://generativelanguage.googleapis.com/v1beta"
	}
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	return &GeminiClient{
		http:        &http.Client{Timeout: timeout},
		apiKey:      cfg.APIKey,
		model:       model,
		baseURL:     base,
		temperature: cfg.Temperature,
		maxTokens:   maxTokens,
		maxRetries:  cfg.MaxRetries,
	}
}

// Name returns the model identifier.
func (c *GeminiClient) Name() string { return c.model }

// Enabled reports whether an API key is configured.
func (c *GeminiClient) Enabled() bool { return c != nil && c.apiKey != "" }

// SetObserver registers a latency/error hook for operational counters.
func (c *GeminiClient) SetObserver(fn func(model string, latency time.Duration, err error)) {
	c.observer = fn
}

type geminiRequest struct {
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	Contents          []geminiContent   `json:"contents"`
	GenerationConfig  geminiGenerConfig `json:"generationConfig"`
	SafetySettings    []geminiSafety    `json:"safetySettings,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenerConfig struct {
	Temperature      float64 `json:"temperature"`
	MaxOutputTokens  int     `json:"maxOutputTokens"`
	ResponseMIMEType string  `json:"responseMimeType"`
	ResponseSchema   any     `json:"responseSchema,omitempty"`
	CandidateCount   int     `json:"candidateCount"`
}

type geminiSafety struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// ErrModelDisabled means no API key is configured.
var ErrModelDisabled = errors.New("agents: gemini client disabled (no API key)")

// ErrEmptyResponse means the model returned no usable candidate.
var ErrEmptyResponse = errors.New("agents: model returned no content")

// Generate calls generateContent with JSON mode and a response schema.
//
// responseMimeType plus responseSchema make the model emit schema-shaped JSON,
// which removes the most common failure mode (prose wrapped around JSON). It is
// still validated on our side — a constrained decoder is a convenience, not a
// guarantee.
func (c *GeminiClient) Generate(ctx context.Context, systemPrompt, userPrompt string, responseSchema any) ([]byte, error) {
	if !c.Enabled() {
		return nil, ErrModelDisabled
	}

	reqBody := geminiRequest{
		Contents: []geminiContent{{Role: "user", Parts: []geminiPart{{Text: userPrompt}}}},
		GenerationConfig: geminiGenerConfig{
			Temperature:      c.temperature,
			MaxOutputTokens:  c.maxTokens,
			ResponseMIMEType: "application/json",
			ResponseSchema:   responseSchema,
			CandidateCount:   1,
		},
	}
	if systemPrompt != "" {
		reqBody.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: systemPrompt}}}
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("agents: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/models/%s:generateContent", c.baseURL, c.model)
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			wait := time.Duration(300*(1<<(attempt-1))) * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		out, err := c.doOnce(ctx, url, payload)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !retryableModelError(err) {
			break
		}
	}
	return nil, lastErr
}

func (c *GeminiClient) doOnce(ctx context.Context, url string, payload []byte) ([]byte, error) {
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("agents: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The key travels in a header rather than the query string so it does not
	// land in intermediary access logs.
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		c.observe(started, err)
		return nil, &ModelError{Kind: ModelErrorTransport, Message: err.Error()}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		c.observe(started, err)
		return nil, &ModelError{Kind: ModelErrorTransport, Message: err.Error()}
	}

	var parsed geminiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		modelErr := &ModelError{Kind: ModelErrorInvalidJSON, Message: "response envelope is not JSON"}
		c.observe(started, modelErr)
		return nil, modelErr
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		kind := ModelErrorAPI
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			kind = ModelErrorTransport
		}
		modelErr := &ModelError{
			Kind:       kind,
			StatusCode: resp.StatusCode,
			Message:    firstNonEmpty(parsed.Error.Message, parsed.Error.Status, resp.Status),
		}
		c.observe(started, modelErr)
		return nil, modelErr
	}

	if parsed.PromptFeedback.BlockReason != "" {
		modelErr := &ModelError{Kind: ModelErrorBlocked, Message: parsed.PromptFeedback.BlockReason}
		c.observe(started, modelErr)
		return nil, modelErr
	}

	var text strings.Builder
	for _, cand := range parsed.Candidates {
		for _, part := range cand.Content.Parts {
			text.WriteString(part.Text)
		}
		break
	}
	out := strings.TrimSpace(text.String())
	if out == "" {
		c.observe(started, ErrEmptyResponse)
		return nil, &ModelError{Kind: ModelErrorInvalidJSON, Message: ErrEmptyResponse.Error()}
	}
	c.observe(started, nil)
	return []byte(out), nil
}

func (c *GeminiClient) observe(started time.Time, err error) {
	if c.observer != nil {
		c.observer(c.model, time.Since(started), err)
	}
}

// ModelErrorKind classifies a model failure so the caller can distinguish
// "retry might help" from "fall back now".
type ModelErrorKind string

const (
	ModelErrorTransport   ModelErrorKind = "transport"
	ModelErrorAPI         ModelErrorKind = "api"
	ModelErrorInvalidJSON ModelErrorKind = "invalid_json"
	ModelErrorBlocked     ModelErrorKind = "blocked"
	ModelErrorSchema      ModelErrorKind = "schema"
)

// ModelError is a classified model failure.
type ModelError struct {
	Kind       ModelErrorKind
	StatusCode int
	Message    string
}

func (e *ModelError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("agents: model %s error (%d): %s", e.Kind, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("agents: model %s error: %s", e.Kind, e.Message)
}

// Unwrap maps every model failure onto ErrAgentUnavailable, so callers can
// treat "the AI did not give us a usable answer" as one condition regardless of
// cause and route to the deterministic path (SRS 20.4).
func (e *ModelError) Unwrap() error { return domain.ErrAgentUnavailable }

func retryableModelError(err error) bool {
	var me *ModelError
	if errors.As(err, &me) {
		return me.Kind == ModelErrorTransport
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// --- response schema helpers ---

// schema builds a Gemini responseSchema fragment. Gemini uses an OpenAPI
// subset, so only type/properties/required/enum/items are used here.
type schema map[string]any

func objectSchema(required []string, props map[string]any) schema {
	return schema{"type": "object", "properties": props, "required": required}
}

func stringSchema() schema              { return schema{"type": "string"} }
func numberSchema() schema              { return schema{"type": "number"} }
func boolSchema() schema                { return schema{"type": "boolean"} }
func integerSchema() schema             { return schema{"type": "integer"} }
func enumSchema(values []string) schema { return schema{"type": "string", "enum": values} }
func arraySchema(items schema) schema   { return schema{"type": "array", "items": items} }
func stringArraySchema() schema         { return arraySchema(stringSchema()) }

// decodeStrict unmarshals model output into v, rejecting unknown fields.
//
// Rejecting extras is deliberate: a response carrying fields we do not model is
// a response we do not fully understand, and this layer authorises money
// movement downstream.
func decodeStrict(raw []byte, v any) error {
	// Every response schema here is an object, and encoding/json treats a bare
	// `null` as a successful no-op decode. Without this check a model replying
	// "null" would be recorded as a clean AI answer carrying an all-zero struct,
	// so the document shape is checked before the decoder runs.
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return &ModelError{Kind: ModelErrorSchema, Message: "response is not a JSON object"}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &ModelError{Kind: ModelErrorSchema, Message: err.Error()}
	}
	// Trailing content means the model emitted more than one document.
	if dec.More() {
		return &ModelError{Kind: ModelErrorSchema, Message: "trailing content after JSON object"}
	}
	return nil
}
