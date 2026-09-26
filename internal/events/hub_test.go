package events

import (
	"context"
	"sync"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

func TestHub_SubscribeAndPublish(t *testing.T) {
	hub := NewHub(10)
	defer hub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, unsub := hub.Subscribe(ctx)
	defer unsub()

	if hub.SubscriberCount() != 1 {
		t.Fatalf("expected 1 subscriber, got %d", hub.SubscriberCount())
	}

	evt := Event{
		Type:   TypeTaskStatus,
		TaskID: "task-1",
		State:  protocol.Running,
	}
	hub.Publish(evt)

	select {
	case received := <-ch:
		if received.TaskID != "task-1" || received.State != protocol.Running {
			t.Fatalf("unexpected event: %+v", received)
		}
		if received.Timestamp.IsZero() {
			t.Fatal("expected non-zero timestamp")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for event")
	}

	unsub()
	if hub.SubscriberCount() != 0 {
		t.Fatalf("expected 0 subscribers after unsub, got %d", hub.SubscriberCount())
	}
}

func TestHub_ContextCancellation(t *testing.T) {
	hub := NewHub(10)
	defer hub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := hub.Subscribe(ctx)

	if hub.SubscriberCount() != 1 {
		t.Fatalf("expected 1 subscriber, got %d", hub.SubscriberCount())
	}

	cancel()

	// Wait for background cancellation goroutine
	for i := 0; i < 20; i++ {
		if hub.SubscriberCount() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if hub.SubscriberCount() != 0 {
		t.Fatalf("expected 0 subscribers after ctx cancel, got %d", hub.SubscriberCount())
	}
	_ = ch
}

func TestHub_BufferFullDoesNotBlock(t *testing.T) {
	hub := NewHub(2)
	defer hub.Close()

	ch, unsub := hub.Subscribe(context.Background())
	defer unsub()

	// Fill buffer
	hub.Publish(Event{Type: TypePing})
	hub.Publish(Event{Type: TypePing})

	// Third publish should not block despite buffer being full
	done := make(chan struct{})
	go func() {
		hub.Publish(Event{Type: TypePing})
		close(done)
	}()

	select {
	case <-done:
		// success, did not block
	case <-time.After(500 * time.Millisecond):
		t.Fatal("publish blocked on slow subscriber")
	}

	// Drain 2 events
	<-ch
	<-ch
}

func TestHub_ConcurrentSubscribers(t *testing.T) {
	hub := NewHub(50)
	defer hub.Close()

	const numSubscribers = 20
	const numEvents = 20

	var wg sync.WaitGroup
	wg.Add(numSubscribers)

	for i := 0; i < numSubscribers; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			ch, unsub := hub.Subscribe(ctx)
			defer unsub()

			count := 0
			for count < numEvents {
				select {
				case <-ch:
					count++
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Give goroutines time to subscribe
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < numEvents; i++ {
		hub.Publish(Event{
			Type:   TypeTaskOutput,
			TaskID: "task-concurrent",
		})
	}

	wg.Wait()
}
