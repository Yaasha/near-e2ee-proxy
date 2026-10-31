package proxy

import "encoding/json"

type requestFieldError struct {
	param, message string
}

func (e *requestFieldError) Error() string { return e.message }
func (e *requestFieldError) Unwrap() error { return errInvalid }

// normalizeThinking translates a reviewed client alias into NEAR's documented
// toggle. Only a boolean is accepted: it remains observable protocol metadata,
// like the existing nested toggle, and cannot carry arbitrary unencrypted text.
// Unknown fields are preserved so the ordinary recursive allowlist rejects them.
func normalizeThinking(raw []byte) ([]byte, error) {
	var request map[string]json.RawMessage
	if json.Unmarshal(raw, &request) != nil {
		return nil, errInvalid
	}
	if _, exists := request["thinking_budget"]; exists {
		// An effort tier is not an exact token cap. Do not silently drop a budget,
		// turn it into a generation cap, or invent an unsupported backend mapping.
		return nil, &requestFieldError{
			param:   "thinking_budget",
			message: "thinking_budget is unsupported: an exact reasoning-token cap cannot be guaranteed. Omit it and use reasoning_effort or chat_template_kwargs.enable_thinking.",
		}
	}
	alias, exists := request["enable_thinking"]
	if !exists {
		return raw, nil
	}
	enabled, err := thinkingBoolean(alias, "enable_thinking")
	if err != nil {
		return nil, err
	}
	template := map[string]json.RawMessage{}
	if original, exists := request["chat_template_kwargs"]; exists {
		if json.Unmarshal(original, &template) != nil || template == nil {
			return nil, &requestFieldError{
				param: "chat_template_kwargs", message: "chat_template_kwargs must be an object when enable_thinking is specified.",
			}
		}
		if nested, exists := template["enable_thinking"]; exists {
			canonical, err := thinkingBoolean(nested, "chat_template_kwargs.enable_thinking")
			if err != nil {
				return nil, err
			}
			if canonical != enabled {
				return nil, &requestFieldError{
					param: "enable_thinking", message: "enable_thinking conflicts with chat_template_kwargs.enable_thinking.",
				}
			}
		}
	}
	template["enable_thinking"] = alias
	request["chat_template_kwargs"], err = json.Marshal(template)
	if err != nil {
		return nil, errInvalid
	}
	delete(request, "enable_thinking")
	return json.Marshal(request)
}

func thinkingBoolean(raw json.RawMessage, param string) (bool, error) {
	var value *bool
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return false, &requestFieldError{param: param, message: param + " must be a boolean."}
	}
	return *value, nil
}
