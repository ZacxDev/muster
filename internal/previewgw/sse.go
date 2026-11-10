package previewgw

// sse.go — keeping a quiet text/event-stream response alive through the
// edge (which cuts a connection idle for ~125 s): after StreamKeepalive with
// no bytes from the app, the proxy writes an SSE comment line (":\n\n"),
// which every conforming client ignores. 🔴 ONLY AT AN EVENT BOUNDARY: after
// a blank line, or before the first byte. An app paused in the middle of an
// event gets nothing injected — a comment there would land inside the event.
// A compressed body (a Content-Encoding other than identity) is never
// wrapped: a plain-text comment inside it would corrupt it (proxy.go).

import (
	"bytes"
	"io"
	"sync"
	"time"
)

var sseComment = []byte(":\n\n")

type sseChunk struct {
	b   []byte
	err error
}

// sseBody wraps an event-stream body. A goroutine reads the app; Read hands
// its chunks on, or a comment when none came for the interval and the stream
// is at a boundary.
type sseBody struct {
	src   io.ReadCloser
	every time.Duration
	ch    chan sseChunk
	stop  chan struct{}
	once  sync.Once

	pending []byte
	tail    []byte // the last bytes passed on, enough to see a blank line
	err     error
}

func newSSEBody(src io.ReadCloser, every time.Duration) *sseBody {
	b := &sseBody{src: src, every: every, ch: make(chan sseChunk), stop: make(chan struct{}),
		tail: []byte("\n\n")} // the start of a stream is a boundary
	go b.read()
	return b
}

func (b *sseBody) read() {
	for {
		buf := make([]byte, 32<<10)
		n, err := b.src.Read(buf)
		select {
		case b.ch <- sseChunk{buf[:n], err}:
		case <-b.stop:
			return
		}
		if err != nil {
			return
		}
	}
}

// atBoundary reports whether what was passed on so far ends with a blank
// line (LF LF, CR CR, or CRLF CRLF) — where a comment line may go.
func (b *sseBody) atBoundary() bool {
	return bytes.HasSuffix(b.tail, []byte("\n\n")) || bytes.HasSuffix(b.tail, []byte("\r\r")) ||
		bytes.HasSuffix(b.tail, []byte("\r\n\r\n"))
}

func (b *sseBody) pass(p []byte) {
	b.tail = append(b.tail, p...)
	if len(b.tail) > 4 {
		b.tail = append(b.tail[:0], b.tail[len(b.tail)-4:]...)
	}
}

func (b *sseBody) Read(p []byte) (int, error) {
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pass(b.pending[:n])
		b.pending = b.pending[n:]
		return n, nil
	}
	if b.err != nil {
		return 0, b.err
	}
	t := time.NewTimer(b.every)
	defer t.Stop()
	for {
		select {
		case c := <-b.ch:
			b.err = c.err
			if len(c.b) == 0 {
				if c.err != nil {
					return 0, c.err
				}
				continue
			}
			n := copy(p, c.b)
			b.pass(c.b[:n])
			b.pending = c.b[n:]
			return n, nil
		case <-t.C:
			if b.atBoundary() && len(p) >= len(sseComment) {
				return copy(p, sseComment), nil
			}
			t.Reset(b.every)
		}
	}
}

func (b *sseBody) Close() error {
	b.once.Do(func() { close(b.stop) })
	return b.src.Close()
}
