package near

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	nearai "github.com/nearai/inference-sdk/go"
)

type fakeSDK struct {
	evidence                         nearai.AttestationVerificationResult
	verifyErr, createErr, receiptErr error
	receipt                          nearai.VerifiedCompletionResult
	body                             json.RawMessage
	creates, receipts                int
	transientReceipts                int
	httpClient                       *http.Client
	beforeReceipt                    func()
	verifiedModels                   []string
}

func (f *fakeSDK) Verify(_ context.Context, model string) (nearai.AttestationVerificationResult, error) {
	f.verifiedModels = append(f.verifiedModels, model)
	return f.evidence, f.verifyErr
}

func TestRequestedModelRequiresItsOwnEvidence(t *testing.T) {
	for _, valid := range []bool{false, true} {
		f := validSDK()
		if !valid {
			f.verifyErr = errors.New("unavailable confidential model")
		}
		c := &Client{sdk: f}
		_, err := c.Complete(context.Background(), json.RawMessage(`{"model":"provider/Another-Model"}`))
		if len(f.verifiedModels) != 1 || f.verifiedModels[0] != "provider/Another-Model" {
			t.Fatal("verify the selected model, never a default discovery model")
		}
		if valid {
			if err != nil || f.creates != 1 {
				t.Fatal("verified model should proceed")
			}
		} else if err == nil || f.creates != 0 {
			t.Fatal("unavailable evidence must block inference")
		}
	}
}
func (f *fakeSDK) CreateChatCompletion(context.Context, any) (json.RawMessage, error) {
	f.creates++
	return f.body, f.createErr
}
func (f *fakeSDK) VerifyResponse(context.Context, string) (nearai.VerifiedCompletionResult, error) {
	if f.beforeReceipt != nil {
		f.beforeReceipt()
	}
	f.receipts++
	if f.transientReceipts > 0 {
		f.transientReceipts--
		return nearai.VerifiedCompletionResult{}, &nearai.Error{Code: "api.completion_signature_unavailable"}
	}
	return f.receipt, f.receiptErr
}

func TestTransientReceiptRetrievalNeverReplaysInference(t *testing.T) {
	f := validSDK()
	f.transientReceipts = 1
	c := &Client{sdk: f}
	if body, err := c.Complete(context.Background(), json.RawMessage(`{"model":"synthetic"}`)); err != nil || len(body) == 0 || f.creates != 1 || f.receipts != 2 {
		t.Fatal("retry receipt retrieval only")
	}
}
func (f *fakeSDK) Close() error             { return nil }
func (f *fakeSDK) HTTPClient() *http.Client { return f.httpClient }
func (f *fakeSDK) BaseURL() string          { return nearai.DefaultBaseURL }

func validSDK() *fakeSDK {
	signer := nearai.SigningIdentity{SigningAlgo: nearai.Ed25519, SigningAddress: "synthetic"}
	a := nearai.VerifiedAttestation{Signer: signer, TCBStatus: "UpToDate", GPUEvidence: "verified", SigningPublicKey: "synthetic", SPKIFingerprint: "synthetic"}
	return &fakeSDK{
		evidence: nearai.AttestationVerificationResult{Gateway: &a, Models: []nearai.VerifiedAttestation{a}},
		body:     json.RawMessage(`{"id":"synthetic-completion","private":"synthetic-answer"}`),
		receipt:  nearai.VerifiedCompletionResult{ID: "synthetic-completion", SignatureKind: "provider_tee", Attestation: &a},
	}
}

