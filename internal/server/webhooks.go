package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sort"
	"sync"
	"time"
)

// MCP events delivery (OpenAI's MCP events spec, webhook flavor).
// ----------------------------------------------------------------
// Subscribers register through the MCP endpoint (events/subscribe). We keep
// their subscription in memory, consume the EventBus with a long-lived
// listener, and POST each matching event to the callback URL — signed with
// Standard Webhooks HMAC-SHA256 using a per-subscription secret.
//
// Mapping between cairn events and MCP event names:
//   bus "comment.created"  → MCP event "comment.created"
//   bus "comment.resolved" → MCP event "comment.resolved"
//   bus "comment.reopened" → MCP event "comment.reopened"
//   bus "comment.updated"  → MCP event "comment.updated"
//
// A subscription filters on artifactId (optional) and on the event name it
// was registered for. Subscriptions are not persisted across a restart —
// clients refresh at least once per ttl, so they re-register on the next
// refresh. Keeping it in memory matches the single-node scale of cairn and
// avoids a schema migration for a first cut; moving to the store is a one-
// hour change later.

// mcpEventTTL is how long a subscription stays valid without a refresh.
// Clients call events/subscribe again before refreshBefore to extend it.
const mcpEventTTL = 1 * time.Hour

// mcpWebhookTimeout bounds every delivery attempt and every verification
// POST independently.
const mcpWebhookTimeout = 10 * time.Second

// mcpMaxBody caps the event payload at 256 KiB, like the OpenAI spec.
const mcpMaxBody = 256 * 1024

// mcpEvents enumerates the events the server advertises in events/list and
// accepts in events/subscribe. Adding a new one is a matter of publishing
// on the bus with the matching name and listing it here.
var mcpEvents = []mcpEventDef{
	{
		name:        "comment.created",
		description: "A new comment or reply was posted on an artifact.",
	},
	{
		name:        "comment.resolved",
		description: "A comment thread was marked resolved.",
	},
	{
		name:        "comment.reopened",
		description: "A resolved thread was reopened.",
	},
	{
		name:        "comment.updated",
		description: "A comment's body was edited.",
	},
}

type mcpEventDef struct {
	name        string
	description string
}

// webhookSubscription is one active MCP events/subscribe registration.
type webhookSubscription struct {
	id          string
	ownerID     string            // user who registered it
	eventName   string            // one of mcpEvents
	args        map[string]any    // canonical filter (today: artifactId)
	callbackURL string
	secret      string            // whsec_<base64>
	createdAt   time.Time
	expiresAt   time.Time
}

// webhookDeliverer owns the subscription table and the goroutine that
// routes bus events to callback URLs.
type webhookDeliverer struct {
	s *Server

	mu   sync.RWMutex
	subs map[string]*webhookSubscription

	httpClient *http.Client

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

func newWebhookDeliverer(s *Server) *webhookDeliverer {
	return &webhookDeliverer{
		s:          s,
		subs:       map[string]*webhookSubscription{},
		httpClient: &http.Client{Timeout: mcpWebhookTimeout},
		stopCh:     make(chan struct{}),
	}
}

// start subscribes to the bus and spawns the delivery goroutine. Safe to
// call once per server lifetime.
func (d *webhookDeliverer) start() {
	if d.s == nil || d.s.bus == nil {
		return
	}
	ch, cancel := d.s.bus.Subscribe(allEventsTopic)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer cancel()
		for {
			select {
			case <-d.stopCh:
				return
			case e, ok := <-ch:
				if !ok {
					return
				}
				d.dispatch(e)
			}
		}
	}()
}

// stop ends the delivery goroutine and waits for any in-flight POST to
// finish (or hit its per-call timeout).
func (d *webhookDeliverer) stop() {
	d.stopOnce.Do(func() { close(d.stopCh) })
	d.wg.Wait()
}

// dispatch sends e to every subscription whose event name and args match.
func (d *webhookDeliverer) dispatch(e Event) {
	d.mu.RLock()
	now := time.Now()
	targets := make([]*webhookSubscription, 0, len(d.subs))
	for _, sub := range d.subs {
		if sub.eventName != e.Name {
			continue
		}
		if sub.expiresAt.Before(now) {
			continue
		}
		if !matchArgs(sub.args, e) {
			continue
		}
		targets = append(targets, sub)
	}
	d.mu.RUnlock()

	for _, sub := range targets {
		sub := sub
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			if err := d.deliver(sub, e); err != nil {
				d.s.log.Warn("mcp webhook delivery", "sub", sub.id, "event", e.Name, "url", sub.callbackURL, "err", err)
			}
		}()
	}
}

