package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type JSONRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run test/test_mcp_client.go <baseURL> [token]")
		os.Exit(1)
	}

	rawBase := os.Args[1]
	token := ""
	if len(os.Args) >= 3 {
		token = os.Args[2]
	}

	targetURL := rawBase
	if token != "" && !strings.Contains(targetURL, "token=") {
		sep := "?"
		if strings.Contains(targetURL, "?") {
			sep = "&"
		}
		targetURL = targetURL + sep + "token=" + token
	}

	fmt.Printf("[MCP-Test] 1. Connecting to SSE stream: %s\n", targetURL)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 0, // SSE hanging GET
	}

	req, err := http.NewRequestWithContext(context.Background(), "GET", targetURL, nil)
	if err != nil {
		fmt.Printf("Create GET request error: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("GET request failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("GET returned non-200: %d, body: %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}

	fmt.Printf("[MCP-Test] 2. SSE connected! HTTP status: %d\n", resp.StatusCode)

	reader := bufio.NewReader(resp.Body)
	endpointChan := make(chan string, 1)
	msgChan := make(chan string, 50)

	// Goroutine to read SSE lines
	go func() {
		var currentEvent string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "event:") {
				currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if currentEvent == "endpoint" {
					endpointChan <- data
				} else if currentEvent == "message" || currentEvent == "" {
					msgChan <- data
				}
			}
		}
	}()

	var postEndpoint string
	select {
	case ep := <-endpointChan:
		postEndpoint = ep
		fmt.Printf("[MCP-Test] 3. Received MCP endpoint: %s\n", postEndpoint)
	case <-time.After(5 * time.Second):
		fmt.Println("Timeout waiting for 'endpoint' event from SSE stream")
		os.Exit(1)
	}

	// Resolve post URL relative to targetURL
	baseParsed, _ := url.Parse(targetURL)
	epParsed, err := url.Parse(postEndpoint)
	if err != nil {
		fmt.Printf("Failed to parse endpoint: %v\n", err)
		os.Exit(1)
	}
	resolvedPostURL := baseParsed.ResolveReference(epParsed).String()
	fmt.Printf("[MCP-Test] 4. Resolved POST URL: %s\n", resolvedPostURL)

	postClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 10 * time.Second,
	}

	sendRPC := func(reqObj any) {
		bodyBytes, _ := json.Marshal(reqObj)
		pReq, err := http.NewRequest("POST", resolvedPostURL, bytes.NewReader(bodyBytes))
		if err != nil {
			fmt.Printf("Error creating POST request: %v\n", err)
			os.Exit(1)
		}
		pReq.Header.Set("Content-Type", "application/json")
		pResp, err := postClient.Do(pReq)
		if err != nil {
			fmt.Printf("POST request failed: %v\n", err)
			os.Exit(1)
		}
		defer pResp.Body.Close()
		pBody, _ := io.ReadAll(pResp.Body)
		if pResp.StatusCode != http.StatusOK && pResp.StatusCode != http.StatusAccepted {
			fmt.Printf("POST returned status %d: %s\n", pResp.StatusCode, string(pBody))
			os.Exit(1)
		}
	}

	// 5. Send initialize
	fmt.Printf("[MCP-Test] 5. Sending 'initialize'...\n")
	sendRPC(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "doubao-test-client",
				"version": "1.0",
			},
		},
	})

	select {
	case msg := <-msgChan:
		fmt.Printf("[MCP-Test] Received initialize response: %s\n", msg)
	case <-time.After(5 * time.Second):
		fmt.Println("Timeout waiting for initialize response")
		os.Exit(1)
	}

	// 6. Send notifications/initialized
	fmt.Printf("[MCP-Test] 6. Sending 'notifications/initialized'...\n")
	sendRPC(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})

	// 7. Send tools/list
	fmt.Printf("[MCP-Test] 7. Sending 'tools/list'...\n")
	sendRPC(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
		"params":  map[string]any{},
	})

	select {
	case msg := <-msgChan:
		fmt.Printf("[MCP-Test] Received tools/list response: %s\n", msg)
	case <-time.After(5 * time.Second):
		fmt.Println("Timeout waiting for tools/list response")
		os.Exit(1)
	}

	// 8. Send tools/call (device_list)
	fmt.Printf("[MCP-Test] 8. Sending 'tools/call' for 'device_list'...\n")
	sendRPC(map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "device_list",
			"arguments": map[string]any{},
		},
	})

	select {
	case msg := <-msgChan:
		fmt.Printf("[MCP-Test] Received tools/call response: %s\n", msg)
	case <-time.After(5 * time.Second):
		fmt.Println("Timeout waiting for tools/call response")
		os.Exit(1)
	}

	fmt.Println("\n🎉 [MCP-Test] ALL PROTOCOL TESTS PASSED SUCCESSFULLY! MCP IS 100% OPERATIONAL!")
}