func TestCompleteRequiresEvidenceAndModelReceipt(t *testing.T) {
	for name, mutate := range map[string]func(*fakeSDK){
		"bad quote":       func(f *fakeSDK) { f.verifyErr = errors.New("private upstream text") },
		"missing gateway": func(f *fakeSDK) { f.evidence.Gateway = nil },
		"missing model":   func(f *fakeSDK) { f.evidence.Models = nil },
		"missing TLS":     func(f *fakeSDK) { f.evidence.Gateway.SPKIFingerprint = "" },
		"CPU advisory":    func(f *fakeSDK) { f.evidence.Models[0].AdvisoryIDs = []string{"advisory"} },
		"outdated CPU":    func(f *fakeSDK) { f.evidence.Models[0].TCBStatus = "OutOfDate" },
		"missing GPU":     func(f *fakeSDK) { f.evidence.Models[0].GPUEvidence = "not_provided" },
		"missing key":     func(f *fakeSDK) { f.evidence.Models[0].SigningPublicKey = "" },
		"one bad candidate": func(f *fakeSDK) {
			a := f.evidence.Models[0]
			a.GPUEvidence = "not_provided"
			f.evidence.Models = append(f.evidence.Models, a)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := validSDK()
			mutate(f)
			c := &Client{sdk: f}
			body, err := c.Complete(context.Background(), json.RawMessage(`{"model":"synthetic"}`))
			if !errors.Is(err, ErrVerification) || body != nil || f.creates != 0 {
				t.Fatal("bad evidence must block inference")
			}
		})
	}
	for name, mutate := range map[string]func(*fakeSDK){
		"upstream error":    func(f *fakeSDK) { f.createErr = errors.New("synthetic-secret") },
		"receipt tampering": func(f *fakeSDK) { f.receiptErr = errors.New("synthetic-secret") },
		"gateway signature": func(f *fakeSDK) { f.receipt.SignatureKind = "gateway" },
		"missing signer":    func(f *fakeSDK) { f.receipt.Attestation = nil },
		"wrong completion":  func(f *fakeSDK) { f.receipt.ID = "another" },
		"missing ID":        func(f *fakeSDK) { f.body = json.RawMessage(`{"content":"synthetic-secret"}`) },
	} {
		t.Run(name, func(t *testing.T) {
			f := validSDK()
			mutate(f)
			c := &Client{sdk: f}
			body, err := c.Complete(context.Background(), json.RawMessage(`{"model":"synthetic"}`))
			if !errors.Is(err, ErrVerification) || body != nil || f.creates != 1 {
				t.Fatal("unverified content must not be released or inference replayed")
			}
		})
	}
	f := validSDK()
	c := &Client{sdk: f}
	if body, err := c.Complete(context.Background(), json.RawMessage(`{"model":"synthetic"}`)); err != nil || len(body) == 0 || f.creates != 1 || f.receipts != 1 {
		t.Fatal("valid receipt must release one answer")
	}
}

func TestReceiptRetryHonorsCancellation(t *testing.T) {
	f := validSDK()
	f.receiptErr = &nearai.Error{Code: "api.completion_signature_unavailable"}
	c := &Client{sdk: f}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if body, err := c.Complete(ctx, json.RawMessage(`{"model":"synthetic"}`)); err == nil || body != nil || f.creates > 1 {
		t.Fatal("cancelled receipt wait must not release or replay")
	}
}

func TestProductionOptionsKeepSecurityEnabled(t *testing.T) {
	o := options("synthetic-key", http.DefaultTransport.(*http.Transport).Clone())
	if !o.E2EE || o.DisableTLSBinding || o.SigningAlgo != nearai.Ed25519 || !o.ModelVerification.Policy.RequireGPUEvidence {
		t.Fatal("mandatory security settings missing")
	}
	for _, verification := range []nearai.VerificationOptions{o.GatewayVerification, o.ModelVerification} {
		if len(verification.Policy.AcceptedTCBStatuses) != 1 || verification.Policy.AcceptedTCBStatuses[0] != "UpToDate" || verification.QuoteVerifier == nil {
			t.Fatal("strict CPU and advisory policy missing")
		}
	}
	if o.ClientOptions.BaseURL != nearai.DefaultBaseURL || o.AttestationTTL == nil || *o.AttestationTTL <= 0 || o.ResponseTTL == nil || *o.ResponseTTL <= 0 {
		t.Fatal("routing or cache safety settings missing")
	}
}
