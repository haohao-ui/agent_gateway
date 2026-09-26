// Package httpapi exposes the gateway task routes over HTTP/2 with mTLS.
//
// The routes, status codes and TLS layering follow the frozen M2 contract in
// docs/CONTRACTS.md: /v1/pair is reachable over server TLS alone, while every
// /v1/tasks/** route requires a client certificate whose chain verified against
// the gateway CA. A node may only act on its own tasks; the caller identity is
// always taken from the verified certificate, never from the request body.
//
// Not implemented in this package, and therefore not to be reported as working:
//
//   - Event persistence. POST /v1/tasks/events validates the caller and the
//     envelope but stores nothing, because no event store exists yet. The 200
//     it returns is an acknowledgement of receipt, not a durability promise.
package httpapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"agent-gateway/internal/artifact"
	"agent-gateway/internal/devicestore"
	"agent-gateway/internal/events"
	"agent-gateway/internal/identity"
	"agent-gateway/internal/policy"
	"agent-gateway/internal/protocol"
	"agent-gateway/internal/taskstore"
	"agent-gateway/internal/webui"
)

type contextKey string

const nodeIDContextKey contextKey = "nodeID"

const (
	// defaultLeaseTTL is used when a node does not ask for a lease duration.
	defaultLeaseTTL = 60 * time.Second
	// claimWait is how long a claim request is held open before the gateway
	// answers 204 and the node starts a new long poll.
	claimWait = 25 * time.Second
)

// Request body limits. They are enforced before JSON decoding, so an oversized
// or hostile body cannot make the gateway allocate without bound. The envelope
// is larger than the contract's 64 KiB payload limit because JSON escaping and
// the surrounding fields add overhead.
const (
	credentialBodyLimit = 8 << 10   // claim, start, renew: ids and a token
	resultBodyLimit     = 256 << 10 // complete: bounded result text
	submitBodyLimit     = 128 << 10 // submit: bounded capability input
	eventBodyLimit      = 256 << 10 // events: one streaming chunk
)

// Config configures the HTTP/2 API server.
type Config struct {
	Addr       string
	Hosts      []string
	DefaultTTL time.Duration
}

// Server exposes the gateway task management and node protocol over HTTP/2 with mTLS.
type Server struct {
	store      *taskstore.Store
	ca         *identity.CA
	mux        *http.ServeMux
	devices    *devicestore.Store
	policies   *policy.Store
	hub        *events.Hub
	artifacts  *artifact.Store
	dataDir    string
	doctorFunc DoctorFunc
	mcpHandler http.Handler

	allowPlainHTTP bool
	adminPassword  string
	adminToken     string
	mcpSessions   sync.Map // sessionID (string) -> expiry time (time.Time)
	nodeRuntime   sync.Map // nodeID (string) -> NodeRuntimeInfo

	// pairLimiter bounds unauthenticated /v1/pair attempts per client address.
	pairLimiter *pairLimiter

	waitersMu sync.Mutex
	waiters   map[string][]chan struct{} // nodeID -> slice of wakeup channels
}

// NodeRuntimeInfo tracks real-time heartbeat, software version and detected agent capabilities.
type NodeRuntimeInfo struct {
	NodeID   string                   `json:"node_id"`
	Version  string                   `json:"version"`
	OS       string                   `json:"os"`
	Arch     string                   `json:"arch"`
	Agents   []protocol.AgentSoftware `json:"agents"`
	LastSeen time.Time                `json:"last_seen"`
	Online   bool                     `json:"online"`
}

// NewServer initializes an HTTP/2 API handler with taskstore and identity CA.
func NewServer(store *taskstore.Store, ca *identity.CA) *Server {
	s := &Server{
		store:       store,
		ca:          ca,
		hub:         events.NewHub(128),
		mux:         http.NewServeMux(),
		pairLimiter: newPairLimiter(pairRatePerSecond, pairBurst, time.Now),
		waiters:     make(map[string][]chan struct{}),
	}
	s.registerRoutes()
	s.registerOperatorRoutes()
	s.registerArtifactRoutes()
	return s
}

// SetAdminCredentials configures the operator login credentials.
func (s *Server) SetAdminCredentials(password, token string) {
	s.adminPassword = password
	s.adminToken = token
}

