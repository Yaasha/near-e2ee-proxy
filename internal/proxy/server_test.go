package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "synthetic-proxy-key-with-32-characters"
const goodAnswer = `{"id":"chatcmpl-synthetic","object":"chat.completion","created":1,"model":"synthetic-model","choices":[{"index":0,"message":{"role":"assistant","content":"synthetic-answer"},"finish_reason":"stop","logprobs":null}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`

type fakeBackend struct {
	complete func(context.Context, json.RawMessage) (json.RawMessage, error)
	stream   func(context.Context, json.RawMessage, func(json.RawMessage) error) error
	check    func(context.Context, string) error
	calls    atomic.Int32
}

// The default fake converts a small synthetic answer into upstream chunks. Real
// streaming tests override stream to control exactly when verification completes.
func (f *fakeBackend) Stream(ctx context.Context, b json.RawMessage, emit func(json.RawMessage) error) error {
	if f.stream != nil {
		f.calls.Add(1)
		return f.stream(ctx, b, emit)
	}
	answer, err := f.Complete(ctx, b)
	if err != nil {
		return err
	}
	var c Completion
	if json.Unmarshal(answer, &c) != nil {
		return errors.New("invalid test fixture")
	}
	m := c.Choices[0].Message
	delta := streamDelta{Role: "assistant", ReasoningContent: m.ReasoningContent, Reasoning: m.Reasoning, Refusal: m.Refusal}
	if len(m.Content) > 0 && string(m.Content) != "null" {
		var text string
		json.Unmarshal(m.Content, &text)
		delta.Content = &text
	}
	for i, call := range m.ToolCalls {
		index := i
		d := toolDelta{Index: &index, ID: call.ID, Type: call.Type}
		d.Function.Name = call.Function.Name
		d.Function.Arguments = call.Function.Arguments
		delta.ToolCalls = append(delta.ToolCalls, d)
	}
	chunk := streamChunk{ID: c.ID, Object: "chat.completion.chunk", Created: c.Created, Model: c.Model, Choices: []streamChoice{{Delta: delta}}}
	raw, _ := json.Marshal(chunk)
	if err := emit(raw); err != nil {
		return err
	}
	chunk.Choices[0].Delta = streamDelta{}
	chunk.Choices[0].FinishReason = &c.Choices[0].FinishReason
	chunk.Usage = c.Usage
	raw, _ = json.Marshal(chunk)
	return emit(raw)
}

func (f *fakeBackend) Check(ctx context.Context, model string) error {
	if f.check != nil {
		return f.check(ctx, model)
	}
	return nil
}
func (f *fakeBackend) Complete(ctx context.Context, b json.RawMessage) (json.RawMessage, error) {
	f.calls.Add(1)
	if f.complete != nil {
		return f.complete(ctx, b)
	}
	return json.RawMessage(goodAnswer), nil
}
func testServer(backend Backend) *Server {
	return NewServer(Config{ProxyKey: testKey, Models: []string{testModel}}, backend)
}
func httpRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testKey)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestAuthenticateBeforeReadingOrCallingBackend(t *testing.T) {
	for name, headers := range map[string][]string{"missing": nil, "wrong": {"Bearer wrong"}, "duplicate": {"Bearer " + testKey, "Bearer " + testKey}, "empty": {"Bearer "}} {
		t.Run(name, func(t *testing.T) {
			b := &fakeBackend{}
			s := testServer(b)
			r := httpRequest("POST", "/v1/chat/completions", goodRequest)
			r.Header["Authorization"] = headers
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 401 || b.calls.Load() != 0 || strings.Contains(w.Body.String(), testKey) {
				t.Fatal("unauthenticated request must fail locally")
			}
		})
	}
}

func TestChatJSONAndNativeSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			b := &fakeBackend{complete: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
				var request map[string]any
				json.Unmarshal(raw, &request)
				if (request["stream"] == true) != stream || (request["stream_options"] != nil) != stream {
					t.Fatal("preserve streaming intent and options")
				}
				return json.RawMessage(goodAnswer), nil
			}}
			input := goodRequest
			if stream {
				input = strings.TrimSuffix(input, "}") + `,"stream":true,"stream_options":{"include_usage":true}}`
			}
			w := httptest.NewRecorder()
			testServer(b).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", input))
			if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || b.calls.Load() != 1 {
				t.Fatal("expected one successful no-store completion")
			}
			if stream {
				if w.Header().Get("Content-Type") != "text/event-stream" || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
					t.Fatal("expected a complete SSE lifecycle")
				}
				for _, line := range strings.Split(w.Body.String(), "\n") {
					if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" && !json.Valid([]byte(strings.TrimPrefix(line, "data: "))) {
						t.Fatal("invalid SSE JSON")
					}
				}
				if !strings.Contains(w.Body.String(), `"content":"synthetic-answer"`) || !strings.Contains(w.Body.String(), `"choices":[],"created"`) || !strings.Contains(w.Body.String(), `"total_tokens":4`) {
					t.Fatal("stream must include text, usage and completion")
				}
			} else if !json.Valid(w.Body.Bytes()) || !strings.Contains(w.Body.String(), "synthetic-answer") {
				t.Fatal("expected Chat JSON")
			}
		})
	}
}

func TestChatPreservesCompletionLimits(t *testing.T) {
	for name, extra := range map[string]string{
		"omitted":                  ``,
		"small explicit limit":     `,"max_completion_tokens":1`,
		"above former ceiling":     `,"max_completion_tokens":8193`,
		"reasoning and answer":     `,"thinking_budget":32768,"max_completion_tokens":65536`,
		"large legacy limit":       `,"max_tokens":131072`,
		"reasoning without output": `,"thinking_budget":32768`,
		"backend sampling":         `,"temperature":3,"top_p":1.1,"frequency_penalty":3,"presence_penalty":3`,
		"max effort":               `,"reasoning_effort":"max"`,
		"xhigh effort":             `,"reasoning_effort":"xhigh"`,
	} {
		for _, stream := range []bool{false, true} {
			mode := "json"
			if stream {
				mode = "sse"
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				input := strings.TrimSuffix(goodRequest, "}") + extra
				if stream {
					input += `,"stream":true`
				}
				input += "}"
				var original map[string]json.RawMessage
				if err := json.Unmarshal([]byte(input), &original); err != nil {
					t.Fatal(err)
				}
				backend := &fakeBackend{complete: func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
					var forwarded map[string]json.RawMessage
					if err := json.Unmarshal(raw, &forwarded); err != nil {
						t.Fatal(err)
					}
					for _, field := range []string{"max_tokens", "max_completion_tokens", "thinking_budget", "temperature", "top_p", "frequency_penalty", "presence_penalty", "reasoning_effort"} {
						if string(forwarded[field]) != string(original[field]) {
							t.Fatalf("proxy changed %s instead of preserving the client's limit or omission", field)
						}
					}
					return json.RawMessage(goodAnswer), nil
				}}
				w := httptest.NewRecorder()
				testServer(backend).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", input))
				if w.Code != 200 || backend.calls.Load() != 1 {
					t.Fatalf("completion limit must reach the backend once; got HTTP %d", w.Code)
				}
				if stream && !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
					t.Fatal("preserving completion limits must retain verified stream completion")
				}
			})
		}
	}
}

func TestChatRejectsInvalidCompletionLimits(t *testing.T) {
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		for name, value := range map[string]string{
			"zero":     `0`,
			"negative": `-1`,
			"fraction": `1.5`,
			"overflow": `999999999999999999999999999999999`,
			"string":   `"private-value"`,
			"boolean":  `true`,
			"object":   `{"private-name":"private-value"}`,
			"array":    `[1]`,
		} {
			t.Run(field+"/"+name, func(t *testing.T) {
				backend := &fakeBackend{}
				input := strings.TrimSuffix(goodRequest, "}") + `,"` + field + `":` + value + `}`
				w := httptest.NewRecorder()
				testServer(backend).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", input))
				if w.Code != 400 || backend.calls.Load() != 0 {
					t.Fatal("invalid completion limits must fail locally before inference")
				}
				if strings.Contains(w.Body.String(), "private-value") || strings.Contains(w.Body.String(), "private-name") {
					t.Fatal("invalid completion limits must not be reflected in errors")
				}
			})
		}
	}
}

