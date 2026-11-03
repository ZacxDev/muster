package clankmailplugin

// state.go — $STATE_DIR/state.json, the plugin's one durable file.
//
// ONE WRITER. The process holds an exclusive flock on $STATE_DIR/state.lock
// for its whole life (lock_unix.go), so a second plugin on the same volume
// refuses to start instead of racing the cursor and the ledger. Within the
// process every change goes through store.update, under one mutex: the change
// is applied to a COPY, the copy is written atomically (temp file, fsync,
// rename, directory fsync), and only then becomes the state. A failed write
// leaves the previous state in memory and on disk.
//
// WHAT IS NEVER IN IT: the text of a message from the requests queue (neither
// body nor subject), and no message body from anyone. A delivered message keeps
// only what a reply needs (its sender key, label, subject and thread) and what
// thread derivation needs (its `re` and derived root). The ledger keeps OUR
// signed replies, whole, because resending the exact bytes is the at-most-once
// rule (replier.go).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Outcomes recorded for an inbox item.
const (
	outDelivered     = "delivered"           // muster answered 2xx
	outMusterRefused = "muster_refused"      // body over 16 KiB, or muster refused the content (400/413/422)
	outNotMine       = "not_mine"            // authentic, but our key is not in `to` (misdelivered)
	outNotPeer       = "not_peer"            // authentic and ours, from a key no peer pins
	outPeerDisabled  = "peer_disabled"       // from a pinned peer whose chain did not verify
	outNotMsg        = "not_msg"             // authentic, but not a `msg`
	outHubThread     = "hub_thread_mismatch" // the hub's unsigned thread named another root
	outSent          = "sent"                // a message WE sent (reply or interim send)
)

// MsgRecord is what the plugin keeps about one m_ id. No body.
type MsgRecord struct {
	Dir     string `json:"dir"`               // "in" or "out"
	From    string `json:"from"`              // signer key
	Peer    string `json:"peer,omitempty"`    // the peer label it came from / went to
	Subject string `json:"subject,omitempty"` // delivered inbound only: what a reply repeats
	Re      string `json:"re,omitempty"`
	Root    string `json:"root,omitempty"` // derived thread root; "" when it could not be derived
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
	TS      int64  `json:"ts"` // the message's signed ts (ms)
	At      int64  `json:"at"` // when recorded (unix s)
}

// RequestRecord is one item seen in the requests queue: who and how big,
// never what it says.
type RequestRecord struct {
	From    string `json:"from"`
	Peer    string `json:"peer,omitempty"` // set when the sender is a configured peer
	TS      int64  `json:"ts"`
	BodyLen int    `json:"body_len"`
	At      int64  `json:"at"`
}

// PeerState is the live-check status of one configured peer key.
type PeerState struct {
	Label     string `json:"label"`
	Principal string `json:"principal,omitempty"` // TOFU: the first principal that verified
	Status    string `json:"status"`              // unverified | ok | disabled
	Reason    string `json:"reason,omitempty"`
	CheckedAt int64  `json:"checked_at,omitempty"`
}

const (
	peerUnverified = "unverified"
	peerOK         = "ok"
	peerDisabled   = "disabled"
)

// Ledger states.
const (
	ledPending = "pending" // signed and written; outcome at the hub not known yet
	ledSent    = "sent"    // the hub holds it (200)
	ledFailed  = "failed"  // the hub definitively does not hold it (E_STALE, a refusal)
	ledRefused = "refused" // refused locally; never signed or never sent
)

// LedgerEntry is one outbound message: a reply to a draft, or an interim send.
type LedgerEntry struct {
	Kind     string `json:"kind"` // "reply" | "send"
	DraftID  string `json:"draft_id,omitempty"`
	Message  string `json:"message,omitempty"` // reply: the m_ id answered
	BodySHA  string `json:"body_sha,omitempty"`
	Peer     string `json:"peer,omitempty"`
	Token    string `json:"token,omitempty"` // the EXACT signed bytes; every resend sends these
	ID       string `json:"id,omitempty"`    // m_ id of Token
	State    string `json:"state"`
	Detail   string `json:"detail,omitempty"`
	Reported bool   `json:"reported,omitempty"` // reply: muster accepted the result
	Bind     string `json:"bind,omitempty"`     // send: the claude session to bind the thread to
	BindDone bool   `json:"bind_done,omitempty"`
	At       int64  `json:"at"`
}

// State is state.json.
type State struct {
	V              int                       `json:"v"`
	InboxCursor    string                    `json:"inbox_cursor,omitempty"`
	RequestsCursor string                    `json:"requests_cursor,omitempty"`
	Messages       map[string]*MsgRecord     `json:"messages"`
	Ancestors      map[string]string         `json:"ancestors,omitempty"` // fetched m_ id → its re ("" = a root)
	Requests       map[string]*RequestRecord `json:"requests"`
	Rejected       int                       `json:"rejected"` // inbox/requests items that failed §4
	Peers          map[string]*PeerState     `json:"peers"`    // by key
	Allowed        map[string]int64          `json:"allowed"`  // keys WE put on the hub allow list (unix s)
	Ledger         map[string]*LedgerEntry   `json:"ledger"`   // "draft:<id>" or "send:<m_ id>"
}

func newState() State {
	return State{V: 1, Messages: map[string]*MsgRecord{}, Ancestors: map[string]string{},
		Requests: map[string]*RequestRecord{}, Peers: map[string]*PeerState{},
		Allowed: map[string]int64{}, Ledger: map[string]*LedgerEntry{}}
}

func (s *State) fill() {
	n := newState()
	if s.Messages == nil {
		s.Messages = n.Messages
	}
	if s.Ancestors == nil {
		s.Ancestors = n.Ancestors
	}
	if s.Requests == nil {
		s.Requests = n.Requests
	}
	if s.Peers == nil {
		s.Peers = n.Peers
	}
	if s.Allowed == nil {
		s.Allowed = n.Allowed
	}
	if s.Ledger == nil {
		s.Ledger = n.Ledger
	}
}

// store is the in-process owner of state.json.
type store struct {
	mu   sync.Mutex
	path string
	st   State
	now  func() time.Time
	lock *os.File
}

func openStore(dir string, now func() time.Time) (*store, error) {
	lk, err := lockDir(filepath.Join(dir, "state.lock"))
	if err != nil {
		return nil, err
	}
	s := &store{path: filepath.Join(dir, "state.json"), st: newState(), now: now, lock: lk}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		lk.Close()
		return nil, err
	}
	if err := json.Unmarshal(b, &s.st); err != nil {
		lk.Close()
		// Refuse to start rather than restart from an empty state, which would
		// re-read the whole inbox and forget the ledger.
		return nil, fmt.Errorf("%s: corrupt state file", s.path)
	}
	s.st.fill()
	return s, nil
}

func (s *store) close() error {
	if s.lock != nil {
		return s.lock.Close()
	}
	return nil
}

// view reads the state under the lock. fn must not keep references.
func (s *store) view(fn func(st *State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
}

// update applies fn to a deep copy and makes it the state only once it is
// durably written. fn's error aborts with nothing written.
func (s *store) update(fn func(st *State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(s.st)
	if err != nil {
		return err
	}
	var next State
	if err := json.Unmarshal(b, &next); err != nil {
		return err
	}
	next.fill()
	if err := fn(&next); err != nil {
		return err
	}
	if err := writeFileAtomic(s.path, next); err != nil {
		return err
	}
	s.st = next
	return nil
}

func writeFileAtomic(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op after the rename
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}
