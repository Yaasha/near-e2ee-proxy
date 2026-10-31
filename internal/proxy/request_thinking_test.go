package proxy

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	nearai "github.com/nearai/inference-sdk/go"
)

func TestThinkingAliasReachesBackendInCanonicalForm(t *testing.T) {
	for name, test := range map[string]struct {
		extra   string
		enabled bool
	}{
		"on":             {`,"enable_thinking":true`, true},
		"off":            {`,"enable_thinking":false`, false},
		"same on":        {`,"enable_thinking":true,"chat_template_kwargs":{"enable_thinking":true}`, true},
		"same off":       {`,"enable_thinking":false,"chat_template_kwargs":{"enable_thinking":false}`, false},
		"empty template": {`,"enable_thinking":true,"chat_template_kwargs":{}`, true},
	} {
		for _, stream := range []bool{false, true} {
			mode := "json"
			if stream {
				mode = "sse"
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				backend := &fakeBackend{complete: func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
					var request struct {
						Template struct {
							Enable *bool `json:"enable_thinking"`
						} `json:"chat_template_kwargs"`
					}
					var fields map[string]json.RawMessage
					if json.Unmarshal(raw, &request) != nil || json.Unmarshal(raw, &fields) != nil || request.Template.Enable == nil || *request.Template.Enable != test.enabled {
						t.Fatal("preserve the requested thinking mode in NEAR's canonical field")
					}
					if fields["enable_thinking"] != nil || fields["thinking_budget"] != nil || !strings.Contains(string(raw), "synthetic-prompt") {
						t.Fatal("normalize only the alias and preserve message content")
					}
					return json.RawMessage(goodAnswer), nil
				}}
				input := strings.TrimSuffix(goodRequest, "}") + test.extra
				if stream {
					input += `,"stream":true`
				}
				w := httptest.NewRecorder()
				testServer(backend).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", input+"}"))
				if w.Code != 200 || backend.calls.Load() != 1 {
					t.Fatalf("thinking alias must reach the backend once; got HTTP %d", w.Code)
				}
				if stream && !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
					t.Fatal("canonical thinking requests must retain successful stream completion")
				}
			})
		}
	}
}

func TestThinkingAliasRejectsAmbiguityAndUnsupportedBudgets(t *testing.T) {
	for name, test := range map[string]struct {
		extra, param, message string
	}{
		"conflict":              {`,"enable_thinking":true,"chat_template_kwargs":{"enable_thinking":false}`, "enable_thinking", "enable_thinking conflicts with chat_template_kwargs.enable_thinking."},
		"reverse conflict":      {`,"enable_thinking":false,"chat_template_kwargs":{"enable_thinking":true}`, "enable_thinking", "enable_thinking conflicts with chat_template_kwargs.enable_thinking."},
		"string":                {`,"enable_thinking":"private-value"`, "enable_thinking", "enable_thinking must be a boolean."},
		"number":                {`,"enable_thinking":1`, "enable_thinking", "enable_thinking must be a boolean."},
		"null":                  {`,"enable_thinking":null`, "enable_thinking", "enable_thinking must be a boolean."},
		"object":                {`,"enable_thinking":{"private-name":"private-value"}`, "enable_thinking", "enable_thinking must be a boolean."},
		"null template":         {`,"enable_thinking":true,"chat_template_kwargs":null`, "chat_template_kwargs", "chat_template_kwargs must be an object when enable_thinking is specified."},
		"scalar template":       {`,"enable_thinking":true,"chat_template_kwargs":"private-value"`, "chat_template_kwargs", "chat_template_kwargs must be an object when enable_thinking is specified."},
		"invalid nested toggle": {`,"enable_thinking":true,"chat_template_kwargs":{"enable_thinking":null}`, "chat_template_kwargs.enable_thinking", "chat_template_kwargs.enable_thinking must be a boolean."},
		"router medium budget":  {`,"enable_thinking":true,"thinking_budget":8192`, "thinking_budget", "thinking_budget is unsupported: an exact reasoning-token cap cannot be guaranteed. Omit it and use reasoning_effort or chat_template_kwargs.enable_thinking."},
		"budget while off":      {`,"enable_thinking":false,"thinking_budget":1024`, "thinking_budget", "thinking_budget is unsupported: an exact reasoning-token cap cannot be guaranteed. Omit it and use reasoning_effort or chat_template_kwargs.enable_thinking."},
		"private budget":        {`,"thinking_budget":"private-value"`, "thinking_budget", "thinking_budget is unsupported: an exact reasoning-token cap cannot be guaranteed. Omit it and use reasoning_effort or chat_template_kwargs.enable_thinking."},
		"null budget":           {`,"thinking_budget":null`, "thinking_budget", "thinking_budget is unsupported: an exact reasoning-token cap cannot be guaranteed. Omit it and use reasoning_effort or chat_template_kwargs.enable_thinking."},
	} {
		t.Run(name, func(t *testing.T) {
			backend := &fakeBackend{}
			w := httptest.NewRecorder()
			input := strings.TrimSuffix(goodRequest, "}") + test.extra + "}"
			testServer(backend).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", input))
			var response struct {
				Error struct {
					Code, Type, Param, Message string
				} `json:"error"`
			}
			if json.Unmarshal(w.Body.Bytes(), &response) != nil || w.Code != 400 || backend.calls.Load() != 0 {
				t.Fatal("invalid controls must fail locally with a JSON error")
			}
			if response.Error.Code != "unsupported_request" || response.Error.Type != "invalid_request_error" || response.Error.Param != test.param || response.Error.Message != test.message {
				t.Fatal("thinking rejection must identify the fixed parameter and corrective action")
			}
			for _, private := range []string{"private-name", "private-value", "synthetic-prompt", testKey} {
				if strings.Contains(w.Body.String(), private) {
					t.Fatal("thinking diagnostics must not reflect request content or credentials")
				}
			}
		})
	}
}