// SetAllowPlainHTTP controls whether operator endpoints can be accessed over unencrypted HTTP (e.g. via --http-addr).
func (s *Server) SetAllowPlainHTTP(allow bool) {
	s.allowPlainHTTP = allow
}

// SetDataDir sets the gateway data directory path for diagnostics and reporting.
func (s *Server) SetDataDir(dir string) {
	s.dataDir = dir
}

// SetDoctorFunc configures the diagnostics runner for web queries.
func (s *Server) SetDoctorFunc(fn DoctorFunc) {
	s.doctorFunc = fn
}

// Hub returns the server's event hub for subscribing to or emitting events.
func (s *Server) Hub() *events.Hub {
	return s.hub
}

func (s *Server) registerRoutes() {
	// Web UI management console and real-time SSE stream
	s.mux.Handle("GET /ui/", http.StripPrefix("/ui", webui.Handler()))
	s.mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusPermanentRedirect)
	})
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/logout", s.handleLogout)
	s.mux.HandleFunc("GET /v1/events/stream", s.handleSSEStreams)
	s.mux.HandleFunc("GET /v1/doctor", s.handleWebDoctor)

	// Public status, CA certificate and AI onboarding documentation
	s.mux.HandleFunc("GET /api/public/status", s.handlePublicStatus)
	s.mux.HandleFunc("GET /onboarding.md", s.handleOnboardingMD)
	s.mux.HandleFunc("GET /ca.crt", s.handleDownloadCA)

	// Download Center for node deployment and skills
	s.mux.HandleFunc("GET /download/mesh", s.handleDownloadMesh)
	s.mux.HandleFunc("GET /download/ca.crt", s.handleDownloadCA)
	s.mux.HandleFunc("GET /download/install.sh", s.handleDownloadInstallScript)
	s.mux.HandleFunc("GET /skills/agent-mesh/SKILL.md", s.handleDownloadSkill)
	s.mux.HandleFunc("GET /download/skills/agent-mesh/SKILL.md", s.handleDownloadSkill)

	// System Management (TLS configuration and restart)
	s.mux.HandleFunc("GET /api/system/tls", s.handleGetTLSStatus)
	s.mux.HandleFunc("POST /api/system/tls/upload", s.handleUploadTLS)
	s.mux.HandleFunc("POST /api/system/tls/reset", s.handleResetTLS)
	s.mux.HandleFunc("POST /api/system/restart", s.handleSystemRestart)

	// Streamable HTTP / SSE MCP protocol endpoint
	s.mux.HandleFunc("/mcp", s.handleMCP)
	s.mux.HandleFunc("/mcp/", s.handleMCP)

	// Public pair endpoint (protected by server TLS, an invitation token and
	// the per-address rate limiter).
	s.mux.HandleFunc("POST /v1/pair", s.handlePair)

	// Every route below requires a verified client certificate.
	s.mux.HandleFunc("POST /v1/tasks/submit", s.requireNodeAuth(s.handleSubmit))
	s.mux.HandleFunc("GET /v1/tasks/{id}", s.requireNodeAuth(s.handleGetTask))
	s.mux.HandleFunc("GET /v1/tasks/{id}/wait", s.requireNodeAuth(s.handleWaitTask))
	s.mux.HandleFunc("POST /v1/tasks/{id}/cancel", s.requireNodeAuth(s.handleCancelTask))
	s.mux.HandleFunc("POST /v1/tasks/claim", s.requireNodeAuth(s.handleClaim))
	s.mux.HandleFunc("POST /v1/tasks/start", s.requireNodeAuth(s.handleStart))
	s.mux.HandleFunc("POST /v1/tasks/renew", s.requireNodeAuth(s.handleRenew))
	s.mux.HandleFunc("POST /v1/tasks/complete", s.requireNodeAuth(s.handleComplete))
	s.mux.HandleFunc("POST /v1/tasks/events", s.requireNodeAuth(s.handleEvents))
	s.mux.HandleFunc("GET /v1/tasks/probe", s.requireNodeAuth(s.handleProbe))
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	nodeID, _ := r.Context().Value(nodeIDContextKey).(string)
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"node_id": nodeID,
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, credentialBodyLimit, &in) {
		return
	}
	expectedPass := s.adminPassword
	if expectedPass == "" {
		expectedPass = "admin"
	}
	if in.Username != "admin" || in.Password != expectedPass {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid username or password")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "gateway_token",
		Value:    s.adminToken,
		Path:     "/",
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   30 * 24 * 3600,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"username": "admin",
		"token":    s.adminToken,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "gateway_token",
		Value:    "",
		Path:     "/",
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

// BuildTLSConfig returns a tls.Config with ALPN h2, server certificates, and mTLS verification.
func (s *Server) BuildTLSConfig(hosts []string) (*tls.Config, error) {
	var serverCert tls.Certificate
	var err error

	// Check if custom TLS certificate and key exist and are valid
	certPath := s.customCertPath()
	keyPath := s.customKeyPath()
	if _, cErr := os.Stat(certPath); cErr == nil {
		if _, kErr := os.Stat(keyPath); kErr == nil {
			serverCert, err = tls.LoadX509KeyPair(certPath, keyPath)
			if err != nil {
				// Fallback to built-in CA if custom keypair loading fails
				serverCert, err = s.ca.GenerateServerCertificate(hosts)
			}
		} else {
			serverCert, err = s.ca.GenerateServerCertificate(hosts)
		}
	} else {
		serverCert, err = s.ca.GenerateServerCertificate(hosts)
	}

	if err != nil {
		return nil, fmt.Errorf("generate or load server certificate: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    s.ca.CertPool(),
		// VerifyClientCertIfGiven allows /v1/pair without client cert,
		// while requireNodeAuth enforces it for node endpoints.
		ClientAuth: tls.VerifyClientCertIfGiven,
		NextProtos: []string{"h2", "http/1.1"},
		MinVersion: tls.VersionTLS13,
	}, nil
}

// requireNodeAuth binds the request to a device identity taken from the client
// certificate the TLS stack verified. A presented certificate that failed
// verification never reaches a handler: the handshake fails first, and
// VerifiedChains stays empty for a client that presented nothing.
func (s *Server) requireNodeAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing verified client certificate")
			return
		}

		// The verified leaf, not PeerCertificates[0]: the latter is only what
		// the client offered.
		nodeID, err := identity.ExtractNodeID([]*x509.Certificate{r.TLS.VerifiedChains[0][0]})
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
			return
		}

		if !s.authorizeDevice(w, r, nodeID) {
			return
		}
		ctx := context.WithValue(r.Context(), nodeIDContextKey, nodeID)
		next(w, r.WithContext(ctx))
	}
}