func TestChatUsesCallerDeadline(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, parentDeadline := range []bool{false, true} {
			ctx := context.Background()
			if parentDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 10*time.Minute)
				defer cancel()
			}
			backend := &fakeBackend{complete: func(got context.Context, _ json.RawMessage) (json.RawMessage, error) {
				expected, wantDeadline := ctx.Deadline()
				actual, hasDeadline := got.Deadline()
				if hasDeadline != wantDeadline || (hasDeadline && !actual.Equal(expected)) {
					t.Fatal("the proxy must not impose a shorter inference deadline")
				}
				return json.RawMessage(goodAnswer), nil
			}}
			input := goodRequest
			if stream {
				input = strings.TrimSuffix(input, "}") + `,"stream":true}`
			}
			w := httptest.NewRecorder()
			testServer(backend).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", input).WithContext(ctx))
			if w.Code != 200 || backend.calls.Load() != 1 {
				t.Fatal("caller-controlled inference must complete once")
			}
		}
	}
}

func TestChatLargeContextAndAnswer(t *testing.T) {
	for _, stream := range []bool{false, true} {
		input := strings.Replace(goodRequest, "synthetic-prompt", strings.Repeat("p", (1<<20)+1), 1)
		answer := strings.Replace(goodAnswer, "synthetic-answer", strings.Repeat("a", (8<<20)+1), 1)
		backend := &fakeBackend{complete: func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
			if len(raw) <= 1<<20 {
				t.Fatal("the full context must reach the backend")
			}
			return json.RawMessage(answer), nil
		}}
		if stream {
			input = strings.TrimSuffix(input, "}") + `,"stream":true}`
		}
		w := httptest.NewRecorder()
		testServer(backend).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", input))
		if w.Code != 200 || backend.calls.Load() != 1 || w.Body.Len() <= 8<<20 || strings.Contains(w.Body.String(), `"error"`) {
			t.Fatal("proxy-specific context and response caps must not interrupt a valid completion")
		}
		if stream && !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
			t.Fatal("the complete large stream must reach verified completion")
		}
	}
}

func TestConfiguredBodyLimits(t *testing.T) {
	const limit = 512
	backend := &fakeBackend{}
	s := NewServer(Config{ProxyKey: testKey, MaxBodyBytes: limit}, backend)
	w := httptest.NewRecorder()
	input := strings.Replace(goodRequest, "synthetic-prompt", strings.Repeat("p", limit), 1)
	s.ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", input))
	if w.Code != 413 || backend.calls.Load() != 0 {
		t.Fatal("an operator's body limit must reject oversized input before inference")
	}
	for _, stream := range []bool{false, true} {
		backend := &fakeBackend{complete: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(strings.Replace(goodAnswer, "synthetic-answer", strings.Repeat("a", limit), 1)), nil
		}}
		s := NewServer(Config{ProxyKey: testKey, MaxBodyBytes: limit}, backend)
		input := goodRequest
		if stream {
			input = strings.TrimSuffix(input, "}") + `,"stream":true}`
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", input))
		if w.Code != 502 || backend.calls.Load() != 1 || strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), strings.Repeat("a", limit)) {
			t.Fatal("oversized responses must fail explicitly without successful completion")
		}
	}
}

func TestConfiguredInferenceTimeout(t *testing.T) {
	for _, stream := range []bool{false, true} {
		backend := &fakeBackend{complete: func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("an explicitly configured deadline must reach inference")
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		input := goodRequest
		if stream {
			input = strings.TrimSuffix(input, "}") + `,"stream":true}`
		}
		w := httptest.NewRecorder()
		s := NewServer(Config{ProxyKey: testKey, RequestTimeout: time.Millisecond}, backend)
		s.ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", input))
		if w.Code != 504 || !strings.Contains(w.Body.String(), `"code":"request_timeout"`) || backend.calls.Load() != 1 || strings.Contains(w.Body.String(), "[DONE]") {
			t.Fatal("operator-configured timeouts must fail explicitly without replay or successful completion")
		}
	}
}

func TestVerificationFailureReleasesNoAnswerOrSSE(t *testing.T) {
	b := &fakeBackend{complete: func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(goodAnswer), errors.New("synthetic-secret-upstream-error")
	}}
	w := httptest.NewRecorder()
	input := strings.TrimSuffix(goodRequest, "}") + `,"stream":true}`
	testServer(b).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", input))
	if w.Code != 502 || w.Flushed || strings.Contains(w.Body.String(), "synthetic-") || strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatal("release nothing until the backend verifies the answer")
	}
}

