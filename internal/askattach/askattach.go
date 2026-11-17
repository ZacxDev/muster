// Package askattach is how a file an agent names reaches an attention ask
// (docs/attention.md "Attachments and context"): the bounds every hop shares,
// the types an ask may carry, and the client that sends one workspace file to
// muster's upload store from where the file is.
//
// muster-server cannot read an agent's filesystem, so the file is sent from
// the agent's side, and the ask carries the upload's id:
//
//   - a MIRRORED Claude Code session: its host's muster-mirror (the local MCP
//     endpoint, internal/mirrorhost) takes attention_ask's `attachments`
//     paths, resolves each against the calling session's working directory,
//     sends it to POST MirrorPath with the host's mirror token, and forwards
//     the call with `attachment_ids` in their place;
//   - a DISPATCHED agent's pod: `ccd ask-attach <path>` sends it to POST
//     AgentPath with the agent's own token and prints the id, which the agent
//     passes as `attachment_ids`.
//
// 🔴 THE PATH IS CONFINED ON THE AGENT'S SIDE (internal/workspacefile), BECAUSE
// ONLY THAT SIDE SEES THE FILESYSTEM. The bytes are the other half: muster
// sniffs and caps them again on arrival, whatever sent them (Allowed, MaxBytes).
package askattach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ZacxDev/muster/internal/noredirect"
	"github.com/ZacxDev/muster/internal/workspacefile"
)

const (
	// MaxCount is how many files one ask may carry (attention.MaxAttachments
	// is this; migration 0143's CHECK holds it).
	MaxCount = 4
	// MaxBytes caps one attached file. muster's upload store may allow more
	// (MUSTER_UPLOAD_MAX_BYTES); an ask's file is held to the smaller.
	MaxBytes int64 = 10 << 20
	// MirrorPath is muster's route a mirror host sends an ask's file to, with
	// its mirror token and the calling session (X-Muster-Claude-Session).
	MirrorPath = "/mirror/ask-attachments"
	// AgentPath is muster's route a dispatched agent's pod sends one to, with
	// the agent's own token (X-Muster-Token).
	AgentPath = "/agent/ask-attachments"
	// SniffLen is how much of a file its type is read from
	// (http.DetectContentType's window).
	SniffLen = 512
)

// allowed are the types an ask may carry, as http.DetectContentType names
// them from the bytes: the four raster images a page shows inline, PDF, plain
// text (Markdown, CSV, JSON, logs and an SVG that does not start with an XML
// declaration all sniff as text), XML (an SVG that does), and ZIP (the Office
// formats are ZIP files). Only the four images are ever shown inline; every
// other type is a download (uploads.SetServeHeaders). HTML and anything
// unrecognised are refused.
var allowed = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
	"application/pdf":              true,
	"text/plain; charset=utf-8":    true,
	"text/plain; charset=utf-16be": true,
	"text/plain; charset=utf-16le": true,
	"text/xml; charset=utf-8":      true,
	"application/zip":              true,
}

// Allowed reports whether a file whose SNIFFED type is contentType may be
// attached to an ask.
func Allowed(contentType string) bool { return allowed[contentType] }

// Sniff is the type of a file whose first bytes are head.
func Sniff(head []byte) string {
	if len(head) > SniffLen {
		head = head[:SniffLen]
	}
	return http.DetectContentType(head)
}

// Refused is the sentence for a file whose sniffed type Allowed refuses.
func Refused(name, contentType string) string {
	return fmt.Sprintf("%s is %s, which an ask cannot carry (images, PDF, text, XML/SVG and ZIP only)", name, contentType)
}

// Record is muster's answer to one sent file: the upload's id, as an ask's
// attachment_ids takes it, and what it stored.
type Record struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// ErrTooLarge is a file over MaxBytes.
var ErrTooLarge = fmt.Errorf("the file is over %d MiB, the most an ask may carry", MaxBytes>>20)

// Target is where Upload sends a file: muster's base URL, the route (MirrorPath
// or AgentPath) and the headers that authenticate it.
type Target struct {
	Base   string
	Path   string
	Header http.Header
}

// Upload sends file p — resolved inside root (workspacefile.Resolve) — to t,
// and returns muster's record of it. The type is sniffed from the file's
// leading bytes and must be Allowed; the size is capped at MaxBytes before
// the upload (Stat) and while it streams. client nil is a client that follows
// no redirect (the request carries a credential).
func Upload(ctx context.Context, client *http.Client, t Target, root, p string) (Record, error) {
	real, err := workspacefile.Resolve(root, p)
	if err != nil {
		return Record{}, err
	}
	f, err := os.Open(real)
	if err != nil {
		return Record{}, fmt.Errorf("cannot read %s: %v", p, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Record{}, fmt.Errorf("cannot read %s: %v", p, err)
	}
	if st.Size() > MaxBytes {
		return Record{}, fmt.Errorf("%s: %w", p, ErrTooLarge)
	}
	if st.Size() == 0 {
		return Record{}, fmt.Errorf("%s is empty", p)
	}
	head := make([]byte, SniffLen)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return Record{}, fmt.Errorf("cannot read %s: %v", p, err)
	}
	head = head[:n]
	if ct := Sniff(head); !Allowed(ct) {
		return Record{}, errors.New(Refused(p, ct))
	}
	body := &capReader{r: io.MultiReader(bytes.NewReader(head), f), left: MaxBytes}
	u := strings.TrimRight(t.Base, "/") + t.Path + "?filename=" + url.QueryEscape(filepath.Base(real))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return Record{}, err
	}
	req.ContentLength = st.Size()
	for k, vs := range t.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := noredirect.Client(client).Do(req)
	if errors.Is(err, ErrTooLarge) {
		return Record{}, fmt.Errorf("%s: %w", p, ErrTooLarge)
	}
	if err != nil {
		return Record{}, fmt.Errorf("reaching muster: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return Record{}, fmt.Errorf("muster refused %s (%d): %s", filepath.Base(p), resp.StatusCode, e.Error)
		}
		return Record{}, fmt.Errorf("muster refused %s (%d)", filepath.Base(p), resp.StatusCode)
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil || rec.ID == "" {
		return Record{}, errors.New("muster's answer carries no upload id")
	}
	return rec, nil
}

// capReader passes at most left bytes and fails on the next one: the cap is
// held WHILE the file streams, so a file that grows after its Stat cannot
// send more.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, ErrTooLarge
	}
	return n, err
}