// matchArgs reports whether sub's arguments filter accepts e. Today the
// only filter is artifactId; an empty filter matches every artifact.
func matchArgs(args map[string]any, e Event) bool {
	if v, ok := args["artifactId"]; ok {
		if s, ok := v.(string); ok && s != "" && s != e.ArtifactID {
			return false
		}
	}
	return true
}

// deliver signs the event and POSTs it to sub.callbackURL. Returns an error
// on non-2xx responses or transport failures; the caller logs.
func (d *webhookDeliverer) deliver(sub *webhookSubscription, e Event) error {
	payload, err := eventWebhookBody(e)
	if err != nil {
		return err
	}
	if len(payload) > mcpMaxBody {
		return fmt.Errorf("payload %d bytes exceeds the %d cap", len(payload), mcpMaxBody)
	}
	req, err := signedWebhookRequest(sub, e.ID, payload)
	if err != nil {
		return err
	}
	req.Header.Set("X-MCP-Subscription-Id", sub.id)
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// eventWebhookBody builds the JSON body MCP events deliveries carry.
func eventWebhookBody(e Event) ([]byte, error) {
	env := map[string]any{
		"eventId":   e.ID,
		"name":      e.Name,
		"timestamp": e.OccurredAt.UTC().Format(time.RFC3339Nano),
		"data":      json.RawMessage(e.Data),
	}
	return json.Marshal(env)
}

// signedWebhookRequest constructs the POST request with Standard Webhooks
// headers. The signature covers (eventId, timestamp, body bytes) so retries
// need a fresh timestamp and signature.
func signedWebhookRequest(sub *webhookSubscription, eventID string, body []byte) (*http.Request, error) {
	key, err := decodeSecret(sub.secret)
	if err != nil {
		return nil, err
	}
	ts := strconvI64(time.Now().Unix())
	toSign := []byte(eventID + "." + ts + ".")
	toSign = append(toSign, body...)
	mac := hmac.New(sha256.New, key)
	mac.Write(toSign)
	sig := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequest("POST", sub.callbackURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", eventID)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", sig)
	return req, nil
}

// decodeSecret parses the whsec_ prefix and returns the raw signing key.
// The spec requires a base64 value that decodes to 24–64 bytes.
func decodeSecret(secret string) ([]byte, error) {
	v, ok := strings.CutPrefix(secret, "whsec_")
	if !ok {
		return nil, errors.New("secret must start with whsec_")
	}
	key, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(v)
	}
	if err != nil {
		return nil, fmt.Errorf("secret is not base64: %w", err)
	}
	if len(key) < 24 || len(key) > 64 {
		return nil, fmt.Errorf("decoded secret is %d bytes, expected 24–64", len(key))
	}
	return key, nil
}

// subscribe registers a new subscription (or refreshes an existing one with
// the same owner + event + args + callback). A verification challenge is
// POSTed to the callback URL and must come back echoed before the
// subscription is stored.
type subscribeInput struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Delivery  struct {
		Mode   string `json:"mode"`
		URL    string `json:"url"`
		Secret string `json:"secret"`
	} `json:"delivery"`
	TTLMs *int64 `json:"ttlMs,omitempty"`
}

type subscribeOutput struct {
	ID             string `json:"id"`
	RefreshBefore  string `json:"refreshBefore,omitempty"`
	Cursor         *struct{} `json:"cursor"` // null per the spec: not replayable
	Truncated      bool   `json:"truncated"`
}

var errUnknownEvent = errors.New("unknown event name")

