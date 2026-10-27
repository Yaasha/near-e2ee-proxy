//go:build benchmark

package near

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"near-e2ee-proxy/internal/proxy"

	nearai "github.com/nearai/inference-sdk/go"
)

// This opt-in benchmark sends exactly ten sequential synthetic requests, each
// capped at 256 output tokens. The ordinary API baseline is intentionally outside
// the production proxy and is never a fallback for confidential user requests.
// Only phase durations, counts and fixed metadata are recorded, never bodies.
func TestProxyBenchmark(t *testing.T) {
	if os.Getenv("NEAR_BENCHMARK") != "1" {
		t.Skip("explicit paid benchmark opt-in required")
	}
	key := os.Getenv("NEAR_AI_API_KEY")
	if key == "" {
		t.Fatal("NEAR_AI_API_KEY is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	backend, err := New(key)
	if err != nil {
		t.Fatal("cannot construct verified client")
	}
	defer backend.Close()
	meter := &timedSDK{sdkClient: backend.sdk}
	backend.sdk = meter
	model := "Qwen/Qwen3.8-27B"
	proxyKey := rand.Text() + rand.Text()
	server := httptest.NewServer(proxy.NewServer(proxy.Config{ProxyKey: proxyKey, Models: []string{model}}, backend))
	defer server.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: proxy.RequestTimeout + time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	input := map[string]any{
		"model": model, "messages": []any{map[string]string{"role": "user", "content": "Write a long numbered list of short facts about basic arithmetic. Continue the list until the output limit. Do not include an introduction or conclusion."}},
		"temperature": 0, "seed": 42, "max_completion_tokens": 256,
		"chat_template_kwargs": map[string]bool{"enable_thinking": false},
	}
	report := benchmarkReport{StartedUTC: time.Now().UTC().Format(time.RFC3339), Model: model, OutputLimit: 256, Concurrency: 1, StreamingPolicy: "provisional_text_verified_tools"}
	for pair := 0; pair < 4; pair++ {
		// Alternate order to reduce systematic advantage from traffic/cache drift.
		paths := []string{"proxy", "direct"}
		if pair%2 == 1 {
			paths = []string{"direct", "proxy"}
		}
		for _, path := range paths {
			meter.reset()
			endpoint, token := nearai.DefaultBaseURL+"/chat/completions", key
			if path == "proxy" {
				endpoint, token = server.URL+"/v1/chat/completions", proxyKey
			}
			label := "warm"
			if pair == 0 {
				label = "client_cold"
			}
			row := measureRequest(t, ctx, client, endpoint, token, path, label, pair+1, input, false)
			if path == "proxy" {
				meter.copyInto(&row)
			}
			report.Samples = append(report.Samples, row)
			t.Logf("%s %s: total %.1f ms, attest %.1f ms, inference exchange %.1f ms, receipt %.1f ms; %d output tokens", path, label, row.TotalMS, row.AttestationMS, row.InferenceMS, row.SignatureMS, row.OutputTokens)
		}
	}
	// One streaming pair measures provisional time-to-first-content. Successful
	// completion still includes the final model receipt. This is not pure GPU speed.
	for _, path := range []string{"direct", "proxy"} {
		meter.reset()
		endpoint, token := nearai.DefaultBaseURL+"/chat/completions", key
		if path == "proxy" {
			endpoint, token = server.URL+"/v1/chat/completions", proxyKey
		}
		row := measureRequest(t, ctx, client, endpoint, token, path, "warm_streaming", 5, input, true)
		if path == "proxy" {
			meter.copyInto(&row)
		}
		report.Samples = append(report.Samples, row)
		t.Logf("%s streaming: first content %.1f ms, total %.1f ms; %d output tokens", path, row.FirstContentMS, row.TotalMS, row.OutputTokens)
	}
	report.FinishedUTC = time.Now().UTC().Format(time.RFC3339)
	output := os.Getenv("NEAR_BENCHMARK_OUTPUT")
	if output == "" {
		t.Fatal("NEAR_BENCHMARK_OUTPUT is required")
	}
	if os.MkdirAll(filepath.Dir(output), 0755) != nil {
		t.Fatal("cannot create benchmark report directory")
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil || os.WriteFile(output, append(encoded, '\n'), 0644) != nil {
		t.Fatal("cannot save metadata-only benchmark report")
	}
}

type benchmarkReport struct {
	StreamingPolicy string            `json:"proxy_streaming_policy"`
	StartedUTC      string            `json:"started_utc"`
	FinishedUTC     string            `json:"finished_utc"`
	Model           string            `json:"model"`
	OutputLimit     int               `json:"max_completion_tokens"`
	Concurrency     int               `json:"concurrency"`
	Samples         []benchmarkSample `json:"samples"`
}
type benchmarkSample struct {
	Path              string  `json:"path"`
	State             string  `json:"state"`
	Pair              int     `json:"pair"`
	TotalMS           float64 `json:"total_ms"`
	FirstContentMS    float64 `json:"first_content_ms,omitempty"`
	AttestationMS     float64 `json:"attestation_ms,omitempty"`
	InferenceMS       float64 `json:"inference_exchange_ms,omitempty"`
	SignatureMS       float64 `json:"signature_ms,omitempty"`
	OtherMS           float64 `json:"other_ms,omitempty"`
	SignatureAttempts int     `json:"signature_attempts,omitempty"`
	InputTokens       int64   `json:"input_tokens"`
	CachedTokens      int64   `json:"cached_input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	ReasoningTokens   int64   `json:"reasoning_tokens"`
	TokensPerSecond   float64 `json:"end_to_end_output_tokens_per_second"`
}

// The wrapper surrounds real SDK methods; it does not replace verification.
// The real HTTP handler runs on another goroutine; the mutex protects counters.
type timedSDK struct {
	sdkClient
	mu                           sync.Mutex
	attest, inference            time.Duration
	signatureStart, signatureEnd time.Time
	inferenceStart               time.Time
	attempts                     int
}

func (m *timedSDK) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attest = 0
	m.inference = 0
	m.signatureStart = time.Time{}
	m.signatureEnd = time.Time{}
	m.attempts = 0
	m.inferenceStart = time.Time{}
}
func (m *timedSDK) Verify(ctx context.Context, model string) (nearai.AttestationVerificationResult, error) {
	start := time.Now()
	result, err := m.sdkClient.Verify(ctx, model)
	elapsed := time.Since(start)
	m.mu.Lock()
	m.attest += elapsed
	m.inferenceStart = time.Now()
	m.mu.Unlock()
	return result, err
}
func (m *timedSDK) CreateChatCompletion(ctx context.Context, request any) (json.RawMessage, error) {
	start := time.Now()
	result, err := m.sdkClient.CreateChatCompletion(ctx, request)
	elapsed := time.Since(start)
	m.mu.Lock()
	m.inference += elapsed
	m.mu.Unlock()
	return result, err
}
func (m *timedSDK) VerifyResponse(ctx context.Context, id string) (nearai.VerifiedCompletionResult, error) {
	m.mu.Lock()
	if m.signatureStart.IsZero() {
		m.signatureStart = time.Now()
		// Native streaming runs through HTTPClient rather than CreateChatCompletion.
		// Include its full exchange and local per-chunk validation before the receipt.
		if m.inference == 0 && !m.inferenceStart.IsZero() {
			m.inference = m.signatureStart.Sub(m.inferenceStart)
		}
	}
	m.attempts++
	m.mu.Unlock()
	result, err := m.sdkClient.VerifyResponse(ctx, id)
	end := time.Now()
	m.mu.Lock()
	m.signatureEnd = end
	m.mu.Unlock()
	return result, err
}
func (m *timedSDK) copyInto(row *benchmarkSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row.AttestationMS = float64(m.attest) / float64(time.Millisecond)
	row.InferenceMS = float64(m.inference) / float64(time.Millisecond)
	if !m.signatureStart.IsZero() {
		row.SignatureMS = float64(m.signatureEnd.Sub(m.signatureStart)) / float64(time.Millisecond)
	}
	row.SignatureAttempts = m.attempts
	row.OtherMS = row.TotalMS - row.AttestationMS - row.InferenceMS - row.SignatureMS
}

func measureRequest(t *testing.T, ctx context.Context, client *http.Client, endpoint, key, path, state string, pair int, input map[string]any, stream bool) benchmarkSample {
	t.Helper()
	input["stream"] = stream
	if stream {
		input["stream_options"] = map[string]bool{"include_usage": true}
	} else {
		delete(input, "stream_options")
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal("cannot encode synthetic request")
	}
	request, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(raw))
	if err != nil {
		t.Fatal("cannot build benchmark request")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	if path == "direct" {
		request.Header.Set(nearai.NoAliasingHeader, "true")
	}
	start := time.Now()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("benchmark transport failed (details suppressed)")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("%s benchmark HTTP status %d (body suppressed)", path, response.StatusCode)
	}
	row := benchmarkSample{Path: path, State: state, Pair: pair}
	var usage struct {
		Prompt        int64 `json:"prompt_tokens"`
		Completion    int64 `json:"completion_tokens"`
		PromptDetails struct {
			Cached int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionDetails struct {
			Reasoning int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	reader := io.LimitReader(response.Body, proxy.MaxResponseBytes+1)
	if stream {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		done := false
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				done = true
				continue
			}
			var event struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
				Usage json.RawMessage `json:"usage"`
			}
			if json.Unmarshal([]byte(data), &event) != nil {
				t.Fatal("invalid benchmark SSE event")
			}
			for _, choice := range event.Choices {
				if choice.Delta.Content != "" && row.FirstContentMS == 0 {
					row.FirstContentMS = float64(time.Since(start)) / float64(time.Millisecond)
				}
			}
			if len(event.Usage) > 0 && string(event.Usage) != "null" {
				if json.Unmarshal(event.Usage, &usage) != nil {
					t.Fatal("invalid benchmark usage")
				}
			}
		}
		if scanner.Err() != nil || !done || row.FirstContentMS == 0 {
			t.Fatal("benchmark SSE did not complete")
		}
	} else {
		body, err := io.ReadAll(reader)
		if err != nil || len(body) > proxy.MaxResponseBytes {
			t.Fatal("cannot read bounded benchmark response")
		}
		var result struct {
			Model string          `json:"model"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(body, &result) != nil || result.Model != input["model"] || json.Unmarshal(result.Usage, &usage) != nil {
			t.Fatal("benchmark model/usage mismatch")
		}
	}
	row.TotalMS = float64(time.Since(start)) / float64(time.Millisecond)
	row.InputTokens, row.CachedTokens, row.OutputTokens, row.ReasoningTokens = usage.Prompt, usage.PromptDetails.Cached, usage.Completion, usage.CompletionDetails.Reasoning
	if row.OutputTokens <= 0 || row.OutputTokens > 256 || row.ReasoningTokens != 0 {
		t.Fatal("benchmark output/disabled-reasoning contract failed")
	}
	row.TokensPerSecond = float64(row.OutputTokens) / (row.TotalMS / 1000)
	return row
}
