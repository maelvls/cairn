package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// AI runtime: artifacts call Claude through the server (cairn.ai in
// cairn.js), the way Claude Artifacts pages use window.claude. The server's
// Anthropic API key never reaches the browser; only signed-in users can spend
// it (anonymous visitors of public artifacts get a 401), each within a daily
// request quota.

const (
	anthropicBaseURL = "https://api.anthropic.com"
	// anthropicVersion is the Messages API version header. 2026-10-21 rejects
	// temperature/top_p/top_k, which this proxy never sends.
	anthropicVersion = "2026-10-21"
	// defaultAIModel is the latest Sonnet: fast and capable for page-level
	// calls (summaries, rewrites, chat).
	defaultAIModel = "claude-sonnet-5-5"

	aiMaxBody       = 200 << 10 // request body cap for POST /api/artifacts/{id}/ai
	aiMaxResponse   = 10 << 20  // upstream response cap
	aiTimeout       = 120 * time.Second
	aiMaxMessages   = 200
	fallbackBetaHdr = "server-side-fallback-2026-07-01"
)

// aiExtraModels are the models a page may pick with "model" besides the
// configured default. Anything else is a 400: the operator chose the default,
// pages only get to trade it for these current ones.
var aiExtraModels = []string{"claude-sonnet-5-5", "claude-opus-5-5", "claude-haiku-5-5"}

// aiFallbackModels accept the server-side `fallbacks: "default"` parameter:
// a request their safety classifiers decline is retried on Anthropic's
// recommended model instead of coming back as a refusal. Other models
// (Haiku 5.5, older ones) reject or ignore it, so it is not sent to them.
var aiFallbackModels = map[string]bool{
	"claude-sonnet-5-5": true,
	"claude-opus-5-5":   true,
	"claude-opus-5":     true,
	"claude-fable-5-1":  true,
}

// aiQuota counts AI requests per user per UTC day, in memory: a restart
// resets it, which is acceptable for a spending guard on a small server.
type aiQuota struct {
	mu   sync.Mutex
	day  string
	used map[string]int
}

func utcDay() string { return time.Now().UTC().Format("2006-01-02") }

// take reserves one request for user and returns the day it was charged to;
// ok is false when the limit is reached (limit <= 0 means unlimited).
func (q *aiQuota) take(user string, limit int) (day string, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.roll()
	if limit > 0 && q.used[user] >= limit {
		return q.day, false
	}
	q.used[user]++
	return q.day, true
}

// refund gives back a reservation for a request Anthropic answered with an
// error (nothing generated). Aborted or timed-out calls are not refunded:
// the generation may have been billed, and a page must not be able to dodge
// the quota by cancelling its requests.
func (q *aiQuota) refund(user string, day string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.day == day && q.used[user] > 0 {
		q.used[user]--
	}
}

func (q *aiQuota) usedBy(user string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.roll()
	return q.used[user]
}

// roll starts a fresh count when the UTC day changes. Callers hold mu.
func (q *aiQuota) roll() {
	if d := utcDay(); d != q.day || q.used == nil {
		q.day = d
		q.used = map[string]int{}
	}
}

func (s *Server) aiEnabled() bool { return s.cfg.AnthropicAPIKey != "" }

// aiModels lists the models a page may request, default first.
func (s *Server) aiModels() []string {
	models := []string{s.cfg.AIModel}
	for _, m := range aiExtraModels {
		if m != s.cfg.AIModel {
			models = append(models, m)
		}
	}
	return models
}

func (s *Server) aiRemaining(userID string) int {
	if s.cfg.AIDailyLimit <= 0 {
		return -1
	}
	return max(0, s.cfg.AIDailyLimit-s.ai.usedBy(userID))
}

type aiStatus struct {
	Enabled    bool     `json:"enabled"`
	Model      string   `json:"model"`
	Models     []string `json:"models"`
	MaxTokens  int      `json:"maxTokens"`
	DailyLimit int      `json:"dailyLimit"` // 0 = unlimited
	Remaining  int      `json:"remaining"`  // -1 = unlimited
}

