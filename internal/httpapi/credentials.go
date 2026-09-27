package httpapi

import (
	"agent-gateway/internal/policy"
	"net/http"
)

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
