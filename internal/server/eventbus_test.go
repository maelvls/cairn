package server

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEventBus_PublishToSubscriber(t *testing.T) {
	bus := NewEventBus()
	ch, cancel := bus.Subscribe(artifactTopic("a1"))
	defer cancel()

	bus.Publish(Event{Name: "comment.created", ArtifactID: "a1", Data: json.RawMessage(`{"id":"c1"}`)}, artifactTopic("a1"))

	select {
	case e := <-ch:
		if e.Name != "comment.created" || e.ArtifactID != "a1" {
			t.Fatalf("got %+v", e)
		}
		if e.ID == "" {
			t.Fatal("id should be auto-assigned")
		}
	case <-time.After(time.Second):
		t.Fatal("no event received")
	}
}

func TestEventBus_SubscriberIsolation(t *testing.T) {
	bus := NewEventBus()
	chA, cancelA := bus.Subscribe(artifactTopic("a1"))
	defer cancelA()
	chB, cancelB := bus.Subscribe(artifactTopic("a2"))
	defer cancelB()

	bus.Publish(Event{Name: "x", ArtifactID: "a1"}, artifactTopic("a1"))

	select {
	case <-chA:
	case <-time.After(time.Second):
		t.Fatal("a1 subscriber missed its event")
	}
	select {
	case e := <-chB:
		t.Fatalf("a2 subscriber saw an event for a1: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEventBus_WildcardTopic(t *testing.T) {
	bus := NewEventBus()
	ch, cancel := bus.Subscribe(allEventsTopic)
	defer cancel()

	bus.Publish(Event{Name: "x", ArtifactID: "a1"}, artifactTopic("a1"), allEventsTopic)

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("wildcard subscriber missed its event")
	}
}

func TestEventBus_SlowSubscriberDrops(t *testing.T) {
	bus := NewEventBus()
	// Never read from the channel: the bus must not block.
	_, cancel := bus.Subscribe(artifactTopic("a1"))
	defer cancel()

	// More events than the subscriber buffer: excess should be dropped
	// rather than blocking.
	for i := 0; i < subBufSize*2; i++ {
		done := make(chan struct{})
		go func() {
			bus.Publish(Event{Name: "x", ArtifactID: "a1"}, artifactTopic("a1"))
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("Publish blocked on a full-buffer subscriber at event %d", i)
		}
	}
}

func TestEventBus_CancelClosesChannel(t *testing.T) {
	bus := NewEventBus()
	ch, cancel := bus.Subscribe(artifactTopic("a1"))
	cancel()
	// A second cancel must be a no-op (not re-close).
	cancel()
	// After cancel, reads return zero-value + ok=false.
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after cancel")
	}
}
