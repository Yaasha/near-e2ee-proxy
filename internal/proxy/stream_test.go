package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func syntheticChunk(delta string, finish string) json.RawMessage {
	return json.RawMessage(`{"id":"chatcmpl-synthetic","object":"chat.completion.chunk","created":1,"model":"synthetic-model","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + finish + `}]}`)
}

func TestStreamTextBeforeReceiptToolsAfterReceipt(t *testing.T) {
	for _, verified := range []bool{true, false} {
		t.Run(map[bool]string{true: "verified", false: "failed"}[verified], func(t *testing.T) {
			w := httptest.NewRecorder()
			b := &fakeBackend{stream: func(ctx context.Context, raw json.RawMessage, emit func(json.RawMessage) error) error {
				var input Request
				if json.Unmarshal(raw, &input) != nil || !input.Stream {
					t.Fatal("native stream intent must reach backend")
				}
				chunks := []json.RawMessage{
					syntheticChunk(`{"role":"assistant","content":"preview","reasoning_content":"reason"}`, `null`),
					syntheticChunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"look","arguments":"{\"value\":"}}]}`, `null`),
					syntheticChunk(`{"tool_calls":[{"index":0,"function":{"name":"up","arguments":"1}"}}]}`, `"tool_calls"`),
				}
				for _, chunk := range chunks {
					if err := emit(chunk); err != nil {
						return err
					}
				}
				if err := emit(json.RawMessage(`{"id":"chatcmpl-synthetic","object":"chat.completion.chunk","created":1,"model":"synthetic-model","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)); err != nil {
					return err
				}
				// Inspect synchronously inside the backend, before it has authorized
				// success. This avoids races with ResponseRecorder's plain buffer.
				body := w.Body.String()
				if !w.Flushed || !strings.Contains(body, `"content":"preview"`) || !strings.Contains(body, `"reasoning_content":"reason"`) {
					t.Fatal("text must flush before verification")
				}
				for _, secret := range []string{"call_1", "lookup", "tool_calls", "total_tokens", "[DONE]", `"finish_reason":"`} {
					if strings.Contains(body, secret) {
						t.Fatal("tools and terminal metadata must wait for receipt")
					}
				}
				if !verified {
					return errors.New("synthetic-private-receipt-error")
				}
				return nil
			}}
			testServer(b).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", strings.TrimSuffix(goodRequest, "}")+`,"stream":true,"stream_options":{"include_usage":true}}`))
			body := w.Body.String()
			if w.Code != 200 || w.Header().Get("X-Response-Verification") != "end-of-stream" {
				t.Fatal("stream must declare deferred verification")
			}
			if verified {
				if !strings.Contains(body, `"name":"lookup"`) || !strings.Contains(body, `"arguments":"{\"value\":1}"`) || !strings.Contains(body, `"total_tokens":4`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
					t.Fatal("verified success must release assembled tools, usage and completion")
				}
			} else {
				if !strings.Contains(body, "event: error\n") || strings.Contains(body, "synthetic-private") {
					t.Fatal("stream failure must be explicit and sanitized")
				}
				for _, forbidden := range []string{"call_1", "lookup", "tool_calls", "total_tokens", "[DONE]", `"finish_reason":"`} {
					if strings.Contains(body, forbidden) {
						t.Fatal("failed receipt must never release tools or success")
					}
				}
			}
		})
	}
}

func TestStreamRejectsMalformedChunksAndCalls(t *testing.T) {
	valid := syntheticChunk(`{"content":"preview"}`, `null`)
	for name, chunks := range map[string][]json.RawMessage{
		"unknown field":             {json.RawMessage(strings.Replace(string(valid), `"content":"preview"`, `"unknown":"private"`, 1))},
		"duplicate key":             {json.RawMessage(strings.Replace(string(valid), `"content":"preview"`, `"content":"a","content":"b"`, 1))},
		"changed response ID":       {valid, json.RawMessage(strings.Replace(string(valid), `"id":"chatcmpl-synthetic"`, `"id":"other"`, 1))},
		"wrong model":               {json.RawMessage(strings.Replace(string(valid), testModel, "other", 1))},
		"missing finish":            {valid},
		"payload after finish":      {syntheticChunk(`{}`, `"stop"`), valid},
		"unknown finish":            {syntheticChunk(`{}`, `"other"`)},
		"tool finish without calls": {syntheticChunk(`{}`, `"tool_calls"`)},
		"missing index":             {syntheticChunk(`{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`, `"tool_calls"`)},
		"sparse index":              {syntheticChunk(`{"tool_calls":[{"index":1,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`, `"tool_calls"`)},
		"invalid arguments":         {syntheticChunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"not json"}}]}`, `"tool_calls"`)},
		"changed tool id":           {syntheticChunk(`{"tool_calls":[{"index":0,"id":"call_1"}]}`, `null`), syntheticChunk(`{"tool_calls":[{"index":0,"id":"call_2"}]}`, `"tool_calls"`)},
	} {
		t.Run(name, func(t *testing.T) {
			b := &fakeBackend{stream: func(_ context.Context, _ json.RawMessage, emit func(json.RawMessage) error) error {
				for _, chunk := range chunks {
					if err := emit(chunk); err != nil {
						return err
					}
				}
				return nil
			}}
			w := httptest.NewRecorder()
			testServer(b).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", strings.TrimSuffix(goodRequest, "}")+`,"stream":true}`))
			body := w.Body.String()
			if (w.Code != 502 && !strings.Contains(body, "event: error")) || strings.Contains(body, "[DONE]") || strings.Contains(body, "call_1") || strings.Contains(body, "lookup") {
				t.Fatal("invalid stream must not release tools or success")
			}
		})
	}
	s := streamState{bytes: MaxResponseBytes}
	if _, err := s.accept(valid, testModel); err == nil {
		t.Fatal("stream accumulator must bound total decoded bytes")
	}
}

func TestStreamCancellation(t *testing.T) {
	b := &fakeBackend{stream: func(ctx context.Context, _ json.RawMessage, _ func(json.RawMessage) error) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	testServer(b).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", strings.TrimSuffix(goodRequest, "}")+`,"stream":true}`).WithContext(ctx))
	if w.Code != 504 || strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatal("stream cancellation must fail without success")
	}
}

func TestStreamNormalizesEngineTimestamps(t *testing.T) {
	w := httptest.NewRecorder()
	b := &fakeBackend{stream: func(_ context.Context, _ json.RawMessage, emit func(json.RawMessage) error) error {
		if err := emit(syntheticChunk(`{"content":"preview"}`, `null`)); err != nil {
			return err
		}
		// The engine may stamp each event with its current time. It is metadata,
		// not response identity; local OpenAI chunks keep the first timestamp.
		return emit(json.RawMessage(strings.Replace(string(syntheticChunk(`{}`, `"stop"`)), `"created":1`, `"created":2`, 1)))
	}}
	testServer(b).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", strings.TrimSuffix(goodRequest, "}")+`,"stream":true}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), `"created":2`) {
		t.Fatal("normalize varying engine timestamps across authenticated stream")
	}
}
