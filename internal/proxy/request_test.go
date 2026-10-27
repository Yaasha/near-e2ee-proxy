package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

const testModel = "synthetic-model"
const goodRequest = `{"model":"synthetic-model","messages":[{"role":"user","content":"synthetic-prompt"}]}`

func TestRequestRejectsUnknownAndUnencryptedFields(t *testing.T) {
	cases := map[string]string{
		"null text part":                `{"model":"synthetic-model","messages":[{"role":"user","content":[{"type":"text","text":null}]}]}`,
		"missing text part":             `{"model":"synthetic-model","messages":[{"role":"user","content":[{"type":"text"}]}]}`,
		"unknown root":                  strings.TrimSuffix(goodRequest, "}") + `,"metadata":{"secret":"private"}}`,
		"case alias":                    strings.Replace(goodRequest, `"model"`, `"Model"`, 1),
		"duplicate key":                 strings.TrimSuffix(goodRequest, "}") + `,"model":"another"}`,
		"escaped duplicate":             strings.TrimSuffix(goodRequest, "}") + `,"\u006dodel":"another"}`,
		"trailing value":                goodRequest + ` {}`,
		"unknown message":               `{"model":"synthetic-model","messages":[{"role":"user","content":"x","metadata":{"private":"x"}}]}`,
		"image content":                 `{"model":"synthetic-model","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com"}}]}]}`,
		"unknown text part":             `{"model":"synthetic-model","messages":[{"role":"user","content":[{"type":"text","text":"x","private":"x"}]}]}`,
		"unknown function":              `{"model":"synthetic-model","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"safe","private":"x"}}]}`,
		"schema response":               strings.TrimSuffix(goodRequest, "}") + `,"response_format":{"type":"json_schema","json_schema":{"private":"x"}}}`,
		"user identifier":               strings.TrimSuffix(goodRequest, "}") + `,"user":"private"}`,
		"stop string":                   strings.TrimSuffix(goodRequest, "}") + `,"stop":"private"}`,
		"sampling string":               strings.TrimSuffix(goodRequest, "}") + `,"temperature":"private"}`,
		"many outputs":                  strings.TrimSuffix(goodRequest, "}") + `,"n":2}`,
		"token conflict":                strings.TrimSuffix(goodRequest, "}") + `,"max_tokens":20,"max_completion_tokens":20}`,
		"oversize output":               strings.TrimSuffix(goodRequest, "}") + `,"max_tokens":8193}`,
		"negative tokens":               strings.TrimSuffix(goodRequest, "}") + `,"max_tokens":-1}`,
		"stream options without stream": strings.TrimSuffix(goodRequest, "}") + `,"stream_options":{"include_usage":true}}`,
		"deep JSON":                     `{"model":"synthetic-model","messages":[],"tools":` + strings.Repeat("[", 70) + `0` + strings.Repeat("]", 70) + `}`,
		"invalid UTF8":                  strings.Replace(goodRequest, "synthetic-prompt", string([]byte{0xff}), 1),
		"missing model":                 strings.Replace(goodRequest, `"model":"synthetic-model",`, "", 1),
		"unknown reasoning":             strings.TrimSuffix(goodRequest, "}") + `,"reasoning_effort":"private"}`,
		"template extension":            strings.TrimSuffix(goodRequest, "}") + `,"chat_template_kwargs":{"private":"x"}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRequest([]byte(raw)); err == nil {
				t.Fatal("unsupported input must fail before inference")
			}
		})
	}
}

func TestToolHistoryAndSchemaArePreserved(t *testing.T) {
	input := `{"model":"synthetic-model","messages":[
	{"role":"user","content":[{"type":"text","text":"synthetic-prompt"}]},
	{"role":"assistant","content":null,"reasoning_content":"private-reasoning","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"value\":1}"}}]},
	{"role":"tool","tool_call_id":"call_1","content":"private-result"}],
	"tools":[{"type":"function","function":{"name":"lookup","description":"private-description","parameters":{"type":"object","properties":{"private-field":{"type":"integer"}},"additionalProperties":false},"strict":true}}],
	"tool_choice":{"type":"function","function":{"name":"lookup"}},"parallel_tool_calls":true,"stream":true,"stream_options":{"include_usage":true}}`
	r, err := parseRequest([]byte(input))
	if err != nil {
		t.Fatal("supported tool history must parse")
	}
	encoded, err := json.Marshal(r)
	if err != nil || !strings.Contains(string(encoded), "private-field") || !strings.Contains(string(encoded), "private-result") || r.MaxCompletionTokens == nil || *r.MaxCompletionTokens != 1024 {
		t.Fatal("preserve schema/results and set a bounded default output")
	}
	for name, invalid := range map[string]string{
		"orphan":           strings.Replace(input, `"tool_call_id":"call_1"`, `"tool_call_id":"call_other"`, 1),
		"missing result":   strings.Replace(input, `{"role":"tool","tool_call_id":"call_1","content":"private-result"}`, `{"role":"user","content":"another"}`, 1),
		"unknown choice":   strings.Replace(input, `"tool_choice":{"type":"function","function":{"name":"lookup"}}`, `"tool_choice":{"type":"function","function":{"name":"other"}}`, 1),
		"choice extension": strings.Replace(input, `"tool_choice":{"type":"function","function":{"name":"lookup"}}`, `"tool_choice":{"type":"function","function":{"name":"lookup","private":"x"}}}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRequest([]byte(invalid)); err == nil {
				t.Fatal("invalid tool correlation or choice must fail")
			}
		})
	}
}

func FuzzRequestValidation(f *testing.F) {
	f.Add([]byte(goodRequest))
	f.Add([]byte(`{"model":"synthetic-model","messages":null}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > MaxRequestBytes {
			return
		}
		r, err := parseRequest(raw)
		if err == nil {
			encoded, err := json.Marshal(r)
			if err != nil || !json.Valid(encoded) || !validModelID(r.Model) {
				t.Fatal("accepted request must have a valid canonical encoding")
			}
		}
	})
}

func TestRequestSelectsModelWithoutAllowlist(t *testing.T) {
	for _, model := range []string{"another-model", "provider/Model-27B", "provider/Model_4.8"} {
		request, err := parseRequest([]byte(strings.Replace(goodRequest, testModel, model, 1)))
		if err != nil || request.Model != model {
			t.Fatal("preserve the requested model without a configured allowlist")
		}
	}
	for _, model := range []string{"", "../model", "provider/../model", "https://example.com/model", "model?route=other", "model#other", " padded", "model\n", strings.Repeat("m", 201)} {
		input, _ := json.Marshal(map[string]any{"model": model, "messages": []any{map[string]string{"role": "user", "content": "synthetic"}}})
		if _, err := parseRequest(input); err == nil {
			t.Fatal("malformed model metadata must fail locally")
		}
	}
}
