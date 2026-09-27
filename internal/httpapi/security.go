package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"time"

	"agent-gateway/internal/devicestore"
	"agent-gateway/internal/events"
	"agent-gateway/internal/identity"
	"agent-gateway/internal/policy"
	"agent-gateway/internal/protocol"
	"agent-gateway/internal/taskstore"
)

// NewSecureServer requires persistent security stores. Pre-registry certificates
// are intentionally rejected: their owners must explicitly pair again.
func NewSecureServer(tasks *taskstore.Store, ca *identity.CA, devices *devicestore.Store, policies *policy.Store) (*Server, error) {
	if tasks == nil || ca == nil || devices == nil || policies == nil {
		return nil, errors.New("all security stores and CA are required")
	}
	s := NewServer(tasks, ca)
	s.devices = devices
	s.policies = policies
	return s, nil
}
func certificateFingerprint(cert *x509.Certificate) string {
	h := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(h[:])
}
func (s *Server) registerCertificate(ctx context.Context, id string, certPEM []byte) error {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return errors.New("invalid certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	return s.devices.Register(ctx, id, certificateFingerprint(cert), cert.NotAfter)
}
func (s *Server) authorizeDevice(w http.ResponseWriter, r *http.Request, id string) bool {
	if s.devices == nil {
		return true
	} // Legacy constructor for module tests only.
	if err := s.devices.Authorize(r.Context(), id, certificateFingerprint(r.TLS.VerifiedChains[0][0])); err != nil {
		writeError(w, 401, "unauthorized", "device is not authorized")
		return false
	}
	return true
}

const principalContextKey contextKey = "principal"

func (s *Server) requireOperator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || s.policies == nil {
			writeError(w, 401, "unauthorized", "HTTPS operator authentication required")
			return
		}
		token := extractOperatorToken(r)
		if token == "" {
			writeError(w, 401, "unauthorized", "operator credential required")
			return
		}
		p, err := s.policies.Authenticate(r.Context(), token)
		if err != nil {
			writeError(w, 401, "unauthorized", "invalid operator credential")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalContextKey, p)))
	}
}
func operatorPrincipal(r *http.Request) policy.Principal {
	p, _ := r.Context().Value(principalContextKey).(policy.Principal)
	return p
}
func (s *Server) registerOperatorRoutes() {
	s.mux.HandleFunc("GET /v1/operator/credentials", s.requireOperator(s.listCredentials))
	s.mux.HandleFunc("POST /v1/operator/credentials/{id}/revoke", s.requireOperator(s.revokeCredential))
	s.mux.HandleFunc("POST /v1/operator/tasks/submit", s.requireOperator(s.operatorSubmit))
	s.mux.HandleFunc("GET /v1/operator/tasks/{id}", s.requireOperator(s.operatorGet))
	s.mux.HandleFunc("GET /v1/operator/tasks/{id}/wait", s.requireOperator(s.operatorWait))
	s.mux.HandleFunc("POST /v1/operator/tasks/{id}/cancel", s.requireOperator(s.operatorCancel))
	s.mux.HandleFunc("POST /v1/operator/tasks/{id}/requeue", s.requireOperator(s.operatorRequeue))
	s.mux.HandleFunc("POST /v1/operator/tasks/{id}/resolve", s.requireOperator(s.operatorResolve))
	s.mux.HandleFunc("GET /v1/operator/tasks", s.requireOperator(s.operatorList))
	s.mux.HandleFunc("GET /v1/operator/devices", s.requireOperator(s.operatorListDevices))
	s.mux.HandleFunc("POST /v1/operator/devices/{id}/revoke", s.requireOperator(s.operatorRevoke))
	s.mux.HandleFunc("DELETE /v1/operator/devices/{id}", s.requireOperator(s.operatorDeleteDevice))
	s.mux.HandleFunc("POST /v1/operator/devices/{id}/delete", s.requireOperator(s.operatorDeleteDevice))
	s.mux.HandleFunc("POST /v1/operator/devices/{id}/restart", s.requireOperator(s.operatorRestartDevice))
	s.mux.HandleFunc("POST /v1/operator/devices/{id}/upgrade", s.requireOperator(s.operatorUpgradeDevice))
}
func (s *Server) operatorSubmit(w http.ResponseWriter, r *http.Request) {
	var in protocol.SubmitRequest
	if !readJSON(w, r, submitBodyLimit, &in) {
		return
	}
	if policy.Authorize(operatorPrincipal(r), "task.submit", in.NodeID) != nil {
		writeError(w, 403, "forbidden", "operation not permitted")
		return
	}
	task, err := s.store.Submit(r.Context(), in)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	s.notifyNode(in.NodeID)
	s.publishEvent(events.Event{
		Type:      events.TypeTaskSubmitted,
		TaskID:    task.ID,
		NodeID:    task.NodeID,
		State:     task.State,
		Timestamp: task.CreatedAt,
	})
	writeJSON(w, 201, task)
}
func (s *Server) operatorTask(w http.ResponseWriter, r *http.Request, action string) (protocol.Task, bool) {
	task, err := s.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		handleStoreError(w, err)
		return task, false
	}
	if policy.Authorize(operatorPrincipal(r), action, task.NodeID) != nil {
		writeError(w, 404, "not_found", "task not found")
		return protocol.Task{}, false
	}
	return task, true
}
func (s *Server) operatorGet(w http.ResponseWriter, r *http.Request) {
	if task, ok := s.operatorTask(w, r, "task.read"); ok {
		writeJSON(w, 200, task)
	}
}
func (s *Server) operatorCancel(w http.ResponseWriter, r *http.Request) {
	task, ok := s.operatorTask(w, r, "task.cancel")
	if !ok {
		return
	}
	task, err := s.store.Cancel(r.Context(), task.ID)
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
	writeJSON(w, 200, task)
}
func (s *Server) operatorRequeue(w http.ResponseWriter, r *http.Request) {
	task, ok := s.operatorTask(w, r, "task.submit")
	if !ok {
		return
	}
	requeued, err := s.store.Requeue(r.Context(), task.ID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	s.notifyNode(requeued.NodeID)
	s.publishEvent(events.Event{
		Type:      events.TypeTaskRequeued,
		TaskID:    requeued.ID,
		NodeID:    requeued.NodeID,
		State:     requeued.State,
		Timestamp: time.Now().UTC(),
	})
	writeJSON(w, 200, requeued)
}
func (s *Server) operatorResolve(w http.ResponseWriter, r *http.Request) {
	task, ok := s.operatorTask(w, r, "task.cancel")
	if !ok {
		return
	}
	var in struct {
		State     string `json:"state"`
		Text      string `json:"text"`
		ErrorCode string `json:"error_code"`
		ExitCode  int    `json:"exit_code"`
	}
	if !readJSON(w, r, credentialBodyLimit, &in) {
		return
	}
	res := protocol.Result{
		State:     protocol.State(in.State),
		Text:      in.Text,
		ErrorCode: in.ErrorCode,
		ExitCode:  in.ExitCode,
	}
	resolved, err := s.store.Resolve(r.Context(), task.ID, res)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	s.publishEvent(events.Event{
		Type:      events.TypeTaskResolved,
		TaskID:    resolved.ID,
		NodeID:    resolved.NodeID,
		State:     resolved.State,
		Timestamp: time.Now().UTC(),
	})
	writeJSON(w, 200, resolved)
}
func (s *Server) operatorList(w http.ResponseWriter, r *http.Request) {
	stateFilter := r.URL.Query().Get("state")
	p := operatorPrincipal(r)
	var tasks []protocol.Task
	var err error
	if stateFilter == "unknown" {
		tasks, err = s.store.ListUnknown(r.Context(), 100)
	} else if stateFilter == "" {
		tasks, err = s.store.List(r.Context(), 100)
	} else {
		writeError(w, 400, "invalid_argument", "only state=unknown is supported for filtering")
		return
	}
	if err != nil {
		handleStoreError(w, err)
		return
	}
	var filtered []protocol.Task
	for _, t := range tasks {
		if policy.Authorize(p, "task.read", t.NodeID) == nil {
			filtered = append(filtered, t)
		}
	}
	if filtered == nil {
		filtered = []protocol.Task{}
	}
	writeJSON(w, 200, filtered)
}
func (s *Server) operatorRevoke(w http.ResponseWriter, r *http.Request) {
	p := operatorPrincipal(r)
	if policy.Authorize(p, "device.revoke", r.PathValue("id")) != nil {
		writeError(w, 403, "forbidden", "operation not permitted")
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !readJSON(w, r, credentialBodyLimit, &in) {
		return
	}
	if s.devices == nil {
		writeError(w, 503, "unavailable", "device registry unavailable")
		return
	}
	if err := s.devices.Revoke(r.Context(), r.PathValue("id"), p.ID, in.Reason); err != nil {
		handleStoreError(w, err)
		return
	}
	s.notifyNode(r.PathValue("id"))
	w.WriteHeader(200)
}

func (s *Server) operatorDeleteDevice(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("id")
	p := operatorPrincipal(r)
	if policy.Authorize(p, "device.revoke", nodeID) != nil {
		writeError(w, 403, "forbidden", "operation not permitted")
		return
	}
	if s.devices == nil {
		writeError(w, 503, "unavailable", "device registry unavailable")
		return
	}
	if err := s.devices.Delete(r.Context(), nodeID); err != nil {
		handleStoreError(w, err)
		return
	}
	s.nodeRuntime.Delete(nodeID)
	s.notifyNode(nodeID)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Device deleted successfully",
		"node_id": nodeID,
	})
}

