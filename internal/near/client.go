// Package near is the only production inference path. The official SDK handles
// cryptography; this wrapper enforces verification before and after inference.
package near

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	nearai "github.com/nearai/inference-sdk/go"
)

// ErrVerification is deliberately safe to expose as a category. SDK errors and
// their causes may contain upstream data and must not be logged or returned.
var ErrVerification = errors.New("confidential inference verification failed")

// sdkClient is a test seam for checking the release-before-verification boundary.
// Production construction below always uses the official SDK's real verifiers.
type sdkClient interface {
	Verify(context.Context, string) (nearai.AttestationVerificationResult, error)
	CreateChatCompletion(context.Context, any) (json.RawMessage, error)
	VerifyResponse(context.Context, string) (nearai.VerifiedCompletionResult, error)
	HTTPClient() *http.Client
	BaseURL() string
	Close() error
}

type Client struct {
	sdk       sdkClient
	transport *http.Transport
}

// New fixes the upstream URL and security settings. Clients cannot override
// routing, verification, credentials, encryption keys or encryption headers.
func New(apiKey string) (*Client, error) {
	if apiKey == "" {
		return nil, ErrVerification
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client, err := nearai.NewInferenceClient(options(apiKey, transport))
	if err != nil {
		transport.CloseIdleConnections()
		return nil, ErrVerification
	}
	return &Client{sdk: client, transport: transport}, nil
}

func options(apiKey string, transport *http.Transport) nearai.InferenceOptions {
	ttl, retention := 5*time.Minute, time.Minute
	quote := nearai.CreateTDXQuoteVerifier("")
	// The SDK checks signatures, collateral, debug mode and TCB status. Add an
	// explicit advisory policy instead of accidentally ignoring advisory IDs.
	checkQuote := func(ctx context.Context, raw string) (nearai.QuoteVerificationResult, error) {
		verified, err := quote(ctx, raw)
		if err == nil && len(verified.AdvisoryIDs) != 0 {
			err = ErrVerification
		}
		return verified, err
	}
	return nearai.InferenceOptions{
		ClientOptions: nearai.ClientOptions{
			APIKey: apiKey, BaseURL: nearai.DefaultBaseURL,
			HTTPClient: &http.Client{Transport: transport, Timeout: 180 * time.Second},
		},
		E2EE: true, SigningAlgo: nearai.Ed25519,
		AttestationTTL: &ttl, ResponseTTL: &retention,
		MaxBodyBytes: 32 << 20, MaxCacheEntries: 16, MaxReceiptCacheBytes: 128 << 20,
		GatewayVerification: nearai.VerificationOptions{
			Policy: nearai.AttestationPolicy{AcceptedTCBStatuses: []string{"UpToDate"}}, QuoteVerifier: checkQuote,
		},
		ModelVerification: nearai.VerificationOptions{
			Policy: nearai.AttestationPolicy{AcceptedTCBStatuses: []string{"UpToDate"}, RequireGPUEvidence: true}, QuoteVerifier: checkQuote,
		},
	}
}

func (c *Client) Close() error {
	err := c.sdk.Close()
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
	return err
}

// Check performs non-billable admission. The accepted policy trusts NEAR's
// deployment controls; it does not claim independent approval of every image.
func (c *Client) Check(ctx context.Context, model string) error {
	evidence, err := c.sdk.Verify(ctx, model)
	if err != nil || evidence.Gateway == nil || len(evidence.Models) == 0 {
		return ErrVerification
	}
	if evidence.Gateway.SPKIFingerprint == "" {
		return ErrVerification
	}
	for _, a := range append([]nearai.VerifiedAttestation{*evidence.Gateway}, evidence.Models...) {
		if a.TCBStatus != "UpToDate" || len(a.AdvisoryIDs) != 0 || a.Signer.SigningAlgo != nearai.Ed25519 {
			return ErrVerification
		}
	}
	for _, a := range evidence.Models {
		if a.GPUEvidence != "verified" || a.SigningPublicKey == "" {
			return ErrVerification
		}
	}
	return nil
}

// Complete makes exactly one nonstreaming inference request and returns only
// after its model receipt verifies against the exact encrypted wire bytes.
func (c *Client) Complete(ctx context.Context, request json.RawMessage) (json.RawMessage, error) {
	if ctx.Err() != nil {
		return nil, ErrVerification
	}
	var input struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(request, &input) != nil || input.Model == "" {
		return nil, ErrVerification
	}
	if err := c.Check(ctx, input.Model); err != nil {
		return nil, err
	}
	body, err := c.sdk.CreateChatCompletion(ctx, request)
	if err != nil {
		return nil, ErrVerification
	}
	var completion struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &completion) != nil || completion.ID == "" {
		return nil, ErrVerification
	}
	if err := c.verifyReceipt(ctx, completion.ID); err != nil {
		return nil, err
	}
	return body, nil
}

func (c *Client) verifyReceipt(ctx context.Context, id string) error {
	// Retry receipt retrieval only. Never retry inference, which could double
	// charge or execute the same logical action twice.
	for attempt := 0; attempt < 5; attempt++ {
		receipt, err := c.sdk.VerifyResponse(ctx, id)
		if err == nil {
			if receipt.SignatureKind != "provider_tee" || receipt.Attestation == nil || receipt.ID != id {
				return ErrVerification
			}
			return nil
		}
		var sdkError *nearai.Error
		if !errors.As(err, &sdkError) || (!sdkError.Retryable && sdkError.Code != "api.completion_signature_unavailable") || attempt == 4 {
			return ErrVerification
		}
		timer := time.NewTimer(time.Duration(1<<attempt) * 500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ErrVerification
		case <-timer.C:
		}
	}
	return ErrVerification
}
