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
	"strings"
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
	return s.registerCertificateWithDevice(ctx, id, certPEM, "", "")
}

func (s *Server) registerCertificateWithDevice(ctx context.Context, id string, certPEM []byte, machineID, hostname string) error {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return errors.New("invalid certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	return s.devices.RegisterWithDevice(ctx, id, certificateFingerprint(cert), machineID, hostname, cert.NotAfter)
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

// credentialContextKey carries the credential that authenticated the principal
// so streaming handlers re-validate the same one instead of re-parsing headers.
const credentialContextKey contextKey = "credential"

func (s *Server) requireOperator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if (r.TLS == nil && !s.IsHTTPAllowed()) || s.policies == nil {
			writeError(w, 401, "unauthorized", "HTTPS operator authentication required")
			return
		}
		tokens := operatorTokens(r)
		if len(tokens) == 0 {
			writeError(w, 401, "unauthorized", "operator credential required")
			return
		}
		p, credential, err := s.authenticateOperator(r.Context(), tokens)
		if err != nil {
			writeError(w, 401, "unauthorized", "invalid operator credential")
			return
		}
		ctx := context.WithValue(r.Context(), principalContextKey, p)
		ctx = context.WithValue(ctx, credentialContextKey, credential)
		next(w, r.WithContext(ctx))
	}
}
func operatorPrincipal(r *http.Request) policy.Principal {
	p, _ := r.Context().Value(principalContextKey).(policy.Principal)
	return p
}

