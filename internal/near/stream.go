package near

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
)

// Stream uses the SDK's real encrypted stream and exact-wire receipt retention.
// emit receives provisional decrypted JSON, never trusted tool execution. The
// HTTP layer validates it and buffers tools/terminal events. A nil return alone
// authorizes successful completion; any error invalidates already-previewed text.
func (c *Client) Stream(ctx context.Context, request json.RawMessage, emit func(json.RawMessage) error) error {
	var input struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if ctx.Err() != nil || json.Unmarshal(request, &input) != nil || input.Model == "" || !input.Stream {
		return ErrVerification
	}
	if err := c.Check(ctx, input.Model); err != nil {
		return err
	}
	url := strings.TrimRight(c.sdk.BaseURL(), "/") + "/chat/completions"
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(request))
	if err != nil {
		return ErrVerification
	}
	r.Header.Set("Accept", "text/event-stream")
	response, err := c.sdk.HTTPClient().Do(r)
	if err != nil {
		return ErrVerification
	}
	defer response.Body.Close()
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || response.StatusCode != http.StatusOK || mediaType != "text/event-stream" {
		return ErrVerification
	}
	id := ""
	err = readEvents(response.Body, func(data json.RawMessage) error {
		var chunk struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &chunk) != nil || chunk.ID == "" || (id != "" && id != chunk.ID) {
			return ErrVerification
		}
		id = chunk.ID
		return emit(data)
	})
	if err != nil || id == "" {
		return ErrVerification
	}
	return c.verifyReceipt(ctx, id)
}

// readEvents consumes through EOF, not merely [DONE]: the SDK must finish its
// original-wire capture before VerifyResponse. Neither raw terminal markers nor
// upstream error text are forwarded. Limits apply to the decrypted SSE stream.
func readEvents(reader io.Reader, emit func(json.RawMessage) error) error {
	const totalLimit = 8 << 20
	limited := &io.LimitedReader{R: reader, N: totalLimit + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	scanner.Split(eventLine)
	var data strings.Builder
	done := false
	dispatch := func() error {
		if data.Len() == 0 {
			return nil
		}
		payload := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		if done {
			return ErrVerification
		}
		if payload == "[DONE]" {
			done = true
			return nil
		}
		return emit(json.RawMessage(payload))
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if dispatch() != nil {
				return ErrVerification
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			if strings.TrimSpace(strings.TrimPrefix(line, "event:")) != "message" {
				return ErrVerification
			}
			continue
		}
		if line == "data" {
			line = "data:"
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len()+len(line) > 1<<20 {
				return ErrVerification
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			data.WriteByte('\n')
		} else {
			return ErrVerification
		}
	}
	if scanner.Err() != nil || limited.N <= 0 || dispatch() != nil || !done {
		return ErrVerification
	}
	return nil
}

// SSE permits LF, CRLF and lone CR. A CR at a read boundary waits for the next
// byte so a fragmented CRLF cannot accidentally create an empty event separator.
func eventLine(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		switch b {
		case '\n':
			return i + 1, data[:i], nil
		case '\r':
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			if i+1 < len(data) && data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
