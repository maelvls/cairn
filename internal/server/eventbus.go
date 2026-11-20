package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
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

// eventLogKeep is how many events the bus remembers for the MCP poll and
// push deliveries (events/poll, events/stream), which read by cursor rather
// than from a channel: a slow reader skips nothing as long as it comes back
// within this many events.
const eventLogKeep = 1000

// loggedEvent is an event the bus remembers, with its id as a number.
type loggedEvent struct {
	seq uint64
	Event
}

// EventBus is a topic-keyed, in-memory broadcaster. It also remembers the
// last events published on the wildcard topic, oldest first, so they can be
// read back by cursor.
type EventBus struct {
	mu     sync.RWMutex
	subs   map[string]map[*subscription]struct{}
	nextID atomic.Uint64

	// The log. epoch names this process's log: ids restart from 1 with the
	// process, so a cursor from another epoch points at nothing.
	logMu   sync.Mutex
	epoch   string
	log     []loggedEvent
	head    uint64 // the last logged event's seq
	dropped uint64 // the last event the log let go of
	wakers  map[chan struct{}]struct{}
	logKeep int
}

func NewEventBus() *EventBus {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return &EventBus{
		subs:    map[string]map[*subscription]struct{}{},
		epoch:   hex.EncodeToString(b[:]),
		wakers:  map[chan struct{}]struct{}{},
		logKeep: eventLogKeep,
	}
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
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	if e.ID == "" && slices.Contains(topics, allEventsTopic) {
		// The id is taken under the log's lock, so the log is in id order.
		b.logMu.Lock()
		seq := b.nextID.Add(1)
		e.ID = strconv.FormatUint(seq, 10)
		b.log = append(b.log, loggedEvent{seq: seq, Event: e})
		if drop := len(b.log) - b.logKeep; drop > 0 {
			b.dropped = b.log[drop-1].seq
			b.log = slices.Delete(b.log, 0, drop)
		}
		b.head = seq
		for ch := range b.wakers {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
		b.logMu.Unlock()
	}
	if e.ID == "" {
		e.ID = b.nextEventID()
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

// listen returns a channel that gets a value (at most one pending) each time
// an event is logged; unlisten ends it.
func (b *EventBus) listen() chan struct{} {
	ch := make(chan struct{}, 1)
	b.logMu.Lock()
	b.wakers[ch] = struct{}{}
	b.logMu.Unlock()
	return ch
}

func (b *EventBus) unlisten(ch chan struct{}) {
	b.logMu.Lock()
	delete(b.wakers, ch)
	b.logMu.Unlock()
}

// cursor is the position after the event seq, as clients keep it.
func (b *EventBus) cursor(seq uint64) string {
	return b.epoch + "." + strconv.FormatUint(seq, 10)
}

// eventID is an event's id for the MCP deliveries: unlike Event.ID, it
// doesn't come back after a restart, so clients can deduplicate on it.
func (b *EventBus) eventID(e Event) string { return "evt_" + b.epoch + "_" + e.ID }

// start is where a reader with this cursor begins: after the event it
// names, else now. truncated says events were skipped: the cursor is from
// another process's log, or older than what the log keeps, or maxAge cut
// the replay short.
func (b *EventBus) start(cursor *string, maxAge time.Duration) (pos uint64, truncated bool) {
	b.logMu.Lock()
	defer b.logMu.Unlock()
	if cursor == nil || *cursor == "" {
		return b.head, false
	}
	epoch, n, _ := strings.Cut(*cursor, ".")
	seq, err := strconv.ParseUint(n, 10, 64)
	if err != nil || epoch != b.epoch || seq > b.head {
		return b.head, true
	}
	pos = seq
	if pos < b.dropped {
		pos, truncated = b.dropped, true
	}
	if maxAge > 0 {
		floor := time.Now().Add(-maxAge)
		for _, e := range b.log {
			if e.seq > pos && e.OccurredAt.Before(floor) {
				pos, truncated = e.seq, true
			}
		}
	}
	return pos, truncated
}

// after returns the logged events after pos that match, at most limit (0:
// all), and the position after them: the last one returned when there are
// more, else the head.
func (b *EventBus) after(pos uint64, match func(Event) bool, limit int) (out []Event, next uint64, more bool) {
	b.logMu.Lock()
	defer b.logMu.Unlock()
	for _, e := range b.log {
		if e.seq <= pos || !match(e.Event) {
			continue
		}
		if limit > 0 && len(out) == limit {
			seq, _ := eventSeq(out[len(out)-1])
			return out, seq, true
		}
		out = append(out, e.Event)
	}
	return out, max(b.head, pos), false
}

// eventSeq is a logged event's id as a number.
func eventSeq(e Event) (uint64, bool) {
	seq, err := strconv.ParseUint(e.ID, 10, 64)
	return seq, err == nil
}