func TestUnsupportedRequestsAndLimits(t *testing.T) {
	for name, test := range map[string]struct {
		method, path, body, header, value string
		status                            int
	}{
		"responses":          {"POST", "/v1/responses", goodRequest, "", "", 404},
		"legacy completions": {"POST", "/v1/completions", goodRequest, "", "", 404},
		"wrong method":       {"GET", "/v1/chat/completions", "", "", "", 404},
		"query":              {"POST", "/v1/chat/completions?model=private", goodRequest, "", "", 400},
		"unknown field":      {"POST", "/v1/chat/completions", strings.TrimSuffix(goodRequest, "}") + `,"user":"private"}`, "", "", 400},
		"mime":               {"POST", "/v1/chat/completions", goodRequest, "Content-Type", "text/plain", 415},
		"compression":        {"POST", "/v1/chat/completions", goodRequest, "Content-Encoding", "gzip", 415},
		"spoofed encryption": {"POST", "/v1/chat/completions", goodRequest, "X-Model-Pub-Key", "attacker-key", 400},
	} {
		t.Run(name, func(t *testing.T) {
			b := &fakeBackend{}
			r := httpRequest(test.method, test.path, test.body)
			if test.header != "" {
				r.Header.Set(test.header, test.value)
			}
			w := httptest.NewRecorder()
			testServer(b).ServeHTTP(w, r)
			if w.Code != test.status || b.calls.Load() != 0 {
				t.Fatal("request must fail before contacting inference")
			}
		})
	}
}

func TestModelAdmissionAndReadiness(t *testing.T) {
	for _, path := range []string{"/v1/models", "/readyz"} {
		w := httptest.NewRecorder()
		testServer(&fakeBackend{}).ServeHTTP(w, httpRequest("GET", path, ""))
		if w.Code != 200 {
			t.Fatal("expected admitted model/readiness")
		}
		w = httptest.NewRecorder()
		b := &fakeBackend{check: func(context.Context, string) error { return errors.New("private-error") }}
		testServer(b).ServeHTTP(w, httpRequest("GET", path, ""))
		if w.Code != 503 || strings.Contains(w.Body.String(), "private-error") {
			t.Fatal("admission failure must be safe and unavailable")
		}
	}
}

func TestRequestModelIsIndependentOfDiscoveryList(t *testing.T) {
	const requested = "provider/Another-Model"
	for _, stream := range []bool{false, true} {
		b := &fakeBackend{complete: func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
			var r Request
			if json.Unmarshal(raw, &r) != nil || r.Model != requested {
				t.Fatal("the requested model must reach the confidential backend unchanged")
			}
			return json.RawMessage(strings.Replace(goodAnswer, testModel, requested, 1)), nil
		}}
		input := strings.Replace(goodRequest, testModel, requested, 1)
		if stream {
			input = strings.TrimSuffix(input, "}") + `,"stream":true}`
		}
		w := httptest.NewRecorder()
		s := testServer(b) // Advertises only testModel, not the requested model.
		s.ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", input))
		if w.Code != 200 || b.calls.Load() != 1 || !strings.Contains(w.Body.String(), requested) {
			t.Fatal("JSON and SSE must support a model outside the discovery list")
		}
		w = httptest.NewRecorder()
		s.ServeHTTP(w, httpRequest("GET", "/v1/models", ""))
		if w.Code != 200 || strings.Contains(w.Body.String(), requested) || !strings.Contains(w.Body.String(), testModel) {
			t.Fatal("discovery must still advertise only configured entries")
		}
	}
}

