package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

var errInvalid = errors.New("invalid or unsupported JSON")

// fields rejects unknown and incorrectly capitalized keys. encoding/json's
// default struct decoding is case-insensitive, which is too permissive here.
// Nested protocol objects implement the same check; encrypted schemas are data.
func fields(raw []byte, destination any, allowed string) error {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return errInvalid
	}
	for key := range object {
		if !slices.Contains(strings.Fields(allowed), key) {
			return errInvalid
		}
	}
	if json.Unmarshal(raw, destination) != nil {
		return errInvalid
	}
	return nil
}

// validJSON rejects duplicate keys before ordinary decoding can discard them.
// It also bounds nesting, rejects invalid UTF-8 and requires exactly one value.
func validJSON(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if scanValue(decoder, 0) != nil {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func scanValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errInvalid
	}
	token, err := decoder.Token()
	if err != nil {
		return errInvalid
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for decoder.More() {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok || seen[key] {
				return errInvalid
			}
			seen[key] = true
			if scanValue(decoder, depth+1) != nil {
				return errInvalid
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errInvalid
		}
	case json.Delim('['):
		for decoder.More() {
			if scanValue(decoder, depth+1) != nil {
				return errInvalid
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errInvalid
		}
	case json.Delim('}'), json.Delim(']'):
		return errInvalid
	}
	return nil
}
