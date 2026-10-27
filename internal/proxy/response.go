package proxy

import (
	"bytes"
	"encoding/json"
	"slices"
)

type Completion struct {
	ID          string   `json:"id"`
	Object      string   `json:"object"`
	Created     int64    `json:"created"`
	Model       string   `json:"model"`
	Choices     []Choice `json:"choices"`
	Usage       *Usage   `json:"usage,omitempty"`
	Fingerprint *string  `json:"system_fingerprint,omitempty"`
}
type Choice struct {
	Index        int             `json:"index"`
	Message      Message         `json:"message"`
	FinishReason string          `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
	MatchedStop  json.RawMessage `json:"matched_stop,omitempty"`
}
type Usage struct {
	Prompt            int64         `json:"prompt_tokens"`
	Completion        int64         `json:"completion_tokens"`
	Total             int64         `json:"total_tokens"`
	PromptDetails     *TokenDetails `json:"prompt_tokens_details,omitempty"`
	CompletionDetails *TokenDetails `json:"completion_tokens_details,omitempty"`
	ReasoningTokens   *int64        `json:"reasoning_tokens,omitempty"`
}
type TokenDetails struct {
	Cached    int64 `json:"cached_tokens,omitempty"`
	Reasoning int64 `json:"reasoning_tokens,omitempty"`
	Audio     int64 `json:"audio_tokens,omitempty"`
	Accepted  int64 `json:"accepted_prediction_tokens,omitempty"`
	Rejected  int64 `json:"rejected_prediction_tokens,omitempty"`
}

func (c *Completion) UnmarshalJSON(b []byte) error {
	type plain Completion
	return fields(b, (*plain)(c), "id object created model choices usage system_fingerprint")
}
func (c *Choice) UnmarshalJSON(b []byte) error {
	type plain Choice
	return fields(b, (*plain)(c), "index message finish_reason logprobs matched_stop")
}
func (u *Usage) UnmarshalJSON(b []byte) error {
	type plain Usage
	return fields(b, (*plain)(u), "prompt_tokens completion_tokens total_tokens prompt_tokens_details completion_tokens_details reasoning_tokens")
}
func (t *TokenDetails) UnmarshalJSON(b []byte) error {
	type plain TokenDetails
	return fields(b, (*plain)(t), "cached_tokens reasoning_tokens audio_tokens accepted_prediction_tokens rejected_prediction_tokens")
}

// parseCompletion runs only after the model receipt has verified. Unknown
// response fields are rejected, rather than relayed with undefined semantics.
func parseCompletion(raw []byte, model string) (Completion, error) {
	var c Completion
	if !validJSON(raw) || json.Unmarshal(raw, &c) != nil || !opaqueID.MatchString(c.ID) || c.Object != "chat.completion" || c.Model != model || c.Created <= 0 || len(c.Choices) != 1 {
		return c, errInvalid
	}
	choice := c.Choices[0]
	m := choice.Message
	if choice.Index != 0 || m.Role != "assistant" || m.Name != "" || m.ToolCallID != "" || !slices.Contains([]string{"stop", "length", "tool_calls", "content_filter"}, choice.FinishReason) {
		return c, errInvalid
	}
	if len(choice.Logprobs) > 0 && !bytes.Equal(choice.Logprobs, []byte("null")) {
		return c, errInvalid
	}
	// SGLang reports its matched end-token ID. Accept only numeric/null metadata,
	// never an unhandled free-text stop string; omit this vendor field locally.
	if !validStop(choice.MatchedStop) {
		return c, errInvalid
	}
	c.Choices[0].MatchedStop = nil
	// Initial outputs are text or native function calls; no multimodal output.
	if len(m.Content) > 0 && !bytes.Equal(m.Content, []byte("null")) {
		var text string
		if json.Unmarshal(m.Content, &text) != nil {
			return c, errInvalid
		}
	}
	seen := map[string]bool{}
	for _, call := range m.ToolCalls {
		if !validCall(call) || seen[call.ID] {
			return c, errInvalid
		}
		seen[call.ID] = true
	}
	if (choice.FinishReason == "tool_calls") != (len(m.ToolCalls) > 0) {
		return c, errInvalid
	}
	if normalizeUsage(c.Usage) != nil {
		return c, errInvalid
	}
	return c, nil
}

func validStop(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return true
	}
	var token int64
	return json.Unmarshal(raw, &token) == nil && token >= 0
}

func normalizeUsage(u *Usage) error {
	if u == nil {
		return nil
	}
	if u.ReasoningTokens != nil {
		if *u.ReasoningTokens < 0 {
			return errInvalid
		}
		if u.CompletionDetails == nil {
			u.CompletionDetails = &TokenDetails{}
		}
		if u.CompletionDetails.Reasoning == 0 {
			u.CompletionDetails.Reasoning = *u.ReasoningTokens
		}
		u.ReasoningTokens = nil
	}
	if u.Prompt < 0 || u.Completion < 0 || u.Total < 0 {
		return errInvalid
	}
	for _, d := range []*TokenDetails{u.PromptDetails, u.CompletionDetails} {
		if d != nil && (d.Cached < 0 || d.Reasoning < 0 || d.Audio < 0 || d.Accepted < 0 || d.Rejected < 0) {
			return errInvalid
		}
	}
	return nil
}