func TestThinkingNormalizationKeepsStrictJSONValidation(t *testing.T) {
	for name, extra := range map[string]string{
		"root extension":    `,"enable_thinking":true,"private-name":"private-value"`,
		"nested extension":  `,"enable_thinking":true,"chat_template_kwargs":{"private-name":"private-value"}`,
		"duplicate alias":   `,"enable_thinking":true,"enable_thinking":false`,
		"escaped duplicate": `,"enable_thinking":true,"\u0065nable_thinking":false`,
		"duplicate nested":  `,"enable_thinking":true,"chat_template_kwargs":{"enable_thinking":true,"enable_thinking":false}`,
		"wrong casing":      `,"Enable_Thinking":true`,
		"duplicate budget":  `,"thinking_budget":8192,"thinking_budget":1024`,
	} {
		t.Run(name, func(t *testing.T) {
			backend := &fakeBackend{}
			w := httptest.NewRecorder()
			input := strings.TrimSuffix(goodRequest, "}") + extra + "}"
			testServer(backend).ServeHTTP(w, httpRequest("POST", "/v1/chat/completions", input))
			if w.Code != 400 || backend.calls.Load() != 0 {
				t.Fatal("normalizing thinking must not admit duplicate or unknown fields")
			}
			if strings.Contains(w.Body.String(), "private-name") || strings.Contains(w.Body.String(), "private-value") {
				t.Fatal("invalid fields must not appear in errors")
			}
		})
	}
}

func TestThinkingAliasPreservesSDKEncryption(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []string{"true", "false"} {
		t.Run(enabled, func(t *testing.T) {
			input := strings.TrimSuffix(goodRequest, "}") + `,"enable_thinking":` + enabled + "}"
			parsed, err := parseRequest([]byte(input))
			if err != nil {
				t.Fatal("supported thinking alias must parse")
			}
			canonical, err := json.Marshal(parsed)
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequest("POST", nearai.DefaultBaseURL+"/chat/completions", bytes.NewReader(canonical))
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := nearai.PrepareE2EEChatRequest(request, nearai.E2EEModelKey{SigningAlgo: nearai.Ed25519, PublicKey: hex.EncodeToString(public)})
			if err != nil {
				t.Fatal("the real SDK must encrypt the normalized request")
			}
			defer prepared.Request.Body.Close()
			wire, err := io.ReadAll(prepared.Request.Body)
			var fields map[string]json.RawMessage
			if err != nil || json.Unmarshal(wire, &fields) != nil || bytes.Contains(wire, []byte("synthetic-prompt")) {
				t.Fatal("normalizing a thinking alias must retain prompt encryption")
			}
			var template map[string]json.RawMessage
			if json.Unmarshal(fields["chat_template_kwargs"], &template) != nil || string(template["enable_thinking"]) != enabled || fields["enable_thinking"] != nil {
				t.Fatal("only the canonical boolean control may remain visible to upstream routing")
			}
			if prepared.Request.Header.Get("X-Encryption-Version") != "2" || prepared.Request.Header.Get("X-Encrypt-All-Fields") != "true" {
				t.Fatal("thinking aliases must preserve expanded model-key E2EE")
			}
		})
	}
}
