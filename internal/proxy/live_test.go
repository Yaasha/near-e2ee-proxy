//go:build live

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"near-e2ee-proxy/internal/near"
)

// This suite has exactly four short synthetic inference requests, no automatic
// inference retries and no prompt/response logging. It is never part of CI/check.
func TestLiveChatProxy(t *testing.T) {
	if os.Getenv("NEAR_LIVE_TEST") != "1" {
		t.Skip("explicit live-test opt-in required")
	}
	key := os.Getenv("NEAR_AI_API_KEY")
	if key == "" {
		t.Fatal("cannot load live-test credential")
	}
	backend, err := near.New(key)
	if err != nil {
		t.Fatal("cannot initialize NEAR client")
	}
	defer backend.Close()
	model := "Qwen/Qwen3.8-27B"
	// The selected model is deliberately outside the discovery list. The live
	// SDK must admit that requested model rather than consulting a static gate.
	server := httptest.NewServer(NewServer(Config{ProxyKey: testKey, Models: []string{"synthetic-discovery-entry"}}, backend))
	defer server.Close()
	client := &http.Client{Timeout: RequestTimeout + time.Second}
	post := func(t *testing.T, input map[string]any) []byte {
		t.Helper()
		input["model"] = model
		input["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
		input["max_completion_tokens"] = 128
		raw, _ := json.Marshal(input)
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		defer cancel()
		r, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/chat/completions", bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+testKey)
		r.Header.Set("Content-Type", "application/json")
		response, err := client.Do(r)
		if err != nil {
			t.Fatal("live HTTP request failed")
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
		if err != nil || len(body) > MaxResponseBytes {
			t.Fatal("live response read failed")
		}
		if response.StatusCode != 200 {
			var safe struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			json.Unmarshal(body, &safe)
			// The local proxy emits only fixed safe codes. Never print the body.
			t.Fatalf("live HTTP status %d, proxy code %s", response.StatusCode, safe.Error.Code)
		}
		return body
	}
	message := map[string]any{"role": "user", "content": "Reply with the single word OK."}
	t.Run("json", func(t *testing.T) {
		body := post(t, map[string]any{"messages": []any{message}})
		completion, err := parseCompletion(body, model)
		if err != nil || !bytes.Contains(completion.Choices[0].Message.Content, []byte("OK")) {
			t.Fatal("expected verified Chat JSON answer")
		}
	})
	t.Run("sse", func(t *testing.T) {
		body := post(t, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "List twenty short arithmetic facts, one per line."}}, "stream": true, "stream_options": map[string]any{"include_usage": true}})
		textEvents := 0
		finished, hasUsage := false, false
		for _, chunk := range liveChunks(t, body) {
			if chunk.Usage != nil {
				hasUsage = true
			}
			for _, choice := range chunk.Choices {
				if choice.Delta.Content != nil && *choice.Delta.Content != "" {
					textEvents++
				}
				// Reaching the output cap may finish with length instead of stop.
				if choice.FinishReason != nil {
					finished = *choice.FinishReason == "stop" || *choice.FinishReason == "length"
				}
			}
		}
		if textEvents < 2 || !finished || !hasUsage {
			t.Fatal("expected incremental native text, verified finish and usage")
		}
		t.Logf("verified HTTP stream: %d text chunks", textEvents)
	})
	t.Run("tool_loop", func(t *testing.T) {
		tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "echo_value", "description": "Echo the supplied value.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}, "required": []string{"value"}, "additionalProperties": false}}}}
		user := map[string]any{"role": "user", "content": "Call echo_value with value hello."}
		choice := map[string]any{"type": "function", "function": map[string]any{"name": "echo_value"}}
		body := post(t, map[string]any{"messages": []any{user}, "tools": tools, "tool_choice": choice, "stream": true})
		assistant := Message{Role: "assistant"}
		for _, chunk := range liveChunks(t, body) {
			for _, c := range chunk.Choices {
				for _, call := range c.Delta.ToolCalls {
					assistant.ToolCalls = append(assistant.ToolCalls, ToolCall{ID: call.ID, Type: call.Type, Function: CallFunction{Name: call.Function.Name, Arguments: call.Function.Arguments}})
				}
			}
		}
		calls := assistant.ToolCalls
		if len(calls) != 1 || calls[0].Function.Name != "echo_value" {
			t.Fatal("expected decrypted tool name")
		}
		var arguments struct {
			Value string `json:"value"`
		}
		if json.Unmarshal([]byte(calls[0].Function.Arguments), &arguments) != nil || arguments.Value != "hello" {
			t.Fatal("expected decrypted tool arguments")
		}
		// The synthetic tool runs here on the trusted test client, never at NEAR.
		result := map[string]any{"role": "tool", "tool_call_id": calls[0].ID, "content": "{\"value\":\"hello\"}"}
		body = post(t, map[string]any{"messages": []any{user, assistant, result}, "tools": tools, "tool_choice": "none"})
		completion, err := parseCompletion(body, model)
		if err != nil || completion.Choices[0].FinishReason != "stop" || !strings.Contains(strings.ToLower(string(completion.Choices[0].Message.Content)), "hello") {
			t.Fatal("expected a verified reply after the encrypted tool result")
		}
	})
}

// Never print live bodies, even on assertion failure. Parse only our local SSE
// contract; tools here are already assembled and authenticated by the proxy.
func liveChunks(t *testing.T, body []byte) []streamChunk {
	t.Helper()
	if !bytes.HasSuffix(body, []byte("data: [DONE]\n\n")) {
		t.Fatal("expected verified terminal marker")
	}
	var chunks []streamChunk
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
			var chunk streamChunk
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) != nil {
				t.Fatal("invalid local SSE JSON")
			}
			chunks = append(chunks, chunk)
		}
	}
	if scanner.Err() != nil {
		t.Fatal("cannot parse local SSE")
	}
	return chunks
}
