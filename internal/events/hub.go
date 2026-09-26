// Package events provides an in-memory event bus and streaming hub for task and system lifecycle events.
package events

import (
	"context"
	"sync"
	"time"

	"agent-gateway/internal/protocol"
)

// Event types supported by the hub.
const (
	TypeTaskStatus    = "task.status"
	TypeTaskOutput    = "task.output"
	TypeTaskSubmitted = "task.submitted"
	TypeTaskClaimed   = "task.claimed"
	TypeTaskCompleted = "task.completed"
	TypeTaskCancelled = "task.cancelled"
	TypeTaskRequeued  = "task.requeued"
	TypeTaskResolved  = "task.resolved"
	TypeNodeHeartbeat = "node.heartbeat"
	TypePing          = "ping"
)

// Event is the JSON-serializable envelope broadcast to SSE subscribers.
type Event struct {
	Type      string         `json:"type"`
	TaskID    string         `json:"task_id,omitempty"`
	NodeID    string         `json:"node_id,omitempty"`
	State     protocol.State `json:"state,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
	Data      any            `json:"data,omitempty"`
}

// Hub manages subscription and fan-out of gateway events.
type Hub struct {
	mu          sync.RWMutex
	subscribers map[chan Event]struct{}
	bufferSize  int
	closed      bool
}

// NewHub initializes an event hub with the specified per-subscriber buffer size.
func NewHub(bufferSize int) *Hub {
	if bufferSize <= 0 {
		bufferSize = 64
	}
	return &Hub{
		subscribers: make(map[chan Event]struct{}),
		bufferSize:  bufferSize,
	}
}

// Subscribe attaches a new listener channel for events.
// The caller must call the returned cancel function or cancel the context when done.
func (h *Hub) Subscribe(ctx context.Context) (<-chan Event, func()) {
	ch := make(chan Event, h.bufferSize)

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subscribers, ch)
			h.mu.Unlock()
			// Drain remaining events to prevent deadlock on slow consumers before GC
			for len(ch) > 0 {
				<-ch
			}
		})
	}

	go func() {
		select {
		case <-ctx.Done():
			cancel()
		}
	}()

	return ch, cancel
}

// Publish distributes an event to all active subscribers.
// Slow subscribers with full buffers will drop the event to prevent head-of-line blocking.
func (h *Hub) Publish(evt Event) {
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.closed {
		return
	}

	for ch := range h.subscribers {
		select {
		case ch <- evt:
		default:
			// Buffer full, drop non-critical events to maintain system throughput
		}
	}
}

// SubscriberCount returns the current number of active event listeners.
func (h *Hub) SubscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers)
}

// Close closes the hub and all active listener channels.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return
	}
	h.closed = true

	for ch := range h.subscribers {
		close(ch)
		delete(h.subscribers, ch)
	}
}
