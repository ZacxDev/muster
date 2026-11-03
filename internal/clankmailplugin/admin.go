package clankmailplugin

// admin.go — the human's door: an HTTP API on a unix socket,
// $STATE_DIR/admin.sock (mode 0600), used by the `muster-clankmail` admin
// subcommands run inside the plugin's own container (`show`, `requests`,
// `peer verify`, `send`).
//
// It exists so the one process that owns state.json stays its only writer: an
// admin command never opens the state itself, it asks this process.
//
// 🔴 HUMAN-ONLY. The socket is reachable only by someone who can exec into the
// plugin's container as its user. No muster session, chief or agent can reach
// it, and muster never calls the plugin. That is what makes `send` a human
// send: it is the interim way to START a conversation (a reply needs no
// door — it goes through muster's approval), until starting one from muster
// itself exists.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// AdminSocket is the admin socket's path under a state directory.
func AdminSocket(stateDir string) string { return filepath.Join(stateDir, "admin.sock") }

func (p *Plugin) listenAdmin() (net.Listener, error) {
	path := AdminSocket(p.cfg.StateDir)
	// A socket left by a dead process: safe to remove, since this process holds
	// the state lock and no other plugin can be serving it.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// SendRequest is POST /send.
type SendRequest struct {
	To      string `json:"to"` // a configured peer label, <owner>/<name>
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
	Bind    string `json:"bind,omitempty"` // a Claude session id: bind the new thread to it
}

// PeerVerifyRequest is POST /peer-verify.
type PeerVerifyRequest struct {
	Peer            string `json:"peer"`
	AcceptPrincipal bool   `json:"accept_principal,omitempty"`
}

func (p *Plugin) adminHandler() http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any, err error) {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
	read := func(r *http.Request, v any) error {
		b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			return err
		}
		return json.Unmarshal(b, v)
	}
	mux.HandleFunc("GET /show", func(w http.ResponseWriter, r *http.Request) { reply(w, p.Show(), nil) })
	mux.HandleFunc("GET /requests", func(w http.ResponseWriter, r *http.Request) { reply(w, p.RequestsList(), nil) })
	mux.HandleFunc("POST /send", func(w http.ResponseWriter, r *http.Request) {
		var in SendRequest
		if err := read(r, &in); err != nil {
			reply(w, nil, errors.New("the body is a SendRequest"))
			return
		}
		out, err := p.Send(r.Context(), in)
		reply(w, out, err)
	})
	mux.HandleFunc("POST /peer-verify", func(w http.ResponseWriter, r *http.Request) {
		var in PeerVerifyRequest
		if err := read(r, &in); err != nil {
			reply(w, nil, errors.New("the body is a PeerVerifyRequest"))
			return
		}
		out, err := p.VerifyPeer(r.Context(), in.Peer, in.AcceptPrincipal)
		reply(w, out, err)
	})
	return mux
}

// Show is the summary plus each record's metadata (never a body) and each
// ledger entry without its token.
func (p *Plugin) Show() map[string]any {
	out := p.Summary()
	type row struct {
		ID string `json:"id"`
		MsgRecord
	}
	var msgs []row
	var led []LedgerEntry
	p.st.view(func(s *State) {
		for id, m := range s.Messages {
			msgs = append(msgs, row{id, *m})
		}
		for _, e := range s.Ledger {
			c := *e
			c.Token = ""
			led = append(led, c)
		}
	})
	sort.Slice(msgs, func(i, j int) bool {
		return msgs[i].At > msgs[j].At || msgs[i].At == msgs[j].At && msgs[i].ID < msgs[j].ID
	})
	if len(msgs) > 50 {
		msgs = msgs[:50]
	}
	sort.Slice(led, func(i, j int) bool { return led[i].At > led[j].At })
	out["recent_messages"], out["ledger_entries"] = msgs, led
	return out
}

// RequestsList is the requests queue as recorded: id, sender, ts, length.
func (p *Plugin) RequestsList() map[string]any {
	type row struct {
		ID string `json:"id"`
		RequestRecord
	}
	var rows []row
	p.st.view(func(s *State) {
		for id, r := range s.Requests {
			rows = append(rows, row{id, *r})
		}
	})
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].TS < rows[j].TS || rows[i].TS == rows[j].TS && rows[i].ID < rows[j].ID
	})
	return map[string]any{"count": len(rows), "items": rows,
		"note": "not delivered to muster: senders nobody allowed (SPEC §7.2). Read one with `muster-clankmail requests --bodies`."}
}

