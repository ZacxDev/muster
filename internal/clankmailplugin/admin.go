package clankmailplugin

// admin.go — the human's door: an HTTP API on a unix socket,
// $STATE_DIR/admin.sock (mode 0600), used by the `muster-clankmail` admin
// subcommands run inside the plugin's own container (`show`, `requests`,
// `peer verify`).
//
// It exists so the one process that owns state.json stays its only writer: an
// admin command never opens the state itself, it asks this process.
//
// 🔴 WHO CAN REACH IT. The socket is not on the network: it is reachable by
// whoever can run a process in the plugin's container as its user, which on
// Kubernetes means whoever holds `pods/exec` in the plugin's namespace. muster
// never calls the plugin, and agent and chief pods carry no service-account
// token, so they cannot exec into it. But `pods/exec` there is wider than "the
// human": it includes any session holding the operator's kubeconfig, and
// muster-server's own service account in the default provisioner layout
// (MUSTER_AGENT_NAMESPACE_CLUSTERROLE unset, docs/provisioner.md), whose
// ClusterRoleBinding grants `pods/exec` in every namespace. Whoever can exec
// in can also read the chief's key file (CHIEF_KEY_FILE), which is more than
// this socket gives. So the deployment, not this socket, is the boundary: run
// the plugin in a namespace of its own where only the operator holds
// `pods/exec` — which, against muster-server, takes the least-privilege
// provisioner layout.
//
// What the socket can do: read state and re-verify a peer. It cannot send
// anything. The plugin only sends replies muster's owner approved
// (replier.go); starting a new conversation will come from muster itself, as
// compose drafts the owner approves.

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
