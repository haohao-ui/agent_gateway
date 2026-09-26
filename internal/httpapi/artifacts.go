package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"agent-gateway/internal/artifact"
	"agent-gateway/internal/policy"
)

const artifactBodyLimit = 64 << 20 // 64 MiB per artifact upload limit

// SetArtifactStore configures the sandboxed file storage for the server.
func (s *Server) SetArtifactStore(store *artifact.Store) {
	s.artifacts = store
}

// authorizeArtifactAccess checks if the caller (node via mTLS or operator via Bearer) is permitted to access task artifacts.
func (s *Server) authorizeArtifactAccess(w http.ResponseWriter, r *http.Request, taskID string, action string) bool {
	if s.policies == nil && r.TLS == nil {
		return true // testing mode without security stores
	}

	// 1. Try Operator auth first if Authorization header is present
	if authHeader := r.Header.Get("Authorization"); authHeader != "" {
		fields := strings.Fields(authHeader)
		if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") && s.policies != nil {
			p, err := s.policies.Authenticate(r.Context(), fields[1])
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", "invalid operator credential")
				return false
			}
			task, err := s.store.Get(r.Context(), taskID)
			if err != nil {
				handleStoreError(w, err)
				return false
			}
			if err := policy.Authorize(p, action, task.NodeID); err != nil {
				writeError(w, http.StatusForbidden, "forbidden", "operation not permitted on this task")
				return false
			}
			return true
		}
	}

	// 2. Otherwise try Node mTLS auth
	caller := nodeIDFromContext(r.Context())
	if caller != "" {
		if _, ok := s.authorizeTask(r.Context(), w, taskID, caller); !ok {
			return false
		}
		return true
	}

	writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
	return false
}

func (s *Server) registerArtifactRoutes() {
	s.mux.HandleFunc("PUT /v1/artifacts/{id}/{filename}", s.handleUploadArtifact)
	s.mux.HandleFunc("GET /v1/artifacts/{id}/{filename}", s.handleDownloadArtifact)
	s.mux.HandleFunc("GET /v1/artifacts/{id}", s.handleListArtifacts)
	s.mux.HandleFunc("DELETE /v1/artifacts/{id}/{filename}", s.handleDeleteArtifact)
}

func (s *Server) handleUploadArtifact(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "artifact storage not configured")
		return
	}
	taskID := r.PathValue("id")
	filename := r.PathValue("filename")

	if !s.authorizeArtifactAccess(w, r, taskID, "task.submit") {
		return
	}

	expectedSHA := r.Header.Get("X-Checksum-SHA256")
	if expectedSHA == "" {
		// Also support Digest header: sha-256=<hex>
		digest := r.Header.Get("Digest")
		if strings.HasPrefix(strings.ToLower(digest), "sha-256=") {
			expectedSHA = digest[8:]
		}
	}

	// Limit upload size
	limitedReader := http.MaxBytesReader(w, r.Body, artifactBodyLimit)
	defer limitedReader.Close()

	info, err := s.artifacts.Save(r.Context(), taskID, filename, limitedReader, expectedSHA)
	if err != nil {
		if errors.Is(err, artifact.ErrChecksumFail) {
			writeError(w, http.StatusBadRequest, "checksum_mismatch", err.Error())
			return
		}
		if errors.Is(err, artifact.ErrInvalidPath) {
			writeError(w, http.StatusBadRequest, "invalid_path", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "upload_failed", err.Error())
		return
	}

	w.Header().Set("ETag", fmt.Sprintf("%q", info.SHA256))
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) handleDownloadArtifact(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "artifact storage not configured")
		return
	}
	taskID := r.PathValue("id")
	filename := r.PathValue("filename")

	if !s.authorizeArtifactAccess(w, r, taskID, "task.read") {
		return
	}

	f, info, err := s.artifacts.Open(r.Context(), taskID, filename)
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "artifact not found")
			return
		}
		if errors.Is(err, artifact.ErrInvalidPath) {
			writeError(w, http.StatusBadRequest, "invalid_path", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "read_failed", err.Error())
		return
	}
	defer f.Close()

	w.Header().Set("ETag", fmt.Sprintf("%q", info.SHA256))
	http.ServeContent(w, r, info.Name, info.UpdatedAt, f)
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "artifact storage not configured")
		return
	}
	taskID := r.PathValue("id")
	if !s.authorizeArtifactAccess(w, r, taskID, "task.read") {
		return
	}

	items, err := s.artifacts.List(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleDeleteArtifact(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "artifact storage not configured")
		return
	}
	taskID := r.PathValue("id")
	filename := r.PathValue("filename")

	if !s.authorizeArtifactAccess(w, r, taskID, "task.cancel") {
		return
	}

	if err := s.artifacts.Delete(r.Context(), taskID, filename); err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "artifact not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
