package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"near-e2ee-proxy/internal/proxy"
)

const transportKey = "synthetic-transport-key-with-32-characters"
const transportRequest = `{"model":"synthetic-model","messages":[{"role":"user","content":"synthetic"}]}`
const transportAnswer = `{"id":"synthetic","object":"chat.completion","created":1,"model":"synthetic-model","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`

type transportBackend struct {
	calls    atomic.Int32
	complete func(context.Context) (json.RawMessage, error)
	stream   func(context.Context, func(json.RawMessage) error) error
}

func (b *transportBackend) Check(context.Context, string) error { return nil }
func (b *transportBackend) Complete(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	b.calls.Add(1)
	return b.complete(ctx)
}
func (b *transportBackend) Stream(ctx context.Context, _ json.RawMessage, emit func(json.RawMessage) error) error {
	b.calls.Add(1)
	return b.stream(ctx, emit)
}

// net.Pipe runs the real HTTP/1 parser and response buffering while making a
// nonreading peer deterministic: writes cannot disappear into an OS send buffer.
func serveTransportPipe(t *testing.T, config proxy.Config, backend proxy.Backend) (net.Conn, <-chan struct{}, <-chan struct{}, <-chan struct{}) {
	t.Helper()
	return serveTransportPipeContext(t, context.Background(), config, backend)
}

func serveTransportPipeContext(t *testing.T, ctx context.Context, config proxy.Config, backend proxy.Backend) (net.Conn, <-chan struct{}, <-chan struct{}, <-chan struct{}) {
	t.Helper()
	client, connection := net.Pipe()
	listener := &transportListener{connections: make(chan net.Conn, 1), closed: make(chan struct{}), addr: connection.LocalAddr()}
	writeStarted := make(chan struct{})
	listener.connections <- &transportConnection{Conn: connection, writeStarted: writeStarted}
	server := newHTTPServer(config, backend)
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	handlerDone, connectionClosed, servingDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := server.Handler
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		handler.ServeHTTP(w, r)
	})
	server.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			close(connectionClosed)
		}
	}
	go func() {
		defer close(servingDone)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		_ = listener.Close()
		select {
		case <-servingDone:
		case <-time.After(2 * time.Second):
			t.Error("HTTP server did not stop")
		}
	})
	return client, handlerDone, connectionClosed, writeStarted
}

type transportConnection struct {
	net.Conn
	writeStarted chan struct{}
	once         sync.Once
}

func (c *transportConnection) Write(data []byte) (int, error) {
	c.once.Do(func() { close(c.writeStarted) })
	return c.Conn.Write(data)
}

type transportListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
	addr        net.Addr
}

func (l *transportListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.connections:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *transportListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}
func (l *transportListener) Addr() net.Addr { return l.addr }

func TestRejectedIncompleteBodiesDoNotDrain(t *testing.T) {
	for name, test := range map[string]struct {
		method, path, headers string
		status                int
	}{
		"unauthenticated": {"POST", "/v1/chat/completions", "", 401},
		"query":           {"POST", "/v1/chat/completions?unsupported=1", "Authorization: Bearer " + transportKey + "\r\n", 400},
		"route":           {"POST", "/unsupported", "Authorization: Bearer " + transportKey + "\r\n", 404},
		"media type":      {"POST", "/v1/chat/completions", "Authorization: Bearer " + transportKey + "\r\nContent-Type: text/plain\r\n", 415},
		"liveness":        {"GET", "/healthz", "", 200},
		"discovery":       {"GET", "/v1/models", "Authorization: Bearer " + transportKey + "\r\n", 200},
	} {
		for _, chunked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/chunked=%v", name, chunked), func(t *testing.T) {
				backend := &transportBackend{}
				client, _, closed, _ := serveTransportPipe(t, proxy.Config{ProxyKey: transportKey}, backend)
				_ = client.SetDeadline(time.Now().Add(time.Second))
				framing, prefix := "Content-Length: 100\r\n", "x"
				if chunked {
					framing, prefix = "Transfer-Encoding: chunked\r\n", "1\r\nx\r\n"
				}
				_, err := fmt.Fprintf(client, "%s %s HTTP/1.1\r\nHost: localhost\r\n%s%s\r\n%s", test.method, test.path, test.headers, framing, prefix)
				if err != nil {
					t.Fatal("send incomplete request")
				}
				response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: test.method})
				if err != nil {
					t.Fatal("an ignored body must not block the response")
				}
				_, err = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if err != nil || response.StatusCode != test.status || !response.Close || backend.calls.Load() != 0 {
					t.Fatal("respond and close without inference or draining the incomplete body")
				}
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("the unread body retained the connection goroutine")
				}
			})
		}
	}
}

