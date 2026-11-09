package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// The offer stream: what the host writes on the GET OfferPath response, one
// JSON object per line, and the Answer the gateway POSTs back.
//
// 🔴 ADDITIVE ONLY. A new field or a new Type is a change both ends can meet
// at different times: a reader ignores unknown fields, and Decoder.Next
// returns an unknown Type to its caller (who skips it) rather than failing
// the stream. Renaming or removing a field is a VersionHeader bump.

// Offer message types.
const (
	TypeConfig    = "config"    // the host's allowlist and caps (Config); first, and again on change
	TypeWindows   = "windows"   // the tmux window ids the host mirrors (Windows), on change
	TypeListeners = "listeners" // the full set of offered listeners (Listeners), on change
	TypeOpen      = "open"      // an agent's preview_open (Request)
	TypeClose     = "close"     // an agent's preview_close (Request)
)

// Offer is one line of the offer stream. Exactly the field its Type names is
// set; Windows and Listeners are the WHOLE current set (an empty one clears).
type Offer struct {
	Type      string     `json:"type"`
	Config    *Config    `json:"config,omitempty"`
	Windows   []string   `json:"windows,omitempty"`
	Listeners []Listener `json:"listeners,omitempty"`
	Request   *Request   `json:"request,omitempty"`
}

// Config is the host's previews.json as the gateway may know it. Disabled is
// non-empty, and says why, when previews are off (a missing, empty or invalid
// file); the other fields are then zero.
type Config struct {
	Ports    []string `json:"ports"`
	Offer    bool     `json:"offer"`
	MaxOpen  int      `json:"maxOpen"`
	MaxConns int      `json:"maxConns"`
	Disabled string   `json:"disabled,omitempty"`
}

// Listener is one offered loopback listener. 🔴 THESE FOUR KEYS AND NO OTHER:
// not the argv, the working directory, the environment or the pid (the
// design's privacy rule; a ledger test pins the key set). Process is the
// listener's comm, at most 15 bytes; Window is a tmux window id ("@3");
// FirstSeen is RFC 3339.
type Listener struct {
	Port      int    `json:"port"`
	Process   string `json:"process"`
	Window    string `json:"window"`
	FirstSeen string `json:"first_seen"`
}

// Request is an agent tool's open or close, carried to the gateway. ID is the
// host's request id, echoed in the gateway's Answer. Session is display-only;
// the gateway matches a preview on (owner, host, Window).
type Request struct {
	ID      string `json:"id"`
	Window  string `json:"window"`
	Session string `json:"session,omitempty"`
	Port    int    `json:"port,omitempty"`    // open
	Label   string `json:"label,omitempty"`   // open, at most 60 bytes
	Path    string `json:"path,omitempty"`    // open, starts with "/"
	Preview string `json:"preview,omitempty"` // close; on a re-announced open, the id the previous gateway gave
}

// Answer is the body of POST OpenedPath and POST ClosedPath. RequestID is
// empty for an open the owner made by clicking an offer (the gateway tells the
// host unsolicited, so the agent's preview_list includes it).
type Answer struct {
	RequestID string `json:"request_id,omitempty"`
	Preview   string `json:"preview,omitempty"`
	Window    string `json:"window,omitempty"`
	Port      int    `json:"port,omitempty"`
	Error     string `json:"error,omitempty"` // a tool error code, e.g. "max_open"; empty on success
}

// MaxLine bounds one offer-stream line, in bytes.
const MaxLine = 1 << 20

// ErrLineTooLong is Decoder.Next's error for a line over MaxLine.
var ErrLineTooLong = errors.New("wire: offer line over MaxLine")

// Encoder writes offer lines. It does not flush: an http.ResponseWriter
// caller flushes after each Encode so the gateway sees the line at once.
type Encoder struct{ w io.Writer }

func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

// Encode writes m as one line, after checking it is well formed.
func (e *Encoder) Encode(m Offer) error {
	if err := m.valid(); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b)+1 > MaxLine {
		return ErrLineTooLong
	}
	_, err = e.w.Write(append(b, '\n'))
	return err
}

// Decoder reads offer lines.
type Decoder struct{ r *bufio.Reader }

func NewDecoder(r io.Reader) *Decoder { return &Decoder{r: bufio.NewReaderSize(r, 64<<10)} }

// Next reads one line. A line of a Type this side does not know is returned
// as is (its caller skips it); a known Type whose field is missing is an
// error. Blank lines are skipped.
func (d *Decoder) Next() (Offer, error) {
	for {
		line, err := d.readLine()
		if err != nil {
			return Offer{}, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m Offer
		if err := json.Unmarshal(line, &m); err != nil {
			return Offer{}, fmt.Errorf("wire: bad offer line: %w", err)
		}
		if err := m.valid(); err != nil {
			return Offer{}, err
		}
		return m, nil
	}
}

func (d *Decoder) readLine() ([]byte, error) {
	var line []byte
	for {
		frag, err := d.r.ReadSlice('\n')
		if len(line)+len(frag) > MaxLine {
			return nil, ErrLineTooLong
		}
		line = append(line, frag...)
		if err == nil {
			return line, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(line) > 0 {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
}

func (m Offer) valid() error {
	switch m.Type {
	case TypeConfig:
		if m.Config == nil {
			return errors.New("wire: config offer without config")
		}
	case TypeOpen, TypeClose:
		if m.Request == nil || m.Request.ID == "" || m.Request.Window == "" {
			return fmt.Errorf("wire: %s offer without a request id and window", m.Type)
		}
	case TypeWindows, TypeListeners:
	case "":
		return errors.New("wire: offer without a type")
	}
	return nil
}
