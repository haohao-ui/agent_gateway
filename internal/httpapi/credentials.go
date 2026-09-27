package httpapi

import (
	"agent-gateway/internal/policy"
	"net/http"
	"strings"
	"time"
)

type issueCredentialRequest struct {
	Role     string   `json:"role"`
	Nodes    []string `json:"nodes"`
	TTLHours int      `json:"ttl_hours"`
}

func (s *Server) issueCredential(w http.ResponseWriter, r *http.Request) {
	if policy.Authorize(operatorPrincipal(r), policy.ActionCredentialManage, "") != nil {
		writeError(w, http.StatusForbidden, "forbidden", "administrator required")
		return
	}
	var in issueCredentialRequest
	if r.Body != nil && r.ContentLength != 0 {
		if !readJSON(w, r, 64*1024, &in) {
			return
		}
	}
	roleStr := strings.ToLower(strings.TrimSpace(in.Role))
	if roleStr == "" {
		roleStr = "admin"
	}
	role := policy.Role(roleStr)
	if role != policy.Admin && role != policy.Operator && role != policy.Viewer {
		writeError(w, http.StatusBadRequest, "invalid_role", "role must be admin, operator, or viewer")
		return
	}

	ttlHours := in.TTLHours
	if ttlHours <= 0 {
		ttlHours = 720 // 默认 30 天
	}
	if ttlHours > 8760 {
		ttlHours = 8760 // 最长 1 年 (365天)
	}

	expiresAt := time.Now().Add(time.Duration(ttlHours) * time.Hour)
	p, token, err := s.policies.Issue(r.Context(), role, in.Nodes, expiresAt)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"principal_id": p.ID,
		"role":         p.Role,
		"node_ids":     p.NodeIDs,
		"token":        token,
		"expires_at":   expiresAt.Format(time.RFC3339),
	})
}

func (s *Server) listCredentials(w http.ResponseWriter, r *http.Request) {
	if policy.Authorize(operatorPrincipal(r), policy.ActionCredentialManage, "") != nil {
		writeError(w, 403, "forbidden", "administrator required")
		return
	}
	items, err := s.policies.Credentials(r.Context(), r.URL.Query().Get("after"), 100)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	next := ""
	if len(items) == 100 {
		next = items[len(items)-1].ID
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"credentials": items, "next_cursor": next, "current_principal_id": operatorPrincipal(r).ID})
}
func (s *Server) revokeCredential(w http.ResponseWriter, r *http.Request) {
	if policy.Authorize(operatorPrincipal(r), policy.ActionCredentialManage, "") != nil {
		writeError(w, 403, "forbidden", "administrator required")
		return
	}
	id := r.PathValue("id")
	if err := s.policies.Revoke(r.Context(), id); err != nil {
		handleStoreError(w, err)
		return
	}
	s.closeStreams(id)
	w.WriteHeader(http.StatusNoContent)
}
