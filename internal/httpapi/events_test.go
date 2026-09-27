package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-gateway/internal/events"
	"agent-gateway/internal/identity"
	"agent-gateway/internal/policy"
	"agent-gateway/internal/protocol"
	"agent-gateway/internal/taskstore"
)

func TestWebUI_StaticAssets(t *testing.T) {
	dir := t.TempDir()
	store, err := taskstore.Open(filepath.Join(dir, "tasks.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ca, err := identity.LoadOrGenerateCA(dir)
	if err != nil {
		t.Fatalf("create ca: %v", err)
	}

	api := NewServer(store, ca)
	server := httptest.NewServer(api.Handler())
	defer server.Close()

	// 1. Root redirects to /ui/
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("expected 302 found for root, got %d", resp.StatusCode)
	}

	// 2. /ui/ serves index.html
	resp, err = http.Get(server.URL + "/ui/")
	if err != nil {
		t.Fatalf("get /ui/: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for /ui/, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Agent Gateway Console") {
		t.Errorf("missing title in index.html body: %s", string(body[:min(200, len(body))]))
	}

	// 3. /ui/style.css
	resp, err = http.Get(server.URL + "/ui/style.css")
	if err != nil {
		t.Fatalf("get style.css: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for style.css, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "--bg-main") {
		t.Error("unexpected style.css content")
	}

	// 4. /ui/app.js
	resp, err = http.Get(server.URL + "/ui/app.js")
	if err != nil {
		t.Fatalf("get app.js: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for app.js, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "EventSource") {
		t.Error("unexpected app.js content")
	}
}

func TestSSE_StreamSubscriptionAndEvents(t *testing.T) {
	api, server, _ := secureFixture(t)
	token := issueRole(t, api, policy.Admin)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/v1/events/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("connect sse: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", contentType)
	}

	reader := bufio.NewReader(resp.Body)

	// 1. First event is connected
	var eventType, eventData string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream line: %v", err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			eventData = strings.TrimPrefix(line, "data: ")
		} else if line == "" && eventData != "" {
			break
		}
	}
	if eventType != "connected" {
		t.Errorf("expected event: connected, got %s", eventType)
	}

	// 2. Publish a custom event via Hub and verify receipt
	testEvt := events.Event{
		Type:   events.TypeTaskSubmitted,
		TaskID: "task-sse-123",
		State:  protocol.Queued,
	}
	api.Hub().Publish(testEvt)

	var receivedData string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream line: %v", err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data: ") {
			receivedData = strings.TrimPrefix(line, "data: ")
		} else if line == "" && receivedData != "" {
			break
		}
	}

	var parsed events.Event
	if err := json.Unmarshal([]byte(receivedData), &parsed); err != nil {
		t.Fatalf("unmarshal received event: %v", err)
	}
	if parsed.TaskID != "task-sse-123" || parsed.Type != events.TypeTaskSubmitted {
		t.Errorf("unexpected event received: %+v", parsed)
	}
}

func TestWebDoctor_Endpoint(t *testing.T) {
	dir := t.TempDir()
	store, err := taskstore.Open(filepath.Join(dir, "tasks.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ca, err := identity.LoadOrGenerateCA(dir)
	if err != nil {
		t.Fatalf("create ca: %v", err)
	}

	api := NewServer(store, ca)
	api.SetDoctorFunc(func(ctx context.Context) any {
		return map[string]any{
			"mode":    "test",
			"healthy": true,
			"score":   100,
		}
	})

	server := httptest.NewServer(api.Handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/v1/doctor")
	if err != nil {
		t.Fatalf("get /v1/doctor: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("decode doctor response: %v", err)
	}
	if data["mode"] != "test" || data["healthy"] != true {
		t.Errorf("unexpected doctor output: %+v", data)
	}
}
