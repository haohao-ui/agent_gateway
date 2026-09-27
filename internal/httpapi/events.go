package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"agent-gateway/internal/events"
	"agent-gateway/internal/policy"
)

// DoctorFunc evaluates diagnostic health of the gateway environment.
type DoctorFunc func(ctx context.Context) any

// handleSSEStreams handles browser and client real-time event subscriptions over Server-Sent Events.
func (s *Server) handleSSEStreams(w http.ResponseWriter, r *http.Request) {
	// 1. Authorize: Header Bearer, Query param token, or secure Cookie
	var currentPrincipal policy.Principal
	if s.policies != nil {
		token := extractOperatorToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "operator token required for event stream")
			return
		}
		p, err := s.policies.Authenticate(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid operator credential")
			return
		}
		currentPrincipal = p
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// 2. Set SSE Headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// 3. Subscribe to Hub
	ch, cancel := s.hub.Subscribe(r.Context())
	defer cancel()

	// Send initial connected event
	fmt.Fprintf(w, "event: connected\ndata: {\"status\":\"connected\",\"time\":%q}\n\n", time.Now().UTC().Format(time.RFC3339))
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// Send ping comment to keep connection alive
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case evt, ok := <-ch:
			if !ok {
				return
			}
			// Scope-based isolation: non-admin operators/viewers only receive events within authorized nodes
			if s.policies != nil && currentPrincipal.Role != policy.Admin {
				if evt.NodeID != "" {
					if err := policy.Authorize(currentPrincipal, policy.ActionTaskRead, evt.NodeID); err != nil {
						continue // Do not leak events outside the principal's node scope
					}
				}
			}
			payload, err := json.Marshal(evt)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleWebDoctor handles diagnostic health checks requested by the Web UI.
func (s *Server) handleWebDoctor(w http.ResponseWriter, r *http.Request) {
	if s.doctorFunc != nil {
		res := s.doctorFunc(r.Context())
		writeJSON(w, http.StatusOK, res)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":    "server",
		"healthy": true,
		"checks":  []any{},
	})
}

// publishEvent is a helper method to safely publish events to the hub if initialized.
func (s *Server) publishEvent(evt events.Event) {
	if s.hub != nil {
		s.hub.Publish(evt)
	}
}
