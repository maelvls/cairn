package server

import (
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Server-side event bus: one process-local pub/sub for notifications about
// things that happen in the API (today: new comments). Two things watch it:
// the SSE endpoint handleCommentsStream, which pushes events to a long-lived
// HTTP client; and the MCP events deliverer (webhooks.go), which fans them
// out to subscribers registered through `events/subscribe`. In-memory is
// enough for a single-node cairn — losing an event on a restart is fine,
// clients re-read the latest state when they reconnect.
//
// The bus is topic-based: each artifact has its own topic ("artifact:<id>")
// so subscribers only see events for the artifact they asked about. Publish
// is non-blocking: a slow subscriber drops events instead of blocking the
// writer that handles the HTTP request.

// Event is one thing that happened on the server, as it travels through the
// bus. Name is the dotted event type (e.g. "comment.created"), Data its
// payload. ID is monotonic within one Server process: clients use it as the
// SSE Last-Event-ID so a reconnect skips what they already saw.
type Event struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	ArtifactID string          `json:"artifactId"`
	OccurredAt time.Time       `json:"occurredAt"`
	Data       json.RawMessage `json:"data"`
}

// subBufSize is how many events a slow subscriber may queue before the bus
// drops. The SSE writer flushes each event, so a reasonable TCP stays well
// below this; a wedged client drops rather than stalling notifyComment.
const subBufSize = 32

type subscription struct {
	topic string
	ch    chan Event
}

// EventBus is a topic-keyed, in-memory broadcaster.
type EventBus struct {
	mu     sync.RWMutex
	subs   map[string]map[*subscription]struct{}
	nextID atomic.Uint64
}

func NewEventBus() *EventBus {
	return &EventBus{subs: map[string]map[*subscription]struct{}{}}
}

// nextEventID returns a fresh, monotonically increasing string id. It is
// process-local and resets on restart, which is fine for Last-Event-ID
// hinting — a new process starts the stream fresh.
func (b *EventBus) nextEventID() string {
	return strconv.FormatUint(b.nextID.Add(1), 10)
}

// Subscribe registers a subscriber on topic and returns the channel to read
// from and a cancel to close the subscription (idempotent). The channel is
// buffered and closed by the cancel; publishers never close it.
func (b *EventBus) Subscribe(topic string) (<-chan Event, func()) {
	s := &subscription{topic: topic, ch: make(chan Event, subBufSize)}
	b.mu.Lock()
	m := b.subs[topic]
	if m == nil {
		m = map[*subscription]struct{}{}
		b.subs[topic] = m
	}
	m[s] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if m, ok := b.subs[topic]; ok {
				delete(m, s)
				if len(m) == 0 {
					delete(b.subs, topic)
				}
			}
			b.mu.Unlock()
			close(s.ch)
		})
	}
	return s.ch, cancel
}

// Publish broadcasts e on every topic it names. The caller fills Name, Data
// and ArtifactID; the bus stamps ID and OccurredAt when they are empty, so
// every subscriber sees the same values. Delivery is non-blocking: a
// subscriber whose buffer is full drops the event.
func (b *EventBus) Publish(e Event, topics ...string) Event {
	if e.ID == "" {
		e.ID = b.nextEventID()
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, t := range topics {
		for s := range b.subs[t] {
			select {
			case s.ch <- e:
			default:
				// drop: the subscriber is slower than the bus
			}
		}
	}
	return e
}

// artifactTopic is the topic one artifact's events are published on; "all"
// is the wildcard topic every event also goes to (so a listener can watch
// the whole server).
func artifactTopic(artifactID string) string { return "artifact:" + artifactID }

const allEventsTopic = "all"
