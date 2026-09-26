package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"agent-gateway/internal/events"
)

// DoctorFunc evaluates diagnostic health of the gateway environment.
type DoctorFunc func(ctx context.Context) any

// handleSSEStreams handles browser and client real-time event subscriptions over Server-Sent Events.
func (s *Server) handleSSEStreams(w http.ResponseWriter, r *http.Request) {
	// 1. Authorize: Header Bearer or Query param token
	if s.policies != nil && r.TLS != nil {
		token := ""
		if authHeader := r.Header.Get("Authorization"); authHeader != "" {
			parts := strings.Fields(authHeader)
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
				token = parts[1]
			}
		}
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if token == "" {
			if c, err := r.Cookie("gateway_token"); err == nil && c.Value != "" {
				token = c.Value
			}
		}

		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "operator token required for event stream")
			return
		}
		if _, err := s.policies.Authenticate(r.Context(), token); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid operator credential")
			return
		}
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