func (d *webhookDeliverer) subscribe(ctx context.Context, ownerID string, in subscribeInput) (*subscribeOutput, int, error) {
	def := mcpEventByName(in.Name)
	if def == nil {
		return nil, -32602, fmt.Errorf("%w: %q", errUnknownEvent, in.Name)
	}
	if in.Delivery.Mode != "webhook" {
		return nil, -32602, fmt.Errorf("delivery.mode must be \"webhook\"")
	}
	if err := validateCallbackURL(in.Delivery.URL); err != nil {
		return nil, -32602, err
	}
	if _, err := decodeSecret(in.Delivery.Secret); err != nil {
		return nil, -32602, err
	}
	args := normalizeArgs(in.Arguments)
	id := subscriptionID(ownerID, def.name, args, in.Delivery.URL)

	// Verification challenge — the callback must echo it back, verbatim.
	if err := d.verifyCallback(ctx, in.Delivery.URL, in.Delivery.Secret); err != nil {
		return nil, -32015, err
	}

	ttl := mcpEventTTL
	if in.TTLMs != nil && *in.TTLMs > 0 {
		ttl = time.Duration(*in.TTLMs) * time.Millisecond
	}
	sub := &webhookSubscription{
		id:          id,
		ownerID:     ownerID,
		eventName:   def.name,
		args:        args,
		callbackURL: in.Delivery.URL,
		secret:      in.Delivery.Secret,
		createdAt:   time.Now(),
		expiresAt:   time.Now().Add(ttl),
	}
	d.mu.Lock()
	d.subs[id] = sub
	d.mu.Unlock()

	return &subscribeOutput{
		ID:            id,
		RefreshBefore: sub.expiresAt.UTC().Format(time.RFC3339),
	}, 0, nil
}

// unsubscribe removes a subscription identified by its (owner, event name,
// arguments, callback URL). Idempotent.
func (d *webhookDeliverer) unsubscribe(ownerID, eventName string, args map[string]any, callbackURL string) {
	id := subscriptionID(ownerID, eventName, normalizeArgs(args), callbackURL)
	d.mu.Lock()
	delete(d.subs, id)
	d.mu.Unlock()
}

// list returns the subscriptions for ownerID, newest first.
func (d *webhookDeliverer) list(ownerID string) []*webhookSubscription {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]*webhookSubscription, 0, len(d.subs))
	for _, sub := range d.subs {
		if sub.ownerID == ownerID {
			out = append(out, sub)
		}
	}
	return out
}

// verifyCallback POSTs a one-shot challenge to callbackURL signed with
// secret. A 2xx response must echo the challenge; anything else fails the
// subscription.
func (d *webhookDeliverer) verifyCallback(ctx context.Context, callbackURL, secret string) error {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return err
	}
	challenge := hex.EncodeToString(buf[:])
	body, err := json.Marshal(map[string]any{"type": "verification", "challenge": challenge})
	if err != nil {
		return err
	}
	req, err := signedWebhookRequest(&webhookSubscription{callbackURL: callbackURL, secret: secret}, "verify-"+challenge, body)
	if err != nil {
		return err
	}
	req = req.WithContext(ctx)
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("verification: HTTP %d", resp.StatusCode)
	}
	echo, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return err
	}
	// The spec allows a JSON envelope `{"challenge": "<value>"}` or a bare
	// string — we accept both.
	s := strings.TrimSpace(string(echo))
	if strings.HasPrefix(s, "{") {
		var v struct {
			Challenge string `json:"challenge"`
		}
		if json.Unmarshal(echo, &v) == nil {
			s = v.Challenge
		}
	}
	if !hmac.Equal([]byte(s), []byte(challenge)) {
		return errors.New("verification: callback did not echo the challenge")
	}
	return nil
}

// validateCallbackURL requires an https URL (the spec), though for local
// development we also accept http://localhost:... and http://127.0.0.1:...
// — same as the Stripe webhook forwarder rules.
func validateCallbackURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("callback URL is not a valid absolute URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return errors.New("callback URL must use https")
	}
	return errors.New("callback URL must use https")
}

// normalizeArgs produces a canonical copy of the subscribe arguments so two
// subscriptions with the same filter land on the same id.
func normalizeArgs(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// subscriptionID is a deterministic id built from the subscription's
// identifying tuple; two events/subscribe calls with the same owner, event
// name, args and callback URL return the same id (and so refresh the
// existing subscription).
func subscriptionID(ownerID, eventName string, args map[string]any, callbackURL string) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buf := bytes.Buffer{}
	buf.WriteString(ownerID)
	buf.WriteByte('|')
	buf.WriteString(eventName)
	buf.WriteByte('|')
	buf.WriteString(callbackURL)
	buf.WriteByte('|')
	for _, k := range keys {
		fmt.Fprintf(&buf, "%s=%v;", k, args[k])
	}
	sum := sha256.Sum256(buf.Bytes())
	return "sub_" + hex.EncodeToString(sum[:16])
}

func strconvI64(n int64) string { return fmt.Sprintf("%d", n) }

func mcpEventByName(name string) *mcpEventDef {
	for i := range mcpEvents {
		if mcpEvents[i].name == name {
			return &mcpEvents[i]
		}
	}
	return nil
}