// Send is the interim human-only send: a new message (no re) to a verified
// peer, through the ledger like a reply, optionally binding its thread (its own
// id: it is the root) to a mirrored Claude session.
func (p *Plugin) Send(ctx context.Context, in SendRequest) (map[string]any, error) {
	pc, ok := p.byLabel[in.To]
	if !ok {
		return nil, errors.New("no configured peer " + in.To)
	}
	if _, status, _ := p.peerFor(pc.Key); status != peerOK {
		return nil, errors.New(in.To + " is not a verified peer now (" + status + ")")
	}
	if in.Body == "" || len(in.Body) > MaxMusterBody || !utf8.ValidString(in.Body) {
		return nil, errors.New("the body must be 1 byte to 16 KiB of UTF-8")
	}
	if in.Subject != "" && (len(in.Subject) > 200 || !utf8.ValidString(in.Subject)) {
		return nil, errors.New("the subject must be at most 200 bytes of UTF-8")
	}
	if in.Bind != "" {
		if p.cfg.MusterAPIToken == "" {
			return nil, errors.New("--bind needs MUSTER_API_TOKEN_FILE (a personal or hook token for POST /api/plugin-threads)")
		}
		if strings.ContainsAny(in.Bind, " \t\r\n/") || len(in.Bind) > 200 {
			return nil, errors.New("--bind takes a Claude session id")
		}
	}
	fields := map[string]any{"to": []string{pc.Key}, "body": in.Body}
	if in.Subject != "" {
		fields["subject"] = in.Subject
	}
	tok, id, err := SignObject(p.cfg.Key, "msg", p.now().UnixMilli(), fields)
	if err != nil {
		return nil, err
	}
	key := "send:" + id
	e := &LedgerEntry{Kind: "send", Peer: pc.Label, Token: tok, ID: id, State: ledPending, Bind: in.Bind, At: p.now().Unix()}
	if err := p.st.update(func(s *State) error { s.Ledger[key] = e; return nil }); err != nil {
		return nil, err
	}
	ferr := p.finish(ctx, key)
	out := map[string]any{"id": id, "to": pc.Label}
	p.st.view(func(s *State) {
		x := s.Ledger[key]
		out["state"], out["detail"], out["bound"] = x.State, x.Detail, x.BindDone && x.Detail == ""
	})
	if ferr != nil {
		out["note"] = "outcome not known yet; the plugin resends the same bytes until it is: " + ferr.Error()
	}
	return out, nil
}

// bind reports a sent thread's root to muster (POST /api/plugin-threads). A
// refusal muster will repeat (409: no online window shows the session; 400) is
// final and recorded; anything else is retried by ResumePending.
func (p *Plugin) bind(ctx context.Context, key string, e LedgerEntry) error {
	created, err := p.muster.bindThread(ctx, p.cfg.MusterAPIToken, e.ID, e.Bind)
	var me *MusterError
	if err != nil && !(errors.As(err, &me) && (me.Status == http.StatusConflict || me.Status == http.StatusBadRequest ||
		me.Status == http.StatusNotFound)) {
		return err
	}
	detail := ""
	if err != nil {
		detail = "the thread was not bound: " + err.Error()
	}
	p.log.Info("thread bind", "id", e.ID, "created", created, "detail", detail)
	return p.st.update(func(s *State) error {
		x := s.Ledger[key]
		x.BindDone, x.Detail = true, detail
		return nil
	})
}

// AdminCall makes one call to a running plugin's admin socket.
func AdminCall(ctx context.Context, stateDir, method, path string, body any) ([]byte, error) {
	sock := AdminSocket(stateDir)
	c := &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://admin"+path, rd)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, errors.New("cannot reach the plugin's admin socket " + sock + " (is muster-clankmail running with this STATE_DIR?)")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		return nil, errors.New(e.Error)
	}
	return b, nil
}