// operatorCredential returns the credential that authenticated the request.
func operatorCredential(r *http.Request) string {
	c, _ := r.Context().Value(credentialContextKey).(string)
	return c
}
func (s *Server) registerOperatorRoutes() {
	s.mux.HandleFunc("GET /v1/operator/credentials", s.requireOperator(s.listCredentials))
	s.mux.HandleFunc("POST /v1/operator/credentials", s.requireOperator(s.issueCredential))
	s.mux.HandleFunc("POST /v1/operator/credentials/{id}/revoke", s.requireOperator(s.revokeCredential))
	s.mux.HandleFunc("GET /api/operator/credentials", s.requireOperator(s.listCredentials))
	s.mux.HandleFunc("POST /api/operator/credentials", s.requireOperator(s.issueCredential))
	s.mux.HandleFunc("POST /api/operator/credentials/{id}/revoke", s.requireOperator(s.revokeCredential))
	s.mux.HandleFunc("POST /v1/operator/tasks", s.requireOperator(s.operatorSubmit))
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
	s.mux.HandleFunc("POST /v1/operator/devices/{id}/tools", s.requireOperator(s.operatorSetDeviceTools))
	s.mux.HandleFunc("PUT /v1/operator/devices/{id}/tools", s.requireOperator(s.operatorSetDeviceTools))
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
	// Tool / Capability execution authorization check
	if s.devices != nil && in.NodeID != "" {
		allowedTools, err := s.devices.GetAllowedTools(r.Context(), in.NodeID)
		if err == nil && len(allowedTools) > 0 {
			permitted := false
			for _, t := range allowedTools {
				if t == in.Capability || t == "*" {
					permitted = true
					break
				}
			}
			// Special built-in maintenance capabilities are allowed
			if !permitted && in.Capability != "agent.run" {
				writeError(w, 403, "forbidden_capability", fmt.Sprintf("工具链 %q 尚未在目标节点获得授权执行，请在控制台勾选授权", in.Capability))
				return
			}
		}
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
	NodeID       string                   `json:"node_id"`
	Fingerprint  string                   `json:"fingerprint"`
	ExpiresAt    time.Time                `json:"cert_expires_at"`
	Revoked      bool                     `json:"revoked"`
	Version      string                   `json:"version,omitempty"`
	OS           string                   `json:"os,omitempty"`
	Arch         string                   `json:"arch,omitempty"`
	Hostname     string                   `json:"hostname,omitempty"`
	MachineID    string                   `json:"machine_id,omitempty"`
	Status       string                   `json:"status,omitempty"`
	Agents       []protocol.AgentSoftware `json:"agents,omitempty"`
	AllowedTools []string                 `json:"allowed_tools"`
	Online       bool                     `json:"online"`
	StartedAt    int64                    `json:"started_at,omitempty"`
	LastSeen     *time.Time               `json:"last_seen,omitempty"`
}

// deduplicateDevices ensures that one physical machine only appears once in the device list.
// If multiple nodes exist for the same machine (same MachineID or Hostname), the latest active
// one is kept, preventing duplicate ghost rows when nodes re-pair.
func deduplicateDevices(views []OperatorDeviceView) []OperatorDeviceView {
	if len(views) <= 1 {
		return views
	}

	machineKey := func(v OperatorDeviceView) string {
		if strings.TrimSpace(v.MachineID) != "" {
			return "mach:" + strings.TrimSpace(v.MachineID)
		}
		h := strings.TrimSpace(strings.ToLower(v.Hostname))
		if h != "" && h != "linux" && h != "darwin" && h != "windows" && h != "unknown-host" {
			return "host:" + h
		}
		return "node:" + v.NodeID
	}

	grouped := make(map[string][]OperatorDeviceView)
	var keysInOrder []string
	for _, v := range views {
		k := machineKey(v)
		if _, exists := grouped[k]; !exists {
			keysInOrder = append(keysInOrder, k)
		}
		grouped[k] = append(grouped[k], v)
	}

	var result []OperatorDeviceView
	for _, k := range keysInOrder {
		group := grouped[k]
		if len(group) == 1 {
			result = append(result, group[0])
			continue
		}

		best := group[0]
		for i := 1; i < len(group); i++ {
			cand := group[i]
			if best.Revoked && !cand.Revoked {
				best = cand
				continue
			}
			if !best.Revoked && cand.Revoked {
				continue
			}
			if !best.Online && cand.Online {
				best = cand
				continue
			}
			if best.Online && !cand.Online {
				continue
			}
			candTime := int64(0)
			if cand.LastSeen != nil {
				candTime = cand.LastSeen.Unix()
			} else {
				candTime = cand.StartedAt
			}
			bestTime := int64(0)
			if best.LastSeen != nil {
				bestTime = best.LastSeen.Unix()
			} else {
				bestTime = best.StartedAt
			}
			if candTime >= bestTime {
				best = cand
			}
		}
		result = append(result, best)
	}
	return result
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
			tools := d.AllowedTools
			if tools == nil {
				tools = []string{}
			}
			v := OperatorDeviceView{
				NodeID:       d.NodeID,
				Fingerprint:  d.Fingerprint,
				ExpiresAt:    d.ExpiresAt,
				Revoked:      d.Revoked,
				MachineID:    d.MachineID,
				Hostname:     d.Hostname,
				AllowedTools: tools,
				Online:       false,
				Status:       "offline",
			}
			if val, ok := s.nodeRuntime.Load(d.NodeID); ok {
				if info, ok := val.(NodeRuntimeInfo); ok {
					v.Version = info.Version
					v.OS = info.OS
					v.Arch = info.Arch
					if info.Hostname != "" {
						v.Hostname = info.Hostname
					}
					if info.MachineID != "" {
						v.MachineID = info.MachineID
					}
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
	views = deduplicateDevices(views)
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
	customURL := r.URL.Query().Get("url")
	promptCmd := "MESH_SYS:UPGRADE"
	if customURL != "" {
		promptCmd = fmt.Sprintf("MESH_SYS:UPGRADE %s", customURL)
	}
	inputData, _ := json.Marshal(map[string]string{
		"prompt": promptCmd,
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

func (s *Server) operatorSetDeviceTools(w http.ResponseWriter, r *http.Request) {
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
	var in struct {
		Tools []string `json:"tools"`
	}
	if !readJSON(w, r, credentialBodyLimit, &in) {
		return
	}
	if in.Tools == nil {
		in.Tools = []string{}
	}
	if err := s.devices.SetAllowedTools(r.Context(), nodeID, in.Tools); err != nil {
		handleStoreError(w, err)
		return
	}
	s.publishEvent(events.Event{
		Type:      "device.tools_updated",
		NodeID:    nodeID,
		Timestamp: time.Now().UTC(),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"success":       true,
		"node_id":       nodeID,
		"allowed_tools": in.Tools,
	})
}