func nodeIDFromContext(ctx context.Context) string {
	if val, ok := ctx.Value(nodeIDContextKey).(string); ok {
		return val
	}
	return ""
}

// authorizeTask loads a task and rejects any caller other than its owning node.
//
// A cross-node answer is 404 rather than 403: a node must not be able to probe
// which task IDs exist on the gateway. The caller's node identity comes from
// its certificate, so a request body cannot claim another node's task.
func (s *Server) authorizeTask(ctx context.Context, w http.ResponseWriter, taskID, callerNodeID string) (protocol.Task, bool) {
	task, err := s.store.Get(ctx, taskID)
	if err != nil {
		handleStoreError(w, err)
		return protocol.Task{}, false
	}
	if task.NodeID != callerNodeID {
		writeError(w, http.StatusNotFound, "not_found", "task not found")
		return protocol.Task{}, false
	}
	return task, true
}

// handlePair handles node enrollment with one-time invitation token.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil {
		writeError(w, http.StatusBadRequest, "tls_required", "pairing requires a TLS connection")
		return
	}
	if !s.pairLimiter.allow(clientKey(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many pairing attempts")
		return
	}

	var req protocol.PairRequest
	if !readJSON(w, r, credentialBodyLimit, &req) {
		return
	}

	nodeID, certPEM, err := s.ca.SignNodeCSR(req.InvitationToken, []byte(req.CSRPEM))
	if err != nil {
		if errors.Is(err, protocol.ErrUnauthorized) {
			writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_csr", err.Error())
		return
	}

	if s.devices != nil {
		if err := s.registerCertificate(r.Context(), nodeID, certPEM); err != nil {
			writeError(w, 500, "internal_error", "device registration failed; request a new invitation")
			return
		}
	}
	resp := protocol.PairResponse{
		NodeID:        nodeID,
		CertPEM:       string(certPEM),
		CACertPEM:     string(s.ca.CACertPEM()),
		ServerVersion: protocol.CurrentProtocolVersion,
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSubmit receives task submit requests from the node that will run them.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	caller := nodeIDFromContext(r.Context())

	var req protocol.SubmitRequest
	if !readJSON(w, r, submitBodyLimit, &req) {
		return
	}

	// The certificate decides the actor: a node cannot queue work on another
	// node by naming it in the body.
	if req.NodeID != caller {
		writeError(w, http.StatusForbidden, "forbidden", "node identity mismatch")
		return
	}

	task, err := s.store.Submit(r.Context(), req)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	// Wake up any long-polling claim waiter for this node
	s.notifyNode(req.NodeID)
	s.publishEvent(events.Event{
		Type:      events.TypeTaskSubmitted,
		TaskID:    task.ID,
		NodeID:    task.NodeID,
		State:     task.State,
		Timestamp: task.CreatedAt,
	})

	writeJSON(w, http.StatusCreated, task)
}

// handleGetTask queries a task owned by the calling node.
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	caller := nodeIDFromContext(r.Context())

	task, ok := s.authorizeTask(r.Context(), w, r.PathValue("id"), caller)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// handleCancelTask cancels a task owned by the calling node.
func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	caller := nodeIDFromContext(r.Context())

	taskID := r.PathValue("id")
	if _, ok := s.authorizeTask(r.Context(), w, taskID, caller); !ok {
		return
	}

	task, err := s.store.Cancel(r.Context(), taskID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	s.publishEvent(events.Event{
		Type:      events.TypeTaskCancelled,
		TaskID:    task.ID,
		NodeID:    task.NodeID,
		State:     task.State,
		Timestamp: time.Now().UTC(),
	})
	writeJSON(w, http.StatusOK, task)
}

// handleClaim handles HTTP/2 long-polling claim requests from nodes.
func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	nodeID := nodeIDFromContext(r.Context())
	if nodeID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing node identity")
		return
	}

	var req protocol.ClaimRequest
	if !readJSON(w, r, credentialBodyLimit, &req) {
		return
	}

	// Update node runtime heartbeat and detected agent tools
	version := req.NodeVersion
	if version == "" {
		version = "0.1.0"
	}
	osName := req.OS
	if osName == "" {
		osName = "linux"
	}
	arch := req.Arch
	if arch == "" {
		arch = "amd64"
	}
	s.nodeRuntime.Store(nodeID, NodeRuntimeInfo{
		NodeID:   nodeID,
		Version:  version,
		OS:       osName,
		Arch:     arch,
		Agents:   req.Agents,
		LastSeen: time.Now().UTC(),
		Online:   true,
	})

	leaseDuration := time.Duration(req.LeaseDurationSeconds) * time.Second
	if leaseDuration <= 0 {
		leaseDuration = defaultLeaseTTL
	}

	// First try an immediate claim
	lease, err := s.store.Claim(r.Context(), nodeID, leaseDuration)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if lease != nil {
		s.publishEvent(events.Event{
			Type:      events.TypeTaskClaimed,
			TaskID:    lease.Task.ID,
			NodeID:    lease.Task.NodeID,
			State:     lease.Task.State,
			Timestamp: time.Now().UTC(),
		})
		writeJSON(w, http.StatusOK, lease)
		return
	}

	// No queued work: hold the request open until a submission wakes this node
	// or the long poll expires.
	waitCh := s.registerWaiter(nodeID)
	defer s.unregisterWaiter(nodeID, waitCh)

	timer := time.NewTimer(claimWait)
	defer timer.Stop()

	select {
	case <-r.Context().Done():
		// The node disconnected; the task, if any, stays queued for the next poll.
		return
	case <-timer.C:
		w.WriteHeader(http.StatusNoContent)
		return
	case <-waitCh:
		if !s.authorizeDevice(w, r, nodeID) {
			return
		}
		lease, err = s.store.Claim(r.Context(), nodeID, leaseDuration)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			handleStoreError(w, err)
			return
		}
		if lease != nil {
			s.publishEvent(events.Event{
				Type:      events.TypeTaskClaimed,
				TaskID:    lease.Task.ID,
				NodeID:    lease.Task.NodeID,
				State:     lease.Task.State,
				Timestamp: time.Now().UTC(),
			})
			writeJSON(w, http.StatusOK, lease)
			return
		}
		// Another waiter took the task first; the node polls again.
		w.WriteHeader(http.StatusNoContent)
	}
}