func TestRequestTimeoutInterruptsBlockedWrites(t *testing.T) {
	for name, config := range map[string]proxy.Config{
		"request deadline": {ProxyKey: transportKey, RequestTimeout: 100 * time.Millisecond},
		"write bound":      {ProxyKey: transportKey, WriteTimeout: 100 * time.Millisecond},
	} {
		for _, test := range []struct {
			name   string
			stream bool
			size   int
		}{
			{"JSON write", false, 64 << 10},
			{"SSE write", true, 64 << 10},
			{"SSE flush", true, 1},
		} {
			t.Run(name+"/"+test.name, func(t *testing.T) {
				entered, cancelled := make(chan struct{}), make(chan struct{})
				backend := &transportBackend{}
				observe := func(ctx context.Context) {
					context.AfterFunc(ctx, func() { close(cancelled) })
					close(entered)
				}
				backend.complete = func(ctx context.Context) (json.RawMessage, error) {
					observe(ctx)
					return json.RawMessage(`{"id":"synthetic","object":"chat.completion","created":1,"model":"synthetic-model","choices":[{"index":0,"message":{"role":"assistant","content":"` + strings.Repeat("a", test.size) + `"},"finish_reason":"stop"}]}`), nil
				}
				backend.stream = func(ctx context.Context, emit func(json.RawMessage) error) error {
					observe(ctx)
					return emit(json.RawMessage(`{"id":"synthetic","object":"chat.completion.chunk","created":1,"model":"synthetic-model","choices":[{"index":0,"delta":{"content":"` + strings.Repeat("a", test.size) + `"},"finish_reason":null}]}`))
				}
				client, returned, _, _ := serveTransportPipe(t, config, backend)
				input := transportRequest
				if test.stream {
					input = strings.TrimSuffix(input, "}") + `,"stream":true}`
				}
				_ = client.SetWriteDeadline(time.Now().Add(time.Second))
				if _, err := fmt.Fprintf(client, "POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", transportKey, len(input), input); err != nil {
					t.Fatal("send authenticated request")
				}
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("inference did not start")
				}
				select {
				case <-cancelled:
				case <-time.After(time.Second):
					t.Fatal("the blocked transport write did not cancel the request")
				}
				// Deliberately never read the response. Context cancellation must
				// interrupt the network operation and return the handler's slot.
				select {
				case <-returned:
				case <-time.After(time.Second):
					t.Fatal("the blocked response retained its inference slot after cancellation")
				}
			})
		}
	}
}

func TestAuthorizedUploadOutlivesWriteTimeout(t *testing.T) {
	const writeTimeout = 100 * time.Millisecond
	backend := &transportBackend{complete: func(ctx context.Context) (json.RawMessage, error) {
		if _, ok := ctx.Deadline(); ok {
			t.Error("a write timeout must not impose an upload deadline")
		}
		return json.RawMessage(transportAnswer), nil
	}}
	client, _, _, _ := serveTransportPipe(t, proxy.Config{ProxyKey: transportKey, WriteTimeout: writeTimeout}, backend)
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	half := len(transportRequest) / 2
	if _, err := fmt.Fprintf(client, "POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", transportKey, len(transportRequest), transportRequest[:half]); err != nil {
		t.Fatal("start authenticated upload")
	}
	timer := time.NewTimer(3 * writeTimeout)
	defer timer.Stop()
	<-timer.C
	if _, err := io.WriteString(client, transportRequest[half:]); err != nil {
		t.Fatal("finish authenticated upload after a generation/write pause")
	}
	response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: "POST"})
	if err != nil {
		t.Fatal("read the authorized upload response")
	}
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, response.Body)
	if err != nil || response.StatusCode != 200 || response.Close || backend.calls.Load() != 1 {
		t.Fatal("accepted uploads must clear rejection deadlines and preserve keep-alive")
	}
}

func TestCallerCancellationInterruptsBlockedWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := &transportBackend{stream: func(_ context.Context, emit func(json.RawMessage) error) error {
		return emit(json.RawMessage(`{"id":"synthetic","object":"chat.completion.chunk","created":1,"model":"synthetic-model","choices":[{"index":0,"delta":{"content":"` + strings.Repeat("a", 64<<10) + `"},"finish_reason":null}]}`))
	}}
	client, returned, _, writing := serveTransportPipeContext(t, ctx, proxy.Config{ProxyKey: transportKey}, backend)
	input := strings.TrimSuffix(transportRequest, "}") + `,"stream":true}`
	_ = client.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := fmt.Fprintf(client, "POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", transportKey, len(input), input); err != nil {
		t.Fatal("send authenticated request")
	}
	select {
	case <-writing:
	case <-time.After(time.Second):
		t.Fatal("the response write did not start")
	}
	cancel()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("caller cancellation must interrupt a write without a request deadline")
	}
}

func TestFinalBufferedWriteIsBounded(t *testing.T) {
	backend := &transportBackend{complete: func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(transportAnswer), nil
	}}
	client, returned, closed, writing := serveTransportPipe(t, proxy.Config{ProxyKey: transportKey, WriteTimeout: 100 * time.Millisecond}, backend)
	_ = client.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := fmt.Fprintf(client, "POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", transportKey, len(transportRequest), transportRequest); err != nil {
		t.Fatal("send authenticated request")
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("the small JSON response should be buffered until the handler returns")
	}
	select {
	case <-writing:
	case <-time.After(time.Second):
		t.Fatal("the final HTTP flush did not start")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("a nonreader must not retain the connection during net/http's final flush")
	}
}

func TestWriteTimeoutDoesNotLimitGeneration(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%v", http2), func(t *testing.T) {
			const writeTimeout = 200 * time.Millisecond
			backend := &transportBackend{stream: func(ctx context.Context, emit func(json.RawMessage) error) error {
				if _, ok := ctx.Deadline(); ok {
					t.Error("a write timeout must not impose an inference deadline")
				}
				for i := 0; i < 2; i++ {
					if err := emit(json.RawMessage(`{"id":"synthetic","object":"chat.completion.chunk","created":1,"model":"synthetic-model","choices":[{"index":0,"delta":{"content":"preview"},"finish_reason":null}]}`)); err != nil {
						return err
					}
					if i == 0 {
						timer := time.NewTimer(3 * writeTimeout)
						defer timer.Stop()
						select {
						case <-timer.C:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
				}
				return emit(json.RawMessage(`{"id":"synthetic","object":"chat.completion.chunk","created":1,"model":"synthetic-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
			}}
			server := httptest.NewUnstartedServer(nil)
			server.Config = newHTTPServer(proxy.Config{ProxyKey: transportKey, WriteTimeout: writeTimeout}, backend)
			server.EnableHTTP2 = http2
			server.StartTLS()
			defer server.Close()
			client := server.Client()
			client.Timeout = 2 * time.Second
			input := strings.TrimSuffix(transportRequest, "}") + `,"stream":true}`
			request, _ := http.NewRequest("POST", server.URL+"/v1/chat/completions", strings.NewReader(input))
			request.Header.Set("Authorization", "Bearer "+transportKey)
			request.Header.Set("Content-Type", "application/json")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal("start the long generation")
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || !strings.HasSuffix(string(body), "data: [DONE]\n\n") || strings.Count(string(body), `"content":"preview"`) != 2 || strings.Contains(string(body), "event: error") {
				t.Fatal("healthy writes must survive generation gaps longer than the write timeout")
			}
			if (response.ProtoMajor == 2) != http2 {
				t.Fatal("exercise the requested HTTP protocol")
			}
		})
	}
}