// handleAIStatus serves GET /api/me/ai: whether the AI runtime is configured
// and how many requests the caller has left today. A server without a key
// answers just {enabled: false}.
func (s *Server) handleAIStatus(w http.ResponseWriter, r *http.Request) {
	if !s.aiEnabled() {
		writeJSON(w, http.StatusOK, map[string]bool{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, aiStatus{
		Enabled:    true,
		Model:      s.cfg.AIModel,
		Models:     s.aiModels(),
		MaxTokens:  s.cfg.AIMaxTokens,
		DailyLimit: max(0, s.cfg.AIDailyLimit),
		Remaining:  s.aiRemaining(requestUser(r).ID),
	})
}

type aiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type aiRequest struct {
	Prompt    *string     `json:"prompt"`
	Messages  []aiMessage `json:"messages"`
	System    string      `json:"system"`
	MaxTokens int         `json:"max_tokens"`
	Model     string      `json:"model"`
}

// anthropicRequest is the Messages API body this proxy sends: nothing
// beyond what the page asked for, the model and the token cap.
type anthropicRequest struct {
	Model     string      `json:"model"`
	MaxTokens int         `json:"max_tokens"`
	System    string      `json:"system,omitempty"`
	Messages  []aiMessage `json:"messages"`
	Fallbacks string      `json:"fallbacks,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicResponse struct {
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      anthropicUsage `json:"usage"`
}

type anthropicError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type aiResponse struct {
	Text       string         `json:"text"`
	Model      string         `json:"model"`
	Usage      anthropicUsage `json:"usage"`
	StopReason string         `json:"stop_reason"`
}

// validateAIContent accepts a message content the Messages API understands:
// a non-empty string, or a non-empty array of content blocks of the types a
// page can sensibly send (text, image, document).
func validateAIContent(raw json.RawMessage) error {
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if strings.TrimSpace(str) == "" {
			return errors.New("content is empty")
		}
		return nil
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return errors.New("content must be a string or an array of content blocks")
	}
	if len(blocks) == 0 {
		return errors.New("content is empty")
	}
	for _, b := range blocks {
		switch b.Type {
		case "text", "image", "document":
		default:
			return fmt.Errorf("unsupported content block type %q (text, image or document)", b.Type)
		}
	}
	return nil
}

// buildAIRequest validates the page's body and maps it to a Messages API
// request. The error message is safe to return to the caller.
func (s *Server) buildAIRequest(in aiRequest) (anthropicRequest, error) {
	out := anthropicRequest{Model: s.cfg.AIModel, System: in.System}
	if in.Model != "" {
		if !slices.Contains(s.aiModels(), in.Model) {
			return out, fmt.Errorf("model %q is not allowed (one of %s)", in.Model, strings.Join(s.aiModels(), ", "))
		}
		out.Model = in.Model
	}
	switch {
	case in.Prompt != nil && in.Messages != nil:
		return out, errors.New("send either prompt or messages, not both")
	case in.Prompt != nil:
		if strings.TrimSpace(*in.Prompt) == "" {
			return out, errors.New("prompt is empty")
		}
		content, _ := json.Marshal(*in.Prompt)
		out.Messages = []aiMessage{{Role: "user", Content: content}}
	case len(in.Messages) > 0:
		if len(in.Messages) > aiMaxMessages {
			return out, fmt.Errorf("too many messages (max %d)", aiMaxMessages)
		}
		for i, m := range in.Messages {
			if m.Role != "user" && m.Role != "assistant" {
				return out, fmt.Errorf("messages[%d]: role must be user or assistant", i)
			}
			if err := validateAIContent(m.Content); err != nil {
				return out, fmt.Errorf("messages[%d]: %v", i, err)
			}
		}
		if in.Messages[0].Role != "user" {
			return out, errors.New("messages[0] must be a user message")
		}
		out.Messages = in.Messages
	default:
		return out, errors.New("prompt or messages is required")
	}
	out.MaxTokens = s.cfg.AIMaxTokens
	if in.MaxTokens > 0 && in.MaxTokens < out.MaxTokens {
		out.MaxTokens = in.MaxTokens
	}
	if aiFallbackModels[out.Model] {
		out.Fallbacks = "default"
	}
	return out, nil
}

// handleAI serves POST /api/artifacts/{id}/ai: one Messages API call with the
// server's key, answered as {text, model, usage, stop_reason}.
func (s *Server) handleAI(w http.ResponseWriter, r *http.Request) {
	if !s.aiEnabled() {
		writeError(w, http.StatusServiceUnavailable, "AI is not configured on this server")
		return
	}
	u, a := requestUser(r), requestArtifact(r)

	var in aiRequest
	r.Body = http.MaxBytesReader(w, r.Body, aiMaxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // a misspelled option must not be silently dropped
	if err := dec.Decode(&in); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body too large (max %d KB)", aiMaxBody>>10))
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	req, err := s.buildAIRequest(in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	day, ok := s.ai.take(u.ID, s.cfg.AIDailyLimit)
	if !ok {
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("daily AI limit reached (%d requests per day); it resets at midnight UTC", s.cfg.AIDailyLimit))
		return
	}
	resp, status, err := s.callAnthropic(r.Context(), req)
	if err != nil {
		if r.Context().Err() != nil {
			// The page went away (navigation, AbortController): nobody to
			// answer, and no refund (see aiQuota.refund).
			s.log.Info("ai request aborted by the client", "user", u.Email, "artifact", a.ID, "model", req.Model)
			return
		}
		if status != 0 {
			s.ai.refund(u.ID, day) // Anthropic answered with an error
		}
		s.log.Warn("ai request failed", "user", u.Email, "artifact", a.ID, "model", req.Model, "status", status, "err", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	var text strings.Builder
	for _, b := range resp.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	s.log.Info("ai request", "user", u.Email, "artifact", a.ID, "model", resp.Model,
		"input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens, "stop_reason", resp.StopReason)
	writeJSON(w, http.StatusOK, aiResponse{
		Text:       text.String(),
		Model:      resp.Model,
		Usage:      resp.Usage,
		StopReason: resp.StopReason,
	})
}

// callAnthropic sends one non-streaming Messages API request. Errors carry
// the upstream status and message, scrubbed of the API key, and are safe to
// show to the caller.
func (s *Server) callAnthropic(ctx context.Context, req anthropicRequest) (*anthropicResponse, int, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, 0, err
	}
	timeout := aiCallTimeout(req.MaxTokens)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.anthropicURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, s.scrubKey(err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("x-api-key", s.cfg.AnthropicAPIKey)
	hreq.Header.Set("anthropic-version", anthropicVersion)
	if req.Fallbacks != "" {
		hreq.Header.Set("anthropic-beta", fallbackBetaHdr)
	}
	hresp, err := s.aiClient.Do(hreq)
	if err != nil {
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return nil, 0, fmt.Errorf("Anthropic API did not answer within %s", timeout)
		}
		return nil, 0, fmt.Errorf("Anthropic API unreachable: %w", s.scrubKey(err))
	}
	defer hresp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(hresp.Body, aiMaxResponse))
	if err != nil {
		return nil, hresp.StatusCode, fmt.Errorf("Anthropic API: reading response: %w", s.scrubKey(err))
	}
	if hresp.StatusCode != http.StatusOK {
		var e anthropicError
		msg := http.StatusText(hresp.StatusCode)
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
			if e.Error.Type != "" {
				msg = e.Error.Type + ": " + msg
			}
		}
		return nil, hresp.StatusCode, s.scrubKey(fmt.Errorf("Anthropic API error (HTTP %d): %s", hresp.StatusCode, msg))
	}
	var out anthropicResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, hresp.StatusCode, fmt.Errorf("Anthropic API: invalid response: %v", err)
	}
	return &out, hresp.StatusCode, nil
}

// aiCallTimeout bounds one non-streaming call: aiTimeout, stretched for
// large token caps (about 40 output tokens per second at worst) so a long
// answer the operator allowed is not cut off after it was billed.
func aiCallTimeout(maxTokens int) time.Duration {
	return max(aiTimeout, time.Duration(maxTokens/40)*time.Second)
}

// scrubKey removes the API key from an error message, should anything
// upstream ever echo it.
func (s *Server) scrubKey(err error) error {
	if err == nil || s.cfg.AnthropicAPIKey == "" || !strings.Contains(err.Error(), s.cfg.AnthropicAPIKey) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), s.cfg.AnthropicAPIKey, "[redacted]"))
}