// StartRequest carries the attempt credentials for POST /v1/tasks/start.
//
// The shared protocol package is coordinator-owned and frozen, and it has no
// StartRequest, so this route defines its own envelope. It mirrors the
// credential triple the renew and complete routes already use.
type StartRequest struct {
	TaskID    string `json:"task_id"`
	AttemptID string `json:"attempt_id"`
	Token     string `json:"token"`
}

// handleStart confirms that the node recorded the lease locally and is about to
// launch the work. It is idempotent for a retransmission: a node that lost the
// first response must be able to confirm the start without the gateway
// treating the retry as a second launch.
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	caller := nodeIDFromContext(r.Context())

	var req StartRequest
	if !readJSON(w, r, credentialBodyLimit, &req) {
		return
	}

	if _, ok := s.authorizeTask(r.Context(), w, req.TaskID, caller); !ok {
		return
	}

	if err := s.store.Start(r.Context(), req.TaskID, req.AttemptID, req.Token); err != nil {
		if errors.Is(err, protocol.ErrConflict) && s.startedByThisAttempt(r.Context(), req) {
			w.WriteHeader(http.StatusOK)
			return
		}
		handleStoreError(w, err)
		return
	}

	s.publishEvent(events.Event{
		Type:      events.TypeTaskStatus,
		TaskID:    req.TaskID,
		NodeID:    caller,
		State:     protocol.Running,
		Timestamp: time.Now().UTC(),
	})

	w.WriteHeader(http.StatusOK)
}

