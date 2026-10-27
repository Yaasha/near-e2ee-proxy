package near_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	nearai "github.com/nearai/inference-sdk/go"
)

// These contract tests exercise the real SDK encryption/receipt implementations
// with synthetic keys. They do not claim that a test-held key belongs to a TEE.
func TestSDKExpandedEncryptionProtectsAgentFields(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	input := `{
	  "model":"synthetic-model",
	  "messages":[
	    {"role":"user","content":"private-prompt-marker"},
	    {"role":"assistant","content":"private-history-marker","reasoning_content":"private-reasoning-marker",
	     "tool_calls":[{"id":"call_1","type":"function","function":{"name":"private-tool-marker","arguments":"private-arguments-marker"}}]},
	    {"role":"tool","tool_call_id":"call_1","content":"private-result-marker"}
	  ],
	  "tools":[{"type":"function","function":{"name":"private-tool-marker","description":"private-description-marker",
	    "parameters":{"type":"object","properties":{"private-schema-marker":{"type":"string"}}}}}],
	  "tool_choice":{"type":"function","function":{"name":"private-choice-marker"}}
	}`
	prepared, wire := encryptFixture(t, input, hex.EncodeToString(public))
	for _, secret := range []string{"prompt", "history", "reasoning", "tool", "arguments", "result", "description", "schema", "choice"} {
		if bytes.Contains(wire, []byte("private-"+secret+"-marker")) {
			t.Fatalf("SDK left the %s fixture in plaintext", secret)
		}
	}
	for _, visible := range []string{"synthetic-model", "call_1", "assistant"} {
		if !bytes.Contains(wire, []byte(visible)) {
			t.Fatal("expected protocol metadata to remain visible")
		}
	}
	if prepared.Request.Header.Get("X-Encryption-Version") != "2" || prepared.Request.Header.Get("X-Encrypt-All-Fields") != "true" {
		t.Fatal("expanded v2 encryption must be enabled")
	}

	// Construct an encrypted reply using the SDK itself, addressed to the fresh
	// client key. Reusing its request encryption helper here is test setup only.
	_, encryptedReply := encryptFixture(t, `{"model":"synthetic-model","messages":[{"role":"assistant","content":"synthetic-reply"}]}`,
		prepared.Request.Header.Get("X-Client-Pub-Key"))
	var reply struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(encryptedReply, &reply); err != nil {
		t.Fatal(err)
	}
	response, _ := json.Marshal(map[string]any{"id": "synthetic-completion", "choices": []any{map[string]any{"message": reply.Messages[0]}}})
	decrypted, err := prepared.DecryptJSON(response)
	if err != nil || !bytes.Contains(decrypted, []byte("synthetic-reply")) {
		t.Fatal("SDK encrypted reply must decrypt with the corresponding client key")
	}
	if _, err := prepared.DecryptJSON([]byte(`{"choices":[{"message":{"content":"plaintext-marker"}}]}`)); err == nil {
		t.Fatal("plaintext response fields must be rejected")
	}
	if _, err := prepared.DecryptJSON([]byte(`{"choices":[{"message":{"content":"` + strings.Repeat("00", 100) + `"}}]}`)); err == nil {
		t.Fatal("unauthenticated ciphertext must be rejected")
	}
}

func TestSDKLeavesUnknownFieldsVisible(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, wire := encryptFixture(t, `{"model":"synthetic-model","messages":[],"metadata":{"private":"unhandled-secret-marker"}}`, hex.EncodeToString(public))
	if !bytes.Contains(wire, []byte("unhandled-secret-marker")) {
		t.Fatal("review changed SDK unknown-field behavior before relying on this contract")
	}
	// This demonstrates why the HTTP endpoint must recursively reject unknown
	// fields. Merely passing E2EE=true to the SDK is insufficient for a proxy.
}

func encryptFixture(t *testing.T, input, publicKey string) (*nearai.PreparedE2EEChatRequest, []byte) {
	t.Helper()
	request, err := http.NewRequest("POST", nearai.DefaultBaseURL+"/chat/completions", strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := nearai.PrepareE2EEChatRequest(request, nearai.E2EEModelKey{SigningAlgo: nearai.Ed25519, PublicKey: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Request.Body.Close()
	wire, err := io.ReadAll(prepared.Request.Body)
	if err != nil {
		t.Fatal(err)
	}
	return prepared, wire
}

func TestSDKModelReceiptBindsExactBytesAndKind(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	request := []byte(`{"model":"synthetic-model","messages":[]}`)
	response := []byte(`{"id":"synthetic-completion","choices":[]}`)
	requestHash, responseHash := sha256.Sum256(request), sha256.Sum256(response)
	text := "synthetic-model:" + hex.EncodeToString(requestHash[:]) + ":" + hex.EncodeToString(responseHash[:])
	attestation := nearai.VerifiedAttestation{Signer: nearai.SigningIdentity{SigningAlgo: nearai.Ed25519, SigningAddress: hex.EncodeToString(public)}}
	receipt := nearai.CompletionSignature{
		Kind: "provider_tee", SignedText: text, Signer: attestation.Signer,
		Signature: hex.EncodeToString(ed25519.Sign(private, []byte(text))),
	}
	if err := nearai.VerifyModelResponse(request, response, receipt, attestation); err != nil {
		t.Fatal("valid synthetic model receipt must verify")
	}
	for name, mutate := range map[string]func(*nearai.CompletionSignature){
		"gateway receipt": func(s *nearai.CompletionSignature) { s.Kind = "gateway" },
		"bad signature":   func(s *nearai.CompletionSignature) { s.Signature = strings.Repeat("00", 64) },
		"wrong signer":    func(s *nearai.CompletionSignature) { s.Signer.SigningAddress = strings.Repeat("00", 32) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := receipt
			mutate(&changed)
			if nearai.VerifyModelResponse(request, response, changed, attestation) == nil {
				t.Fatal("invalid receipt must fail")
			}
		})
	}
	if nearai.VerifyModelResponse(append(append([]byte(nil), request...), ' '), response, receipt, attestation) == nil {
		t.Fatal("reserializing or changing request bytes must invalidate the receipt")
	}
	if nearai.VerifyModelResponse(request, append(append([]byte(nil), response...), ' '), receipt, attestation) == nil {
		t.Fatal("changing response bytes must invalidate the receipt")
	}
}