type OperatorDeviceView struct {
	NodeID      string                   `json:"node_id"`
	Fingerprint string                   `json:"fingerprint"`
	ExpiresAt   time.Time                `json:"cert_expires_at"`
	Revoked     bool                     `json:"revoked"`
	Version     string                   `json:"version,omitempty"`
	OS          string                   `json:"os,omitempty"`
	Arch        string                   `json:"arch,omitempty"`
	Hostname    string                   `json:"hostname,omitempty"`
	Status      string                   `json:"status,omitempty"`
	Agents      []protocol.AgentSoftware `json:"agents,omitempty"`
	Online      bool                     `json:"online"`
	StartedAt   int64                    `json:"started_at,omitempty"`
	LastSeen    *time.Time               `json:"last_seen,omitempty"`
}

func (s *Server) operatorListDevices(w http.ResponseWriter, r *http.Request) {
	if s.devices == nil {
		writeError(w, 503, "unavailable", "device registry unavailable")
		return
	}
	p := operatorPrincipal(r)
	devices, err := s.devices.List(r.Context())
	if err != nil {
		handleStoreError(w, err)
		return
	}

	now := time.Now().UTC()
	var views []OperatorDeviceView
	for _, d := range devices {
		if policy.Authorize(p, "task.read", d.NodeID) == nil {
			v := OperatorDeviceView{
				NodeID:      d.NodeID,
				Fingerprint: d.Fingerprint,
				ExpiresAt:   d.ExpiresAt,
				Revoked:     d.Revoked,
				Online:      false,
				Status:      "offline",
			}
			if val, ok := s.nodeRuntime.Load(d.NodeID); ok {
				if info, ok := val.(NodeRuntimeInfo); ok {
					v.Version = info.Version
					v.OS = info.OS
					v.Arch = info.Arch
					v.Hostname = info.Hostname
					v.Status = info.Status
					v.Agents = info.Agents
					v.StartedAt = info.StartedAt
					ls := info.LastSeen
					v.LastSeen = &ls
					if !d.Revoked && now.Sub(info.LastSeen) < 90*time.Second {
						v.Online = true
						if v.Status == "" {
							v.Status = "online"
						}
					} else {
						v.Status = "offline"
					}
				}
			}
			views = append(views, v)
		}
	}
	if views == nil {
		views = []OperatorDeviceView{}
	}
	writeJSON(w, 200, views)
}