// startedByThisAttempt reports whether a conflict from Start means this attempt
// is already running under a live lease, which is what a retransmitted Start
// looks like. Any other conflict (a different attempt, a lease that has since
// lapsed) stays an error.
//
// The attempt ID and the caller's certificate are the checks available here:
// the store keeps only the token's hash and exposes no way to verify a token
// without also mutating state, so a retransmission is confirmed on
// attempt+state+lease rather than on the token itself.
func (s *Server) startedByThisAttempt(ctx context.Context, req StartRequest) bool {
	task, err := s.store.Get(ctx, req.TaskID)
	if err != nil {
		return false
	}
	if task.AttemptID != req.AttemptID {
		return false
	}
	// cancel_requested still means this attempt started; the cancellation
	// arrived afterwards and the node reports it through Complete.
	if task.State != protocol.Running && task.State != protocol.CancelRequested {
		return false
	}
	return task.LeaseExpiresAt != nil && task.LeaseExpiresAt.After(time.Now().UTC())
}

// handleRenew handles lease renewal from active nodes.
func (s *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	caller := nodeIDFromContext(r.Context())

	var req protocol.RenewRequest
	if !readJSON(w, r, credentialBodyLimit, &req) {
		return
	}

	if _, ok := s.authorizeTask(r.Context(), w, req.TaskID, caller); !ok {
		return
	}

	leaseDuration := time.Duration(req.LeaseDurationSeconds) * time.Second
	if leaseDuration <= 0 {
		leaseDuration = defaultLeaseTTL
	}

	if err := s.store.Renew(r.Context(), req.TaskID, req.AttemptID, req.Token, leaseDuration); err != nil {
		handleStoreError(w, err)
		return
	}

	// Report the deadline the store actually recorded rather than one derived
	// from this process's clock after the write lock was released.
	task, err := s.store.Get(r.Context(), req.TaskID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if task.LeaseExpiresAt == nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "renewed lease has no deadline")
		return
	}

	s.publishEvent(events.Event{
		Type:      events.TypeTaskStatus,
		TaskID:    req.TaskID,
		NodeID:    caller,
		State:     protocol.Running,
		Timestamp: time.Now().UTC(),
	})

	writeJSON(w, http.StatusOK, protocol.RenewResponse{LeaseExpiresAt: *task.LeaseExpiresAt})
}

