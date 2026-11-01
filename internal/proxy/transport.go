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

const terminalErrorTimeout = time.Second

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
	controller    *http.ResponseController
	ctx           context.Context // includes the optional inference deadline
	lifetime      context.Context // original caller/transport cancellation remains binding
	cancel        context.CancelFunc
	timeout       time.Duration
	mu            sync.Mutex // serialize deadline renewal with cancellation
	writing       bool
	failed        bool
	errorDeadline time.Time // one nonrenewable budget, only for terminal errors
}

func boundResponseWrites(w http.ResponseWriter, ctx, lifetime context.Context, cancel context.CancelFunc, timeout time.Duration) (*boundedResponseWriter, func()) {
	writer := &boundedResponseWriter{ResponseWriter: w, controller: http.NewResponseController(w), ctx: ctx, lifetime: lifetime, cancel: cancel, timeout: timeout}
	stopInference := writer.watchCancellation(ctx, false)
	stopRequest := writer.watchCancellation(lifetime, true)
	return writer, func() {
		// A callback must finish before net/http can reuse the connection for
		// another request; otherwise it could expire that request's writes.
		stopInference()
		stopRequest()
		// net/http may flush buffered JSON or chunk terminators after the
		// handler returns. Leave that final transport write bounded as well.
		_ = writer.prepareWrite()
	}
}

func (w *boundedResponseWriter) watchCancellation(ctx context.Context, includeTerminal bool) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		w.mu.Lock()
		// Do not kill an idle HTTP/2 stream when inference expires: it still
		// needs to deliver its safe error. A blocked write must be interrupted.
		if w.writing && (includeTerminal || w.errorDeadline.IsZero()) {
			w.failed = true
			_ = w.controller.SetWriteDeadline(time.Now())
		}
		w.mu.Unlock()
		close(done)
	})
	return func() {
		if !stop() {
			<-done
		}
	}
}

// Only the fixed JSON/SSE error paths can opt in. Never reopen a failed transport
// or ignore caller cancellation, and never renew the terminal delivery budget.
func beginTerminalError(w http.ResponseWriter) {
	writer, ok := w.(*boundedResponseWriter)
	if !ok {
		return
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.lifetime.Err() == nil && !writer.failed && writer.errorDeadline.IsZero() {
		writer.errorDeadline = time.Now().Add(min(writer.timeout, terminalErrorTimeout))
	}
}

func (w *boundedResponseWriter) prepareWrite() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	deadline := time.Now().Add(w.timeout)
	if w.failed || w.lifetime.Err() != nil {
		deadline = time.Now()
	} else if !w.errorDeadline.IsZero() {
		deadline = w.errorDeadline
	} else if w.ctx.Err() != nil {
		w.failed = true
		deadline = time.Now()
	} else if requestDeadline, ok := w.ctx.Deadline(); ok && requestDeadline.Before(deadline) {
		deadline = requestDeadline
	}
	if callerDeadline, ok := w.lifetime.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	w.writing = true
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
	w.mu.Lock()
	w.writing = false
	if err != nil {
		w.failed = true
		w.mu.Unlock()
		w.cancel()
		return
	}
	defer w.mu.Unlock()
	if w.ctx.Err() == nil && w.errorDeadline.IsZero() && !w.failed {
		// HTTP/2 expires the stream when this timer fires, even between writes.
		// Clear it during generation gaps; the next write gets a fresh bound.
		_ = w.controller.SetWriteDeadline(time.Time{})
	}
}

func (w *boundedResponseWriter) WriteHeader(status int) {
	if err := w.prepareWrite(); err != nil {
		w.finishWrite(err)
		return
	}
	w.ResponseWriter.WriteHeader(status)
	w.finishWrite(nil)
}

func (w *boundedResponseWriter) Write(data []byte) (int, error) {
	if err := w.prepareWrite(); err != nil {
		w.finishWrite(err)
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