func (s *Server) operatorRestartDevice(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("id")
	p := operatorPrincipal(r)
	if policy.Authorize(p, "task.submit", nodeID) != nil {
		writeError(w, 403, "forbidden", "operation not permitted")
		return
	}
	task, err := s.store.Submit(r.Context(), protocol.SubmitRequest{
		NodeID:            nodeID,
		Capability:        "agent.run",
		CapabilityVersion: 1,
		Input:             json.RawMessage(`{"prompt":"MESH_SYS:RESTART"}`),
		TimeoutSeconds:    60,
	})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if val, ok := s.nodeRuntime.Load(nodeID); ok {
		if info, ok := val.(NodeRuntimeInfo); ok {
			info.Status = "restarting"
			s.nodeRuntime.Store(nodeID, info)
			s.publishEvent(events.Event{
				Type:      events.TypeNodeHeartbeat,
				NodeID:    nodeID,
				Timestamp: time.Now().UTC(),
			})
		}
	}
	s.notifyNode(nodeID)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"task_id": task.ID,
		"message": "Node restart command dispatched",
	})
}

func (s *Server) operatorUpgradeDevice(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("id")
	p := operatorPrincipal(r)
	if policy.Authorize(p, "task.submit", nodeID) != nil {
		writeError(w, 403, "forbidden", "operation not permitted")
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	downloadURL := fmt.Sprintf("%s://%s/download/mesh", scheme, r.Host)
	if val, ok := s.nodeRuntime.Load(nodeID); ok {
		if info, ok := val.(NodeRuntimeInfo); ok && info.OS != "" && info.Arch != "" {
			downloadURL = fmt.Sprintf("%s://%s/download/mesh?arch=%s-%s", scheme, r.Host, info.OS, info.Arch)
		}
	}
	inputData, _ := json.Marshal(map[string]string{
		"prompt": fmt.Sprintf("MESH_SYS:UPGRADE %s", downloadURL),
	})
	task, err := s.store.Submit(r.Context(), protocol.SubmitRequest{
		NodeID:            nodeID,
		Capability:        "agent.run",
		CapabilityVersion: 1,
		Input:             json.RawMessage(inputData),
		TimeoutSeconds:    120,
	})
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if val, ok := s.nodeRuntime.Load(nodeID); ok {
		if info, ok := val.(NodeRuntimeInfo); ok {
			info.Status = "upgrading"
			s.nodeRuntime.Store(nodeID, info)
			s.publishEvent(events.Event{
				Type:      events.TypeNodeHeartbeat,
				NodeID:    nodeID,
				Timestamp: time.Now().UTC(),
			})
		}
	}
	s.notifyNode(nodeID)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"task_id": task.ID,
		"message": "Node upgrade command dispatched",
	})
}