// handleComplete handles task completion submission from nodes.
func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request) {
	caller := nodeIDFromContext(r.Context())

	var req protocol.CompleteRequest
	if !readJSON(w, r, resultBodyLimit, &req) {
		return
	}

	if _, ok := s.authorizeTask(r.Context(), w, req.TaskID, caller); !ok {
		return
	}

	// The store owns idempotency: the same result for the same attempt is
	// accepted again after a lost acknowledgement, a different one conflicts.
	if err := s.store.Complete(r.Context(), req.TaskID, req.AttemptID, req.Token, req.Result); err != nil {
		handleStoreError(w, err)
		return
	}

	s.publishEvent(events.Event{
		Type:      events.TypeTaskCompleted,
		TaskID:    req.TaskID,
		NodeID:    caller,
		Timestamp: time.Now().UTC(),
		Data:      req.Result,
	})

	w.WriteHeader(http.StatusOK)
}

// handleEvents handles streaming execution chunks from nodes.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	caller := nodeIDFromContext(r.Context())

	var ev protocol.TaskEvent
	if !readJSON(w, r, eventBodyLimit, &ev) {
		return
	}
	if ev.Sequence <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "event sequence must be positive")
		return
	}
	if _, ok := s.authorizeTask(r.Context(), w, ev.TaskID, caller); !ok {
		return
	}

	ts := ev.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	s.publishEvent(events.Event{
		Type:      events.TypeTaskOutput,
		TaskID:    ev.TaskID,
		NodeID:    caller,
		Timestamp: ts,
		Data:      string(ev.Data),
	})

	w.WriteHeader(http.StatusOK)
}

func (s *Server) registerWaiter(nodeID string) chan struct{} {
	s.waitersMu.Lock()
	defer s.waitersMu.Unlock()
	ch := make(chan struct{}, 1)
	s.waiters[nodeID] = append(s.waiters[nodeID], ch)
	return ch
}

func (s *Server) unregisterWaiter(nodeID string, ch chan struct{}) {
	s.waitersMu.Lock()
	defer s.waitersMu.Unlock()
	list := s.waiters[nodeID]
	for i, c := range list {
		if c == ch {
			s.waiters[nodeID] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(s.waiters[nodeID]) == 0 {
		delete(s.waiters, nodeID)
	}
}

func (s *Server) notifyNode(nodeID string) {
	s.waitersMu.Lock()
	defer s.waitersMu.Unlock()
	for _, ch := range s.waiters[nodeID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// readJSON reads at most limit bytes and decodes them as JSON. It writes the
// error response itself and reports whether the caller may continue, so a
// handler cannot forget to answer an oversized or malformed body.
func readJSON(w http.ResponseWriter, r *http.Request, limit int64, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large",
				fmt.Sprintf("request body exceeds %d bytes", limit))
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, protocol.ErrorResponse{ErrorCode: code, Message: msg})
}

func handleStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, protocol.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, protocol.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, protocol.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, protocol.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
	case errors.Is(err, protocol.ErrLeaseExpired):
		writeError(w, http.StatusConflict, "lease_expired", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

// Pairing rate limits. The invitation token is the real secret; the limiter
// exists so an unauthenticated endpoint cannot be hammered for free.
const (
	pairRatePerSecond = 5
	pairBurst         = 10
	// maxLimiterEntries bounds the bucket map, so a stream of distinct source
	// addresses cannot grow it without limit.
	maxLimiterEntries = 4096
)

type pairBucket struct {
	tokens float64
	last   time.Time
}

// pairLimiter is a per-key token bucket with an injectable clock.
type pairLimiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*pairBucket
	now     func() time.Time
}

func newPairLimiter(rate, burst float64, now func() time.Time) *pairLimiter {
	return &pairLimiter{
		rate:    rate,
		burst:   burst,
		buckets: make(map[string]*pairBucket),
		now:     now,
	}
}

// allow reports whether a request from key may proceed.
func (l *pairLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if len(l.buckets) > maxLimiterEntries {
		l.evictRefilled(now)
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &pairBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// evictRefilled drops buckets that have had time to refill completely, which
// bounds the map without tracking a separate expiry list.
func (l *pairLimiter) evictRefilled(now time.Time) {
	refill := time.Duration(l.burst/l.rate*float64(time.Second)) + time.Second
	for key, b := range l.buckets {
		if now.Sub(b.last) >= refill {
			delete(l.buckets, key)
		}
	}
}

// clientKey identifies the rate-limit bucket for a request.
//
// X-Forwarded-For is deliberately ignored: a header the client controls must
// not choose its own bucket, and no trusted proxy is configured at this layer.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
