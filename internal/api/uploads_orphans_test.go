package api

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/uploads"
)

// rawUpload opens a TCP connection to the test server and sends a signed-in,
// same-origin POST /uploads whose declared body is a `total`-byte file, but
// writes only `send` bytes of the file. It returns the open connection.
func (w *upWorld) rawUpload(t *testing.T, send, total int) net.Conn {
	t.Helper()
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	_, _ = mw.CreateFormFile("file", "big.bin")
	boundary := mw.Boundary()
	tail := "\r\n--" + boundary + "--\r\n"
	u, _ := url.Parse(w.ts.URL)
	c, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_, _ = fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nCookie: %s\r\nX-Requested-With: XMLHttpRequest\r\n"+
		"Content-Type: multipart/form-data; boundary=%s\r\nContent-Length: %d\r\n\r\n",
		ui.UploadPath, u.Host, w.ts.URL, sessionCookieFor(w.s), boundary, head.Len()+total+len(tail))
	_, _ = c.Write(head.Bytes())
	chunk := bytes.Repeat([]byte{0xff}, 64<<10)
	for left := send; left > 0; {
		k := min(left, len(chunk))
		if _, err := c.Write(chunk[:k]); err != nil {
			t.Fatalf("write: %v", err)
		}
		left -= k
	}
	return c
}

// entered waits until the server has taken the upload into its in-flight
// count. 🔴 settled alone would pass at once on a raw upload the handler has
// not reached yet: zero in flight BEFORE it starts reads the same as zero after
// it ended, and the store is then checked mid-write, its multipart upload
// still (rightly) open.
func (w *upWorld) entered(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		w.s.uploadLimits.mu.Lock()
		n := w.s.uploadLimits.total
		w.s.uploadLimits.mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the upload never reached the server's in-flight count in 10s")
		}
		time.Sleep(time.Millisecond)
	}
}

// settled waits until no upload is in flight. A failed write's clean-up (the
// store's abort round trip, then the service's clean-up) runs inside the
// store call, before the handler leaves the count, so zero in flight means it
// has landed; no grace period is added, so a clean-up that ever went
// asynchronous would fail here rather than be waited out.
func (w *upWorld) settled(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		w.s.uploadLimits.mu.Lock()
		n := w.s.uploadLimits.total
		w.s.uploadLimits.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d upload(s) still in flight after 10s", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAFailedUploadOrphansNoMultipartUpload: a multipart write (a file over
// one part) that fails — the client disconnects, the read deadline passes, or
// the cap overflows — leaves no incomplete multipart upload in the store and
// no object.
func TestAFailedUploadOrphansNoMultipartUpload(t *testing.T) {
	big := uploads.PartSize + uploads.PartSize/2
	t.Run("client disconnects", func(t *testing.T) {
		w := uploadServer(t, true, 100<<20)
		c := w.rawUpload(t, big, 4*uploads.PartSize)
		w.entered(t)
		time.Sleep(200 * time.Millisecond) // the server is reading the second part
		c.Close()
		w.settled(t)
		if inc := w.objs.Incomplete(); len(inc) != 0 {
			t.Errorf("incomplete multipart uploads left: %v", inc)
		}
		if keys := w.objs.Keys(); len(keys) != 0 {
			t.Errorf("objects left: %v", keys)
		}
	})
	t.Run("read deadline", func(t *testing.T) {
		w := uploadServer(t, true, 100<<20)
		w.s.uploadLimits.readTimeout = 500 * time.Millisecond
		_ = w.rawUpload(t, big, 4*uploads.PartSize) // and then nothing
		w.entered(t)
		w.settled(t)
		if inc := w.objs.Incomplete(); len(inc) != 0 {
			t.Errorf("incomplete multipart uploads left: %v", inc)
		}
		if keys := w.objs.Keys(); len(keys) != 0 {
			t.Errorf("objects left: %v", keys)
		}
	})
	t.Run("cap overflow", func(t *testing.T) {
		// One byte over a cap of two parts and a bit: the declared length is
		// inside the body limit, so the FILE cap is what trips, mid-stream.
		max := 2*uploads.PartSize + 5
		w := uploadServer(t, true, int64(max))
		resp, body := w.postFile(t, "over.bin", bytes.Repeat([]byte{0xff}, max+1), upOpt{})
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("over the cap: %d %q; want 413", resp.StatusCode, body)
		}
		w.settled(t)
		if inc := w.objs.Incomplete(); len(inc) != 0 {
			t.Errorf("incomplete multipart uploads left: %v", inc)
		}
	})
}
