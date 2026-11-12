package client

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// SSEReader parses a Server-Sent Events stream frame by frame (per the
// `text/event-stream` MIME type). Each call to Next returns the next event
// or an error, blocking on reads from the stream; io.EOF marks a clean
// server close.
type SSEReader struct {
	br *bufio.Reader
}

// NewSSEReader wraps a stream reader (e.g. a response body).
func NewSSEReader(r io.Reader) *SSEReader {
	return &SSEReader{br: bufio.NewReader(r)}
}

// SSEEvent is one event frame as the server sent it. Fields map to the SSE
// field names; Data is the concatenation of every `data:` line, in order.
type SSEEvent struct {
	ID   string
	Name string // from the `event:` field; empty for the default "message"
	Data string
}

// Next reads until the next blank-line separator and returns the event, or
// io.EOF when the stream ends.
func (r *SSEReader) Next() (*SSEEvent, error) {
	e := &SSEEvent{}
	hasField := false
	for {
		line, err := r.br.ReadString('\n')
		if err != nil {
			if err == io.EOF && hasField {
				return e, nil
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if hasField {
				return e, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			// Comment line (keepalive). Ignore.
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			field, value = line, ""
		}
		value = strings.TrimPrefix(value, " ")
		hasField = true
		switch field {
		case "id":
			e.ID = value
		case "event":
			e.Name = value
		case "data":
			if e.Data != "" {
				e.Data += "\n"
			}
			e.Data += value
		case "retry":
			// Clients that reconnect could honor this; our CLI uses its
			// own exponential backoff. Ignore.
		}
	}
}

// AsCommentEvent decodes the event's data as a CommentEvent. Handy when the
// caller already knows which stream this is.
func (e *SSEEvent) AsCommentEvent() (*CommentEvent, error) {
	if e.Data == "" {
		return nil, errors.New("empty data")
	}
	var c CommentEvent
	if err := json.Unmarshal([]byte(e.Data), &c); err != nil {
		return nil, err
	}
	if c.ID == "" {
		c.ID = e.ID
	}
	if c.Name == "" {
		c.Name = e.Name
	}
	return &c, nil
}
