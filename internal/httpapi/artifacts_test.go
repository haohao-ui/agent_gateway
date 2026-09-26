package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"agent-gateway/internal/artifact"
	"agent-gateway/internal/identity"
	"agent-gateway/internal/taskstore"
)

func setupTestServerWithArtifacts(t *testing.T) (*Server, *httptest.Server, *artifact.Store, func()) {
	t.Helper()

	dir := t.TempDir()
	tasks, err := taskstore.Open(filepath.Join(dir, "tasks.sqlite"))
	if err != nil {
		t.Fatalf("open tasks: %v", err)
	}
	ca, err := identity.LoadOrGenerateCA(dir)
	if err != nil {
		t.Fatalf("open ca: %v", err)
	}
	artStore, err := artifact.Open(filepath.Join(dir, "artifacts"))
	if err != nil {
		t.Fatalf("open artifacts: %v", err)
	}

	api := NewServer(tasks, ca)
	api.SetArtifactStore(artStore)

	srv := httptest.NewServer(api.Handler())

	cleanup := func() {
		srv.Close()
		artStore.Close()
		tasks.Close()
	}

	return api, srv, artStore, cleanup
}

func TestArtifactEndpoints_UploadAndDownload(t *testing.T) {
	_, srv, _, cleanup := setupTestServerWithArtifacts(t)
	defer cleanup()

	taskID := "task-file-01"
	filename := "report.json"
	content := []byte(`{"status":"completed","tests_passed":42}`)

	hasher := sha256.New()
	hasher.Write(content)
	checksum := hex.EncodeToString(hasher.Sum(nil))

	// 1. Upload artifact
	uploadURL := srv.URL + "/v1/artifacts/" + taskID + "/" + filename
	req, err := http.NewRequest("PUT", uploadURL, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("X-Checksum-SHA256", checksum)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201 Created, got %d: %s", resp.StatusCode, string(body))
	}

	var info artifact.FileInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if info.Size != int64(len(content)) || info.SHA256 != checksum {
		t.Fatalf("unexpected file info: %+v", info)
	}

	// 2. Download entire artifact
	getResp, err := http.Get(uploadURL)
	if err != nil {
		t.Fatalf("download request: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", getResp.StatusCode)
	}
	downloaded, err := io.ReadAll(getResp.Body)
	if err != nil {
		t.Fatalf("read download body: %v", err)
	}
	if !bytes.Equal(downloaded, content) {
		t.Fatalf("content mismatch: got %q, want %q", downloaded, content)
	}

	// 3. HTTP Range Request (Partial Content: first 10 bytes)
	rangeReq, err := http.NewRequest("GET", uploadURL, nil)
	if err != nil {
		t.Fatalf("new range request: %v", err)
	}
	rangeReq.Header.Set("Range", "bytes=0-9")

	rangeResp, err := http.DefaultClient.Do(rangeReq)
	if err != nil {
		t.Fatalf("do range request: %v", err)
	}
	defer rangeResp.Body.Close()

	if rangeResp.StatusCode != http.StatusPartialContent {
		t.Fatalf("expected 206 Partial Content, got %d", rangeResp.StatusCode)
	}
	rangeBody, err := io.ReadAll(rangeResp.Body)
	if err != nil {
		t.Fatalf("read range body: %v", err)
	}
	if !bytes.Equal(rangeBody, content[:10]) {
		t.Fatalf("range body mismatch: got %q, want %q", rangeBody, content[:10])
	}
}

func TestArtifactEndpoints_ListAndDelete(t *testing.T) {
	_, srv, _, cleanup := setupTestServerWithArtifacts(t)
	defer cleanup()

	taskID := "task-file-02"
	filename := "log.txt"
	content := []byte("execution log line 1\nline 2\n")

	// 1. Upload
	uploadURL := srv.URL + "/v1/artifacts/" + taskID + "/" + filename
	req, _ := http.NewRequest("PUT", uploadURL, bytes.NewReader(content))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload failed: %v, code: %d", err, resp.StatusCode)
	}
	resp.Body.Close()

	// 2. List
	listResp, err := http.Get(srv.URL + "/v1/artifacts/" + taskID)
	if err != nil {
		t.Fatalf("list request: %v", err)
	}
	defer listResp.Body.Close()

	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", listResp.StatusCode)
	}
	var items []artifact.FileInfo
	if err := json.NewDecoder(listResp.Body).Decode(&items); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(items) != 1 || items[0].Name != filename {
		t.Fatalf("unexpected items: %+v", items)
	}

	// 3. Delete
	delReq, _ := http.NewRequest("DELETE", uploadURL, nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("delete request: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content, got %d", delResp.StatusCode)
	}

	// 4. Download after delete should return 404
	afterResp, err := http.Get(uploadURL)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	afterResp.Body.Close()
	if afterResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", afterResp.StatusCode)
	}
}

func TestArtifactEndpoints_ChecksumMismatch(t *testing.T) {
	_, srv, _, cleanup := setupTestServerWithArtifacts(t)
	defer cleanup()

	taskID := "task-file-03"
	filename := "bad.txt"
	content := []byte("good content")

	uploadURL := srv.URL + "/v1/artifacts/" + taskID + "/" + filename
	req, _ := http.NewRequest("PUT", uploadURL, bytes.NewReader(content))
	req.Header.Set("X-Checksum-SHA256", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on checksum mismatch, got %d", resp.StatusCode)
	}
}
