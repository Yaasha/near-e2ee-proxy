package near

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

func TestReadEventsFramingAndLimits(t *testing.T) {
	for _, separator := range []string{"\n", "\r\n", "\r"} {
		wire := strings.Join([]string{": heartbeat", "event: message", `data: {"id":`, `data: "synthetic-completion"}`, "", "data: [DONE]", "", ""}, separator)
		count := 0
		err := readEvents(iotest.OneByteReader(strings.NewReader(wire)), func(b json.RawMessage) error {
			count++
			if !json.Valid(b) {
				t.Fatal("multiline JSON must survive fragmented SSE framing")
			}
			return nil
		})
		if err != nil || count != 1 {
			t.Fatal("expected one event and a complete stream")
		}
	}
	for name, wire := range map[string]string{
		"truncated":       "data: {}\n\n",
		"after done":      "data: [DONE]\n\ndata: {}\n\n",
		"duplicate done":  "data: [DONE]\n\ndata: [DONE]\n\n",
		"error event":     "event: error\ndata: {}\n\ndata: [DONE]\n\n",
		"unknown control": "retry: 100\ndata: [DONE]\n\n",
		"long line":       "data: " + strings.Repeat("x", 1<<20) + "\n\ndata: [DONE]\n\n",
		"large total":     strings.Repeat(": heartbeat\n", 800000) + "data: [DONE]\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			if readEvents(strings.NewReader(wire), func(json.RawMessage) error { return nil }) == nil {
				t.Fatal("invalid stream must fail closed")
			}
		})
	}
	wire := "data: {}\n\ndata: [DONE]\n\n"
	if readEvents(strings.NewReader(wire), func(json.RawMessage) error { return errors.New("client disconnected") }) == nil {
		t.Fatal("callback cancellation must stop parsing")
	}
	if readEvents(iotest.ErrReader(errors.New("read failure")), func(json.RawMessage) error { return nil }) == nil {
		t.Fatal("transport failure must invalidate stream")
	}
}

type streamTransport func(*http.Request) (*http.Response, error)

func (f streamTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type eofReader struct {
	io.Reader
	reached bool
}

func (r *eofReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		r.reached = true
	}
	return n, err
}

// VerifyResponse may only run after EOF closes the SDK's exact-wire capture.
// The synthetic transport substitutes no production verifier or crypto code.
func TestStreamEvidenceReceiptAndPreviewOrder(t *testing.T) {
	for _, failure := range []string{"", "evidence", "receipt", "gateway", "wrong id", "missing signer", "truncated", "changed id", "callback", "status", "mime"} {
		t.Run(failure, func(t *testing.T) {
			f := validSDK()
			switch failure {
			case "evidence":
				f.verifyErr = errors.New("private")
			case "receipt":
				f.receiptErr = errors.New("private")
			case "gateway":
				f.receipt.SignatureKind = "gateway"
			case "wrong id":
				f.receipt.ID = "other"
			case "missing signer":
				f.receipt.Attestation = nil
			}
			requests, previews := 0, 0
			var wireReader *eofReader
			f.beforeReceipt = func() {
				if wireReader == nil || !wireReader.reached {
					t.Fatal("verify only after complete stream capture")
				}
			}
			f.httpClient = &http.Client{Transport: streamTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.URL.Scheme != "https" || r.Header.Get("Accept") != "text/event-stream" {
					t.Fatal("expected native SDK HTTPS route")
				}
				wire := "data: {\"id\":\"synthetic-completion\"}\n\n"
				if failure == "changed id" {
					wire += "data: {\"id\":\"other\"}\n\n"
				}
				if failure != "truncated" {
					wire += "data: [DONE]\n\n"
				}
				status, mime := 200, "text/event-stream"
				if failure == "status" {
					status = 500
				}
				if failure == "mime" {
					mime = "application/json"
				}
				wireReader = &eofReader{Reader: strings.NewReader(wire)}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {mime}}, Body: io.NopCloser(wireReader)}, nil
			})}
			c := &Client{sdk: f}
			err := c.Stream(context.Background(), json.RawMessage(`{"model":"synthetic","stream":true}`), func(json.RawMessage) error {
				previews++
				if f.receipts != 0 {
					t.Fatal("text must arrive before receipt verification")
				}
				if failure == "callback" {
					return errors.New("private")
				}
				return nil
			})
			if failure == "" {
				if err != nil || previews != 1 || requests != 1 || f.receipts != 1 {
					t.Fatal("expected preview then verified completion")
				}
			} else if err != ErrVerification {
				t.Fatal("failures must be sanitized")
			}
			if failure == "evidence" && requests != 0 {
				t.Fatal("never send inference before evidence checks")
			}
			if len(f.verifiedModels) != 1 || f.verifiedModels[0] != "synthetic" {
				t.Fatal("streaming must verify the requested model")
			}
			if (failure == "truncated" || failure == "changed id" || failure == "callback") && f.receipts != 0 {
				t.Fatal("incomplete wire capture cannot be verified")
			}
			if requests > 1 || f.creates != 0 {
				t.Fatal("stream must not replay or use nonstreaming inference")
			}
		})
	}
}
