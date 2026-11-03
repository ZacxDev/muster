package clankmailplugin

// peers.go — the pinned peers (CLANKMAIL_PEERS) and the live check of each
// one's identity chain.
//
// THE PIN IS THE TRUST ANCHOR. A peer is configured as `<owner>/<name>=k_…`:
// the operator learned that chief key out of band. The hub is trusted for
// nothing here. What the live check adds is that the pinned key is STILL the
// one `@<owner>/<name>` names: the handle `<owner>` (signed by some principal
// P, for this hub), a grant by P naming the pinned key under `<name>` (for this
// hub, unexpired), and the pinned key's own enroll of that grant. Each object
// passes the §4 pipeline here; nothing the hub says about them is believed.
//
// TOFU on the principal (SPEC §6.1): the first P that verifies is recorded in
// the state. A later chain under a DIFFERENT principal — the hub serving
// another key's handle record, or the name re-claimed — disables the peer
// until a human accepts the change (`muster-clankmail peer verify --accept-principal`).
//
// A peer whose chain does not verify is DISABLED: its key is removed from the
// hub allow list, and its messages are not delivered. Disabled is sticky: the
// periodic check does not re-enable it; only `peer verify` (a human) does.

import (
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

// PeerConf is one configured peer.
type PeerConf struct {
	Label string // "<owner>/<name>": the sender muster is told
	Owner string
	Name  string
	Key   string // k_…: the pinned chief key
}

var (
	// reOwner is the ClankMail handle grammar (SPEC §6.1), which also fits
	// muster's agent-mail owner grammar (letters, digits, hyphens, ≤39).
	reOwner = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}$`)
	// reName is a grant name (SPEC §6.2: ^[a-z0-9][a-z0-9-]{0,31}$) narrowed
	// to muster's agent-mail handle grammar (a letter first, 2–32 characters),
	// so the label `<owner>/<name>` is one muster's allowlists can name.
	reName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// validKeyID applies the §2 canonical decoding rule to a k_ id.
func validKeyID(s string) bool {
	rest, ok := strings.CutPrefix(s, "k_")
	if !ok {
		return false
	}
	b, err := b32.DecodeString(strings.ToUpper(rest))
	return err == nil && len(b) == 32 && clankmail.B32(b) == rest
}

// validObjectID is validKeyID for an m_ id.
func validObjectID(s string) bool {
	rest, ok := strings.CutPrefix(s, "m_")
	if !ok {
		return false
	}
	b, err := b32.DecodeString(strings.ToUpper(rest))
	return err == nil && len(b) == 32 && clankmail.B32(b) == rest
}

// ParsePeers reads CLANKMAIL_PEERS: comma-separated `<owner>/<name>=k_…`.
// self is our own key, which may not be a peer.
func ParsePeers(s, self string) ([]PeerConf, error) {
	var out []PeerConf
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	labels, keys := map[string]bool{}, map[string]bool{}
	for _, raw := range strings.Split(s, ",") {
		e := strings.TrimSpace(raw)
		label, key, ok := strings.Cut(e, "=")
		owner, name, ok2 := strings.Cut(label, "/")
		switch {
		case !ok || !ok2:
			return nil, fmt.Errorf("CLANKMAIL_PEERS: %q is not <owner>/<name>=k_…", e)
		case !reOwner.MatchString(owner):
			return nil, fmt.Errorf("CLANKMAIL_PEERS: owner %q is not a ClankMail handle", owner)
		case !reName.MatchString(name):
			return nil, fmt.Errorf("CLANKMAIL_PEERS: name %q must match ^[a-z][a-z0-9-]{1,31}$", name)
		case !validKeyID(key):
			return nil, fmt.Errorf("CLANKMAIL_PEERS: %s: %q is not a canonical k_ key id", label, key)
		case key == self:
			return nil, fmt.Errorf("CLANKMAIL_PEERS: %s is this plugin's own key", label)
		case labels[label] || keys[key]:
			return nil, fmt.Errorf("CLANKMAIL_PEERS: %s or its key is listed twice", label)
		}
		labels[label], keys[key] = true, true
		out = append(out, PeerConf{Label: label, Owner: owner, Name: name, Key: key})
	}
	return out, nil
}

// errChain is a DEFINITIVE chain failure: what the hub served does not bind
// the pinned key to the label. (A failure to ask is a transport/hub error.)
type errChain struct{ why string }

func (e errChain) Error() string { return e.why }

// ChainResult is a verified chain.
type ChainResult struct {
	Principal string // k_ of the handle's signer
	Grant     string // m_ of the live grant
	Enroll    string // m_ of the pinned key's enroll
}

// VerifyChain checks what GET /v1/handles/{owner} served (found=false: 404)
// against the pin. principal, when non-empty, is the TOFU-recorded principal.
func VerifyChain(p PeerConf, hubName string, handleTok string, agents []AgentPair, found bool, principal string, nowMS int64) (ChainResult, error) {
	if !found {
		return ChainResult{}, errChain{"the hub has no handle @" + p.Owner}
	}
	h, err := clankmail.Verify(handleTok)
	if err != nil {
		return ChainResult{}, errChain{"the handle record fails verification: " + codeOf(err)}
	}
	if h.Type != "handle" || h.Fields["name"] != p.Owner || h.Fields["hub"] != hubName {
		return ChainResult{}, errChain{"the handle record is not @" + p.Owner + " on " + hubName}
	}
	if principal != "" && h.From != principal {
		return ChainResult{}, errChain{"@" + p.Owner + " is now signed by a different principal key (" + short(h.From) + ", was " + short(principal) + ")"}
	}
	for _, a := range agents {
		g, err := clankmail.Verify(a.Grant)
		if err != nil || g.Type != "grant" || g.From != h.From || g.Fields["agent"] != p.Key ||
			g.Fields["name"] != p.Name || g.Fields["hub"] != hubName {
			continue
		}
		if exp, ok := g.Fields["exp"].(int64); ok && exp <= nowMS {
			continue
		}
		e, err := clankmail.Verify(a.Enroll)
		if err != nil || e.Type != "enroll" || e.From != p.Key || e.Fields["grant"] != g.ID {
			continue
		}
		return ChainResult{Principal: h.From, Grant: g.ID, Enroll: e.ID}, nil
	}
	return ChainResult{}, errChain{"no live grant+enroll binds the pinned key to @" + p.Label}
}

func codeOf(err error) string {
	var ce *clankmail.Error
	if errors.As(err, &ce) {
		return ce.Code
	}
	return "invalid"
}

// short is a key id for a log line: k_abcd…wxyz.
func short(k string) string {
	if len(k) < 12 {
		return k
	}
	return k[:6] + "…" + k[len(k)-4:]
}
