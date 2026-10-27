// Streaming keeps text responsive while withholding executable tool fields and
// successful completion until the upstream model receipt has been verified.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

type streamChunk struct {
	ID          string         `json:"id"`
	Object      string         `json:"object"`
	Created     int64          `json:"created"`
	Model       string         `json:"model"`
	Choices     []streamChoice `json:"choices"`
	Usage       *Usage         `json:"usage"`
	Fingerprint *string        `json:"system_fingerprint,omitempty"`
}
type streamChoice struct {
	Index        int             `json:"index"`
	Delta        streamDelta     `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
	MatchedStop  json.RawMessage `json:"matched_stop,omitempty"`
}
type streamDelta struct {
	Role             string      `json:"role,omitempty"`
	Content          *string     `json:"content,omitempty"`
	ReasoningContent *string     `json:"reasoning_content,omitempty"`
	Reasoning        *string     `json:"reasoning,omitempty"`
	Refusal          *string     `json:"refusal,omitempty"`
	ToolCalls        []toolDelta `json:"tool_calls,omitempty"`
}

// Pointer fields distinguish a missing value from zero/empty. In particular,
// tool index 0 is valid, but a fragment with no index cannot be correlated safely.
type toolDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

func (c *streamChunk) UnmarshalJSON(b []byte) error {
	type plain streamChunk
	return fields(b, (*plain)(c), "id object created model choices usage system_fingerprint")
}
func (c *streamChoice) UnmarshalJSON(b []byte) error {
	type plain streamChoice
	return fields(b, (*plain)(c), "index delta finish_reason logprobs matched_stop")
}
func (d *streamDelta) UnmarshalJSON(b []byte) error {
	type plain streamDelta
	return fields(b, (*plain)(d), "role content reasoning_content reasoning refusal tool_calls")
}
func (d *toolDelta) UnmarshalJSON(b []byte) error {
	type plain toolDelta
	if fields(b, (*plain)(d), "index id type function") != nil {
		return errInvalid
	}
	var object map[string]json.RawMessage
	json.Unmarshal(b, &object)
	if function, ok := object["function"]; ok {
		if fields(function, &d.Function, "name arguments") != nil {
			return errInvalid
		}
	}
	return nil
}

type bufferedTool struct {
	id, kind        string
	name, arguments strings.Builder
}
type streamState struct {
	// Only metadata and the latest usage are kept here; text is not accumulated.
	completion Completion
	finish     *string
	tools      map[int]*bufferedTool
	bytes      int
}

// accept validates every chunk before anything is exposed. Only text-like fields
// are returned for preview. Tool fields stay in this bounded request-local state.
func (s *streamState) accept(raw json.RawMessage, model string) (streamDelta, error) {
	s.bytes += len(raw)
	var chunk streamChunk
	if s.bytes > MaxResponseBytes || !validJSON(raw) || json.Unmarshal(raw, &chunk) != nil || chunk.Model != model || chunk.Object != "chat.completion.chunk" || !opaqueID.MatchString(chunk.ID) || chunk.Created <= 0 || len(chunk.Choices) > 1 {
		return streamDelta{}, errInvalid
	}
	if s.completion.ID == "" {
		s.completion = Completion{ID: chunk.ID, Created: chunk.Created, Model: model, Object: "chat.completion"}
		s.tools = map[int]*bufferedTool{}
	}
	// NEAR's engine refreshes created during generation. Keep the first timestamp
	// locally while requiring the same response ID/model throughout the stream.
	if chunk.ID != s.completion.ID {
		return streamDelta{}, errInvalid
	}
	if normalizeUsage(chunk.Usage) != nil {
		return streamDelta{}, errInvalid
	}
	if chunk.Usage != nil {
		s.completion.Usage = chunk.Usage
	}
	if len(chunk.Choices) == 0 {
		if chunk.Usage == nil {
			return streamDelta{}, errInvalid
		}
		return streamDelta{}, nil
	}
	choice := chunk.Choices[0]
	d := choice.Delta
	if choice.Index != 0 || (d.Role != "" && d.Role != "assistant") || !validStop(choice.MatchedStop) || (len(choice.Logprobs) > 0 && string(choice.Logprobs) != "null") {
		return streamDelta{}, errInvalid
	}
	hasData := d.Content != nil || d.ReasoningContent != nil || d.Reasoning != nil || d.Refusal != nil || len(d.ToolCalls) > 0
	if s.finish != nil && (hasData || choice.FinishReason != nil) {
		return streamDelta{}, errInvalid
	}
	for _, call := range d.ToolCalls {
		if call.Index == nil || *call.Index < 0 || *call.Index >= 128 || (call.ID != "" && !opaqueID.MatchString(call.ID)) || (call.Type != "" && call.Type != "function") {
			return streamDelta{}, errInvalid
		}
		tool := s.tools[*call.Index]
		if tool == nil {
			tool = &bufferedTool{}
			s.tools[*call.Index] = tool
		}
		if call.ID != "" {
			if tool.id != "" && tool.id != call.ID {
				return streamDelta{}, errInvalid
			}
			tool.id = call.ID
		}
		if call.Type != "" {
			tool.kind = call.Type
		}
		tool.name.WriteString(call.Function.Name)
		tool.arguments.WriteString(call.Function.Arguments)
	}
	if choice.FinishReason != nil {
		s.finish = choice.FinishReason
	}
	d.ToolCalls = nil
	return d, nil
}

// finishTools runs only after Backend.Stream returns a verified success. It
// validates fragmented calls as complete calls before releasing a single byte.
func (s *streamState) finishTools() ([]toolDelta, error) {
	if s.finish == nil || !slices.Contains([]string{"stop", "length", "tool_calls", "content_filter"}, *s.finish) || ((*s.finish == "tool_calls") != (len(s.tools) > 0)) {
		return nil, errInvalid
	}
	calls := make([]toolDelta, 0, len(s.tools))
	seen := map[string]bool{}
	for i := 0; i < len(s.tools); i++ {
		tool := s.tools[i]
		if tool == nil {
			return nil, errInvalid
		}
		call := ToolCall{ID: tool.id, Type: tool.kind, Function: CallFunction{Name: tool.name.String(), Arguments: tool.arguments.String()}}
		if !validCall(call) || seen[call.ID] {
			return nil, errInvalid
		}
		seen[call.ID] = true
		index := i
		delta := toolDelta{Index: &index, ID: call.ID, Type: call.Type}
		delta.Function.Name = call.Function.Name
		delta.Function.Arguments = call.Function.Arguments
		calls = append(calls, delta)
	}
	return calls, nil
}

func (s *Server) stream(ctx context.Context, w http.ResponseWriter, request json.RawMessage, model string, includeUsage bool) {
	state := streamState{}
	started := false
	// Re-encode allowed fields, never forward untrusted SSE controls or terminal
	// markers. The final receipt covers the original wire stream held by the SDK.
	write := func(delta streamDelta, finish *string, usage *Usage) error {
		choices := []streamChoice{{Index: 0, Delta: delta, FinishReason: finish}}
		if usage != nil {
			choices = []streamChoice{}
		}
		// Construct output from typed, validated fields. Avoid decoding back through
		// interface{}: that would convert integer metadata to float64.
		chunk := map[string]any{"id": state.completion.ID, "object": "chat.completion.chunk", "created": state.completion.Created, "model": model, "choices": choices}
		if includeUsage {
			chunk["usage"] = usage
		}
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return errInvalid
		}
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Response-Verification", "end-of-stream")
			started = true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	}
	// Stream invokes this callback as decrypted chunks arrive. Its return value
	// is the verification boundary: callbacks alone never authorize tool delivery.
	err := s.backend.Stream(ctx, request, func(raw json.RawMessage) error {
		delta, err := state.accept(raw, model)
		if err != nil {
			return err
		}
		if delta.Role == "" && delta.Content == nil && delta.ReasoningContent == nil && delta.Reasoning == nil && delta.Refusal == nil {
			return nil
		}
		return write(delta, nil, nil)
	})
	var tools []toolDelta
	if err == nil {
		tools, err = state.finishTools()
	}
	if err != nil {
		if !started {
			if ctx.Err() != nil {
				writeError(w, 504, "server_error", "request_timeout", "The inference deadline expired or the request was cancelled.")
			} else {
				writeError(w, 502, "server_error", "verification_failed", "The encrypted stream could not be verified.")
			}
			return
		}
		// The HTTP status is already 200. Explicit failure and absence of finish/
		// [DONE] are essential, but cannot retract text a client already consumed.
		_, _ = fmt.Fprint(w, "event: error\ndata: {\"error\":{\"message\":\"The encrypted stream could not be verified; discard provisional output.\",\"type\":\"server_error\",\"code\":\"verification_failed\"}}\n\n")
		_ = http.NewResponseController(w).Flush()
		return
	}
	if len(tools) > 0 {
		if write(streamDelta{ToolCalls: tools}, nil, nil) != nil {
			return
		}
	}
	if write(streamDelta{}, state.finish, nil) != nil {
		return
	}
	if includeUsage && state.completion.Usage != nil {
		if write(streamDelta{}, nil, state.completion.Usage) != nil {
			return
		}
	}
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err == nil {
		_ = http.NewResponseController(w).Flush()
	}
}
