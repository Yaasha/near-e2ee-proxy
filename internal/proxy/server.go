package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"near-e2ee-proxy/internal/limits"
)

// Defaults used by offline/live harnesses; active handlers use config.MaxBodyBytes.
const MaxRequestBytes = limits.DefaultBodyBytes
const MaxResponseBytes = limits.DefaultBodyBytes

// Backend is small so HTTP behavior can be tested offline. The production
// implementation verifies receipts before completion. Stream callbacks carry
// provisional decrypted deltas; this layer withholds tools and terminal events.
type Backend interface {
	Check(context.Context, string) error
	Complete(context.Context, json.RawMessage) (json.RawMessage, error)
	Stream(context.Context, json.RawMessage, func(json.RawMessage) error) error
}

type Server struct {
	backend        Backend
	keyHash        [32]byte
	keyConfigured  bool
	models         []string
	maxBodyBytes   int64
	requestTimeout time.Duration
	slots          chan struct{}
}

func NewServer(config Config, backend Backend) *Server {
	return &Server{
		backend: backend, keyHash: sha256.Sum256([]byte(config.ProxyKey)),
		keyConfigured: len(config.ProxyKey) >= 32 && len(config.ProxyKey) <= 256,
		models:        append([]string(nil), config.Models...), slots: make(chan struct{}, 4),
		maxBodyBytes: limits.BodyBytes(config.MaxBodyBytes), requestTimeout: config.RequestTimeout,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, 401, "authentication_error", "invalid_api_key", "A valid proxy bearer token is required.")
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, 400, "invalid_request_error", "unsupported_query", "Query parameters are unsupported.")
		return
	}
	for _, header := range []string{"X-Signing-Algo", "X-Client-Pub-Key", "X-Model-Pub-Key", "X-Encryption-Version", "X-Encrypt-All-Fields", "X-No-Aliasing", "Api-Key"} {
		if len(r.Header.Values(header)) > 0 {
			writeError(w, 400, "invalid_request_error", "unsupported_header", "Upstream encryption and routing headers are controlled by the proxy.")
			return
		}
	}
	switch {
	case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
	case (r.URL.Path == "/v1/models" || r.URL.Path == "/readyz") && r.Method == http.MethodGet:
	default:
		writeError(w, 404, "invalid_request_error", "unsupported_endpoint", "This endpoint or method is unsupported.")
		return
	}
	// A bounded, non-waiting semaphore prevents requests accumulating in memory.
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, 429, "rate_limit_error", "overloaded", "The proxy is at its concurrency limit.")
		return
	}
	ctx := r.Context()
	timeout := s.requestTimeout
	if r.Method == http.MethodGet {
		// Readiness is bounded independently of potentially long inference.
		timeout = 3 * time.Minute
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if r.Method == http.MethodGet {
		models := make([]any, 0, len(s.models))
		for _, model := range s.models {
			if s.backend.Check(ctx, model) != nil {
				writeError(w, 503, "server_error", "verification_failed", "Model admission is currently unavailable.")
				return
			}
			models = append(models, map[string]any{"id": model, "object": "model", "created": 0, "owned_by": "near"})
		}
		if r.URL.Path == "/readyz" {
			writeJSON(w, 200, map[string]string{"status": "ready"})
			return
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": models})
		return
	}
	s.chat(ctx, w, r)
}

func (s *Server) authorized(r *http.Request) bool {
	if !s.keyConfigured {
		return false
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return false
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if len(token) > 256 {
		return false
	}
	actual := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(actual[:], s.keyHash[:]) == 1
}

func (s *Server) chat(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	contentType, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" || (parameters["charset"] != "" && !strings.EqualFold(parameters["charset"], "utf-8")) || (r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity") {
		writeError(w, 415, "invalid_request_error", "unsupported_media_type", "Use uncompressed application/json with UTF-8 text.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	defer r.Body.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = http.NewResponseController(w).SetReadDeadline(deadline)
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var size *http.MaxBytesError
		if errors.As(err, &size) {
			writeError(w, 413, "invalid_request_error", "request_too_large", "The request exceeds the configured body size limit.")
		} else {
			writeError(w, 400, "invalid_request_error", "invalid_body", "Cannot read the request body.")
		}
		return
	}
	request, err := parseRequest(raw)
	if err != nil {
		message, param := "Invalid request, unsupported field, or invalid model ID; see the compatibility documentation.", ""
		var field *requestFieldError
		if errors.As(err, &field) {
			message, param = field.message, field.param
		}
		writeErrorParam(w, 400, "invalid_request_error", "unsupported_request", message, param)
		return
	}
	stream, includeUsage := request.Stream, request.StreamOptions != nil && request.StreamOptions.IncludeUsage
	if !stream {
		request.StreamOptions = nil
	}
	upstream, err := json.Marshal(request)
	if err != nil {
		writeError(w, 400, "invalid_request_error", "invalid_request", "Cannot encode this request.")
		return
	}
	if stream {
		s.stream(ctx, w, upstream, request.Model, includeUsage)
		return
	}
	answer, err := s.backend.Complete(ctx, upstream)
	if err != nil || int64(len(answer)) > s.maxBodyBytes {
		if ctx.Err() != nil {
			writeError(w, 504, "server_error", "request_timeout", "The inference deadline expired or the request was cancelled.")
		} else {
			writeError(w, 502, "server_error", "verification_failed", "Confidential inference did not complete with a verified model response.")
		}
		return
	}
	completion, err := parseCompletion(answer, request.Model)
	if err != nil {
		writeError(w, 502, "server_error", "invalid_upstream_response", "The verified response has an unsupported shape.")
		return
	}
	writeJSON(w, 200, completion)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, kind, code, message string) {
	writeErrorParam(w, status, kind, code, message, "")
}

func writeErrorParam(w http.ResponseWriter, status int, kind, code, message, param string) {
	var parameter any
	if param != "" {
		parameter = param
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": kind, "param": parameter, "code": code}})
}
