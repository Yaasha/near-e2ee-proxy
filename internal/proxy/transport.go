package proxy

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// DefaultWriteTimeout bounds an active socket write, not generation time.
const DefaultWriteTimeout = 30 * time.Second

// closeUnreadBody prevents HTTP/1's automatic body drain from retaining a
// connection after an early response. Do not call Body.Close here: it can itself
// drain. HTTP/2 closes unread request streams without draining the connection.
func closeUnreadBody(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor == 1 && r.Body != nil && r.Body != http.NoBody {
		w.Header().Set("Connection", "close")
		_ = http.NewResponseController(w).SetReadDeadline(time.Now())
	}
}

type boundedResponseWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	ctx        context.Context
	cancel     context.CancelFunc
	timeout    time.Duration
	mu         sync.Mutex // serialize deadline renewal with cancellation
}

func boundResponseWrites(w http.ResponseWriter, ctx context.Context, cancel context.CancelFunc, timeout time.Duration) (*boundedResponseWriter, func()) {
	writer := &boundedResponseWriter{ResponseWriter: w, controller: http.NewResponseController(w), ctx: ctx, cancel: cancel, timeout: timeout}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		writer.mu.Lock()
		_ = writer.controller.SetWriteDeadline(time.Now())
		writer.mu.Unlock()
		close(done)
	})
	return writer, func() {
		// A callback must finish before net/http can reuse the connection for
		// another request; otherwise it could expire that request's writes.
		if !stop() {
			<-done
		}
		// net/http may flush buffered JSON or chunk terminators after the
		// handler returns. Leave that final transport write bounded as well.
		_ = writer.prepareWrite()
	}
}

func (w *boundedResponseWriter) prepareWrite() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	deadline := time.Now().Add(w.timeout)
	if w.ctx.Err() != nil {
		deadline = time.Now()
	} else if requestDeadline, ok := w.ctx.Deadline(); ok && requestDeadline.Before(deadline) {
		deadline = requestDeadline
	}
	err := w.controller.SetWriteDeadline(deadline)
	// In-memory ResponseRecorders have no transport deadlines. The production
	// net/http response writer implements these through ResponseController.
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func (w *boundedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *boundedResponseWriter) finishWrite(err error) {
	if err != nil {
		w.cancel()
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx.Err() == nil {
		// HTTP/2 expires the stream when this timer fires, even between writes.
		// Clear it during generation gaps; the next write gets a fresh bound.
		_ = w.controller.SetWriteDeadline(time.Time{})
	}
}

func (w *boundedResponseWriter) WriteHeader(status int) {
	if w.prepareWrite() != nil {
		w.cancel()
		return
	}
	w.ResponseWriter.WriteHeader(status)
	w.finishWrite(nil)
}

func (w *boundedResponseWriter) Write(data []byte) (int, error) {
	if err := w.prepareWrite(); err != nil {
		w.cancel()
		return 0, err
	}
	n, err := w.ResponseWriter.Write(data)
	w.finishWrite(err)
	return n, err
}

func (w *boundedResponseWriter) FlushError() error {
	err := w.prepareWrite()
	if err == nil {
		err = w.controller.Flush()
	}
	w.finishWrite(err)
	return err
}

func (w *boundedResponseWriter) Flush() { _ = w.FlushError() }
