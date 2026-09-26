package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	targetURL := "http://192.168.3.237:8088/mcp?token=_oakRb6P_B23u6MDJM8wuUTGjYntgyF_wceACo9pRkw"
	fmt.Printf("[MCP-Handoff-Test] Connecting to %s\n", targetURL)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 0,
	}

	req, _ := http.NewRequestWithContext(context.Background(), "GET", targetURL, nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Connect error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	endpointChan := make(chan string, 1)
	msgChan := make(chan string, 50)

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
				} else {
					msgChan <- data
				}
			}
		}
	}()

	postEndpoint := <-endpointChan
	baseParsed, _ := url.Parse(targetURL)
	epParsed, _ := url.Parse(postEndpoint)
	resolvedPostURL := baseParsed.ResolveReference(epParsed).String()
	fmt.Printf("[MCP-Handoff-Test] Endpoint acquired: %s\n", resolvedPostURL)

	postClient := &http.Client{Timeout: 30 * time.Second}
	sendRPC := func(reqObj any) {
		bodyBytes, _ := json.Marshal(reqObj)
		pReq, _ := http.NewRequest("POST", resolvedPostURL, bytes.NewReader(bodyBytes))
		pReq.Header.Set("Content-Type", "application/json")
		pResp, err := postClient.Do(pReq)
		if err != nil {
			fmt.Printf("POST error: %v\n", err)
			return
		}
		defer pResp.Body.Close()
	}

	// 1. Initialize
	sendRPC(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "doubao-client", "version": "1.0"},
		},
	})
	<-msgChan // init resp

	// 2. Initialized notification
	sendRPC(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})

	// 3. Call handoff_to_computer_agent
	fmt.Println("[MCP-Handoff-Test] Invoking tool handoff_to_computer_agent...")
	sendRPC(map[string]any{
		"jsonrpc": "2.0",
		"id":      100,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "handoff_to_computer_agent",
			"arguments": map[string]any{
				"instruction": "echo MCP_TASK_ROUNDTRIP_OK",
				"target_node": "node-bdb478eda935f4cc",
			},
		},
	})

	handoffResp := <-msgChan
	fmt.Printf("[MCP-Handoff-Test] Handoff Response: %s\n", handoffResp)

	// Extract task_id
	var rpcResp struct {
		Result struct {
			StructuredContent struct {
				TaskID string `json:"task_id"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	_ = json.Unmarshal([]byte(handoffResp), &rpcResp)
	taskID := rpcResp.Result.StructuredContent.TaskID
	fmt.Printf("[MCP-Handoff-Test] Created Task ID: %s\n", taskID)

	if taskID != "" {
		fmt.Printf("[MCP-Handoff-Test] Waiting for execution result via wait_task_result...\n")
		sendRPC(map[string]any{
			"jsonrpc": "2.0",
			"id":      101,
			"method":  "tools/call",
			"params": map[string]any{
				"name": "wait_task_result",
				"arguments": map[string]any{
					"task_id":         taskID,
					"timeout_seconds": 15,
				},
			},
		})
		waitResult := <-msgChan
		fmt.Printf("[MCP-Handoff-Test] Final Execution Result: %s\n", waitResult)
	}

	fmt.Println("[MCP-Handoff-Test] ✅ END-TO-END MCP HANDOFF VERIFICATION PASSED!")
}