func TestOverloadAndCancellation(t *testing.T) {
	entered, release, finished := make(chan struct{}, 4), make(chan struct{}), make(chan int, 4)
	s := testServer(&fakeBackend{complete: func(context.Context, json.RawMessage) (json.RawMessage, error) {
		entered <- struct{}{}
		<-release
		return json.RawMessage(goodAnswer), nil
	}})
	for i := 0; i < 4; i++ {
		go func() {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", goodRequest))
			finished <- w.Code
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("inference did not start")
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", goodRequest))
	close(release)
	if w.Code != 429 {
		t.Fatal("reject overload without a waiting queue")
	}
	for i := 0; i < 4; i++ {
		if <-finished != 200 {
			t.Fatal("admitted requests must complete")
		}
	}
	b := &fakeBackend{complete: func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w = httptest.NewRecorder()
	testServer(b).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", goodRequest).WithContext(ctx))
	if w.Code != 504 {
		t.Fatal("propagate cancellation")
	}
}

func TestEmptyServerKeyCannotAuthenticate(t *testing.T) {
	b := &fakeBackend{}
	s := NewServer(Config{Models: []string{testModel}}, b)
	r := httpRequest("POST", "/v1/chat/completions", goodRequest)
	r.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 || b.calls.Load() != 0 {
		t.Fatal("an empty configured key must never authenticate")
	}
}

func TestEngineMetadataIsExplicitlyNormalized(t *testing.T) {
	answer := strings.Replace(goodAnswer, `"logprobs":null`, `"logprobs":null,"matched_stop":151645`, 1)
	answer = strings.Replace(answer, `"total_tokens":4`, `"total_tokens":4,"reasoning_tokens":2`, 1)
	completion, err := parseCompletion([]byte(answer), testModel)
	if err != nil || completion.Choices[0].MatchedStop != nil || completion.Usage.ReasoningTokens != nil || completion.Usage.CompletionDetails.Reasoning != 2 {
		t.Fatal("normalize numeric engine metadata to the local contract")
	}
	for _, invalid := range []string{strings.Replace(answer, `"matched_stop":151645`, `"matched_stop":"unhandled-private-text"`, 1), strings.Replace(answer, `"reasoning_tokens":2`, `"reasoning_tokens":-1`, 1)} {
		if _, err := parseCompletion([]byte(invalid), testModel); err == nil {
			t.Fatal("reject unhandled text and invalid usage")
		}
	}
}

func TestRejectUnsupportedUpstreamShape(t *testing.T) {
	for name, answer := range map[string]string{
		"unknown field":     strings.TrimSuffix(goodAnswer, "}") + `,"private":"unknown"}`,
		"wrong model":       strings.Replace(goodAnswer, "synthetic-model", "another", 1),
		"unknown message":   strings.Replace(goodAnswer, `"content":"synthetic-answer"`, `"content":"synthetic-answer","private":"unknown"`, 1),
		"non-function call": strings.Replace(goodAnswer, `"content":"synthetic-answer"`, `"tool_calls":[{"id":"call_1","type":"custom","function":{"name":"safe","arguments":"{}"}}]`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			b := &fakeBackend{complete: func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(answer), nil }}
			w := httptest.NewRecorder()
			testServer(b).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", goodRequest))
			if w.Code != 502 || strings.Contains(w.Body.String(), "synthetic-answer") {
				t.Fatal("unsupported output must not be relayed")
			}
		})
	}
}

func TestToolCallStream(t *testing.T) {
	answer := strings.Replace(goodAnswer, `"content":"synthetic-answer"`, `"content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"value\":1}"}}]`, 1)
	answer = strings.Replace(answer, `"finish_reason":"stop"`, `"finish_reason":"tool_calls"`, 1)
	b := &fakeBackend{complete: func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(answer), nil }}
	w := httptest.NewRecorder()
	testServer(b).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", strings.TrimSuffix(goodRequest, "}")+`,"stream":true}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"index":0,"id":"call_1","type":"function"`) || !strings.Contains(w.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatal("emit indexed native function-call deltas")
	}
}

// This uses a real HTTP listener, rather than only calling the handler directly.
func TestHTTPServerSmoke(t *testing.T) {
	server := httptest.NewServer(testServer(&fakeBackend{}))
	defer server.Close()
	client := &http.Client{Timeout: time.Second}
	for _, path := range []string{"/healthz", "/v1/models"} {
		request, _ := http.NewRequest("GET", server.URL+path, nil)
		if path != "/healthz" {
			request.Header.Set("Authorization", "Bearer "+testKey)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("HTTP request failed")
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("HTTP service is not healthy")
		}
	}
}
