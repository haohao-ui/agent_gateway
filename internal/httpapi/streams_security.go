package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

const maxPrincipalStreams = 4
const maxTotalStreams = 64
const streamAuthInterval = 250 * time.Millisecond

type streamLease struct {
	ctx    context.Context
	cancel context.CancelFunc
	close  func()
}

// All stream admission (including MCP) is atomic and precedes response headers.
func (s *Server) openStream(ctx context.Context, id, token string) (*streamLease, error) {
	s.streamsMu.Lock()
	if s.streamCount >= maxTotalStreams || len(s.streams[id]) >= maxPrincipalStreams {
		s.streamsMu.Unlock()
		return nil, errors.New("stream limit")
	}
	ctx, cancel := context.WithCancel(ctx)
	lease := &streamLease{ctx: ctx, cancel: cancel}
	if s.streams[id] == nil {
		s.streams[id] = make(map[*streamLease]struct{})
	}
	s.streams[id][lease] = struct{}{}
	s.streamCount++
	s.streamsMu.Unlock()
	done := make(chan struct{})
	var once sync.Once
	lease.close = func() {
		once.Do(func() {
			cancel()
			<-done
			s.streamsMu.Lock()
			delete(s.streams[id], lease)
			if len(s.streams[id]) == 0 {
				delete(s.streams, id)
			}
			s.streamCount--
			s.streamsMu.Unlock()
		})
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(streamAuthInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := s.policies.Authenticate(ctx, token); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return lease, nil
}

func (s *Server) closeStreams(id string) {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	for lease := range s.streams[id] {
		lease.cancel()
	}
}

// Recheck before writing each frame; the timer also terminates idle streams
// revoked by a separate CLI/process. Already in-flight bytes cannot be recalled.
type authenticatedStreamWriter struct {
	http.ResponseWriter
	server *Server
	token  string
	ctx    context.Context
}

func (w *authenticatedStreamWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *authenticatedStreamWriter) Write(b []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if _, err := w.server.policies.Authenticate(w.ctx, w.token); err != nil {
		return 0, err
	}
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(5 * time.Second))
	return w.ResponseWriter.Write(b)
}
func (w *authenticatedStreamWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }
