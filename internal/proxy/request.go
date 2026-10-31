package proxy

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

// Request is the entire supported input contract. Adding a field requires a
// confidentiality review and a test; arbitrary OpenAI extensions are not relayed.
// Pointer fields distinguish an omitted setting from an explicit zero/false.
type Request struct {
	Model               string          `json:"model"`
	Messages            []Message       `json:"messages"`
	Tools               []Tool          `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	FrequencyPenalty    *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64        `json:"presence_penalty,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Seed                *int64          `json:"seed,omitempty"`
	N                   *int            `json:"n,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	ChatTemplate        *Template       `json:"chat_template_kwargs,omitempty"`
	ResponseFormat      *Format         `json:"response_format,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
}

type Message struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content,omitempty"`
	Name             string          `json:"name,omitempty"`
	ReasoningContent *string         `json:"reasoning_content,omitempty"`
	Reasoning        *string         `json:"reasoning,omitempty"`
	Refusal          *string         `json:"refusal,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
}

type Function struct {
	Name        string          `json:"name"`
	Description *string         `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}
type CallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function CallFunction `json:"function"`
}
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}
type Template struct {
	EnableThinking *bool `json:"enable_thinking,omitempty"`
}
type Format struct {
	Type string `json:"type"`
}
type textPart struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
}
type namedChoice struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// Each protocol object admits exact field names. JSON-schema content inside
// Function.Parameters is intentionally opaque: the SDK encrypts the whole object.
// The local "plain" types retain fields but omit methods, so decoding them does
// not recursively call the same UnmarshalJSON method forever.
func (r *Request) UnmarshalJSON(b []byte) error {
	type plain Request
	normalized, err := normalizeThinking(b)
	if err != nil {
		return err
	}
	return fields(normalized, (*plain)(r), "model messages tools tool_choice parallel_tool_calls temperature top_p frequency_penalty presence_penalty max_tokens max_completion_tokens seed n reasoning_effort chat_template_kwargs response_format stream stream_options")
}
func (m *Message) UnmarshalJSON(b []byte) error {
	type plain Message
	return fields(b, (*plain)(m), "role content name reasoning_content reasoning refusal tool_calls tool_call_id")
}
func (f *Function) UnmarshalJSON(b []byte) error {
	type plain Function
	return fields(b, (*plain)(f), "name description parameters strict")
}
func (t *Tool) UnmarshalJSON(b []byte) error {
	type plain Tool
	return fields(b, (*plain)(t), "type function")
}
func (t *ToolCall) UnmarshalJSON(b []byte) error {
	type plain ToolCall
	return fields(b, (*plain)(t), "id type function")
}
func (f *CallFunction) UnmarshalJSON(b []byte) error {
	type plain CallFunction
	return fields(b, (*plain)(f), "name arguments")
}
func (s *StreamOptions) UnmarshalJSON(b []byte) error {
	type plain StreamOptions
	return fields(b, (*plain)(s), "include_usage")
}
func (t *Template) UnmarshalJSON(b []byte) error {
	type plain Template
	return fields(b, (*plain)(t), "enable_thinking")
}
func (f *Format) UnmarshalJSON(b []byte) error {
	type plain Format
	return fields(b, (*plain)(f), "type")
}
func (p *textPart) UnmarshalJSON(b []byte) error {
	type plain textPart
	return fields(b, (*plain)(p), "type text")
}

var functionName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var opaqueID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
var modelID = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_./-]{0,199}$`)

// Syntax validation bounds model metadata; actual availability and confidential
// hardware admission are checked by the SDK for the model selected in each call.
func validModelID(model string) bool {
	return modelID.MatchString(model) && !strings.Contains(model, "..")
}

func parseRequest(raw []byte) (Request, error) {
	var r Request
	if !validJSON(raw) {
		return r, errInvalid
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	if !validModelID(r.Model) || len(r.Messages) == 0 || len(r.Messages) > 1024 {
		return r, errInvalid
	}
	if !within(r.Temperature, 0, 2) || !within(r.TopP, 0, 1) || !within(r.FrequencyPenalty, -2, 2) || !within(r.PresencePenalty, -2, 2) || (r.N != nil && *r.N != 1) {
		return r, errInvalid
	}
	if r.MaxTokens != nil && r.MaxCompletionTokens != nil {
		return r, errInvalid
	}
	for _, limit := range []*int{r.MaxTokens, r.MaxCompletionTokens} {
		if limit != nil && (*limit < 1 || *limit > 8192) {
			return r, errInvalid
		}
	}
	if r.MaxTokens == nil && r.MaxCompletionTokens == nil {
		limit := 1024
		r.MaxCompletionTokens = &limit
	}
	if r.ReasoningEffort != "" && !slices.Contains([]string{"none", "minimal", "low", "medium", "high"}, r.ReasoningEffort) {
		return r, errInvalid
	}
	if r.ResponseFormat != nil && !slices.Contains([]string{"text", "json_object"}, r.ResponseFormat.Type) {
		return r, errInvalid
	}
	if r.StreamOptions != nil && !r.Stream {
		return r, errInvalid
	}
	names := map[string]bool{}
	if len(r.Tools) > 128 {
		return r, errInvalid
	}
	for _, tool := range r.Tools {
		f := tool.Function
		if tool.Type != "function" || !functionName.MatchString(f.Name) || names[f.Name] {
			return r, errInvalid
		}
		names[f.Name] = true
		if len(f.Parameters) > 0 && !isObject(f.Parameters) {
			return r, errInvalid
		}
	}
	if len(r.ToolChoice) > 0 && !bytes.Equal(r.ToolChoice, []byte("null")) {
		var choice string
		if json.Unmarshal(r.ToolChoice, &choice) == nil {
			if !slices.Contains([]string{"none", "auto", "required"}, choice) || (choice != "none" && len(r.Tools) == 0) {
				return r, errInvalid
			}
		} else {
			var choice namedChoice
			if fields(r.ToolChoice, &choice, "type function") != nil || choice.Type != "function" || !names[choice.Function.Name] {
				return r, errInvalid
			}
			var object map[string]json.RawMessage
			json.Unmarshal(r.ToolChoice, &object)
			var function struct {
				Name string `json:"name"`
			}
			if fields(object["function"], &function, "name") != nil {
				return r, errInvalid
			}
		}
	}
	// Enforce call/result association without storing conversation state. Every
	// request carries its own history; tool execution stays on the agent's host.
	pending, used := map[string]bool{}, map[string]bool{}
	for _, message := range r.Messages {
		if !slices.Contains([]string{"system", "developer", "user", "assistant", "tool"}, message.Role) {
			return r, errInvalid
		}
		if !validContent(message.Content, message.Role == "assistant") {
			return r, errInvalid
		}
		if message.Name != "" && !functionName.MatchString(message.Name) {
			return r, errInvalid
		}
		if message.Role != "assistant" && (len(message.ToolCalls) > 0 || message.ReasoningContent != nil || message.Reasoning != nil || message.Refusal != nil) {
			return r, errInvalid
		}
		if message.Role == "tool" {
			if !pending[message.ToolCallID] {
				return r, errInvalid
			}
			delete(pending, message.ToolCallID)
		} else {
			if message.ToolCallID != "" || len(pending) > 0 {
				return r, errInvalid
			}
		}
		for _, call := range message.ToolCalls {
			if !validCall(call) || used[call.ID] {
				return r, errInvalid
			}
			pending[call.ID], used[call.ID] = true, true
		}
	}
	if len(pending) > 0 {
		return r, errInvalid
	}
	return r, nil
}

func within(value *float64, low, high float64) bool {
	return value == nil || (*value >= low && *value <= high)
}
func isObject(raw []byte) bool {
	var v map[string]json.RawMessage
	return json.Unmarshal(raw, &v) == nil && v != nil
}
func validCall(call ToolCall) bool {
	return opaqueID.MatchString(call.ID) && call.Type == "function" && functionName.MatchString(call.Function.Name) && validJSON([]byte(call.Function.Arguments)) && isObject([]byte(call.Function.Arguments))
}

func validContent(raw json.RawMessage, nullable bool) bool {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nullable
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return true
	}
	var parts []textPart
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if part.Type != "text" || part.Text == nil {
			return false
		}
	}
	return true
}
