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

	"golang.org/x/sys/unix"

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
	raw, err := Send(ctx, noredirect.Client(client), Spec{Target: t, MaxBytes: MaxBytes, Noun: filepath.Base(p),
		TooLarge: func(p string) error { return fmt.Errorf("%s: %w", p, ErrTooLarge) },
		Type: func(p string, size int64, head []byte) (string, error) {
			if size == 0 {
				return "", fmt.Errorf("%s is empty", p)
			}
			if ct := Sniff(head); !Allowed(ct) {
				return "", errors.New(Refused(p, ct))
			}
			return "application/octet-stream", nil
		}}, root, p)
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil || rec.ID == "" {
		return Record{}, errors.New("muster's answer carries no upload id")
	}
	return rec, nil
}

// THE ONE WORKSPACE-FILE UPLOAD CLIENT (Send): an ask's attachment (Upload,
// `ccd ask-attach`, muster-mirror's MCP proxy) and an officer's ask image
// (podagent.UploadAskImage, `ccd ask-image`) are each a Spec over it — the
// route, the size cap and the type rule — so the resolve, the Stat cap, the
// sniff, the streaming cap, the POST and the refusal's parsing are one code.

// Spec is one kind of workspace upload.
type Spec struct {
	Target
	// MaxBytes caps the file, before the upload (Stat) and while it streams;
	// TooLarge is the error for file p over it.
	MaxBytes int64
	TooLarge func(p string) error
	// Type checks file p (its size, its first SniffLen bytes) and answers the
	// Content-Type to send, or the refusal.
	Type func(p string, size int64, head []byte) (string, error)
	// Noun names the file in muster's refusal: "muster refused <Noun> (413): …".
	Noun string
}

// openResolved opens real — the file workspacefile.ResolveStat confined, as
// want — and returns it with its own stat, or a refusal.
//
// 🔴 THE FILE READ IS THE FILE RESOLVED. Between the resolution and this open
// the path can be swapped: O_NOFOLLOW refuses a symbolic link put in place of
// the last part (real has none of its own: every link was resolved), O_NONBLOCK
// keeps a FIFO or device put there from blocking the open, and the opened
// descriptor's own stat must be a regular file AND the same file (device and
// inode, os.SameFile) as the one confined — so a file replaced, or a directory
// above it swapped for a link elsewhere, is refused rather than read.
// (O_NONBLOCK changes nothing for a regular file's reads.)
func openResolved(real string, want os.FileInfo, p string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(real, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, nil, fmt.Errorf("%s changed into a symbolic link while it was being read; nothing was sent", p)
		}
		return nil, nil, fmt.Errorf("cannot read %s: %v", p, err)
	}
	f := os.NewFile(uintptr(fd), real)
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("cannot read %s: %v", p, err)
	}
	if !st.Mode().IsRegular() || !os.SameFile(want, st) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s changed while it was being read (it is no longer the file that was checked); nothing was sent", p)
	}
	return f, st, nil
}

// afterResolve, when set (tests only), runs between the path's resolution and
// its open: where a race would swap the file.
var afterResolve func(real string)

// errCapped is the streaming cap's failure, which Send reports as TooLarge.
var errCapped = errors.New("the file grew past its cap while it was sent")

// Send sends file p — resolved inside root (workspacefile.Resolve) — as s
// says, with client as given (the caller decides the redirect rule), and
// returns muster's 2xx answer body; a non-2xx is muster's refusal, in its own
// words when it gave some.
func Send(ctx context.Context, client *http.Client, s Spec, root, p string) ([]byte, error) {
	real, want, err := workspacefile.ResolveStat(root, p)
	if err != nil {
		return nil, err
	}
	if afterResolve != nil {
		afterResolve(real)
	}
	f, st, err := openResolved(real, want, p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st.Size() > s.MaxBytes {
		return nil, s.TooLarge(p)
	}
	head := make([]byte, SniffLen)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("cannot read %s: %v", p, err)
	}
	head = head[:n]
	ct, err := s.Type(p, st.Size(), head)
	if err != nil {
		return nil, err
	}
	body := &capReader{r: io.MultiReader(bytes.NewReader(head), f), left: s.MaxBytes}
	u := strings.TrimRight(s.Base, "/") + s.Path + "?filename=" + url.QueryEscape(filepath.Base(real))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = st.Size()
	for k, vs := range s.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", ct)
	resp, err := client.Do(req)
	if errors.Is(err, errCapped) {
		return nil, s.TooLarge(p)
	}
	if err != nil {
		return nil, fmt.Errorf("reaching muster: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("muster refused %s (%d): %s", s.Noun, resp.StatusCode, e.Error)
		}
		return nil, fmt.Errorf("muster refused %s (%d)", s.Noun, resp.StatusCode)
	}
	return raw, nil
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
		return 0, errCapped
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, errCapped
	}
	return n, err
}
