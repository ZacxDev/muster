package connectors

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// A VIRTUAL CONNECTOR is one whose credential is not a stored secret but is
// produced per request by something muster holds — today the GitHub App
// (internal/githubapp), which mints a short-lived installation token limited to
// the calling agent's own repository.
//
// 🔴 IT IS PRESENTED THROUGH THE SAME [Store] AS EVERY OTHER CONNECTOR, AND THAT
// IS THE POINT. Every reader that asks "does connector X exist" — the agent's
// GET /agent/connectors/{name} (which ccd's git routing and clone retry read),
// the operator's request card (Approve vs "Enter credential…"), the approve
// route's existence check, the Connectors tab list — goes through
// GetConnector/ListConnectors. Wrapping the store ([WithVirtual]) answers all of
// them in one place, so a request filed BEFORE the virtual connector existed
// becomes approvable the moment it does, with nothing re-filed and no row edited.
// The grant flow is untouched: a grant names its connector BY NAME
// (connector_grants.connector_name, not a foreign key), and the owner still
// approves each agent's request.
//
// 🔴 A VIRTUAL NAME SHADOWS A STORED ROW OF THE SAME NAME. Configuring the
// GitHub App makes it muster's GitHub identity, so a `github` connector an owner
// created earlier (a personal access token) is no longer listed or used, and it
// cannot be created, rotated or deleted while the App is configured. Its row is
// not touched: unconfiguring the App brings it back as it was.

// Virtual is a connector whose credential is minted per request.
type Virtual interface {
	// Describe is the connector's public description. Its Name is the name it
	// answers to; its Hosts are the allowlist the proxy enforces BEFORE asking for
	// a credential.
	Describe() Connector
	// Credential returns the secret to inject for caller's request to host/rest
	// (rest as the proxy received it: escaped, no leading slash). host is already
	// on Describe().Hosts. A request the connector refuses to serve for this
	// caller (another repository than the agent's, say) is [ErrTargetRefused];
	// any other error means no credential could be produced, and its text is
	// shown to the agent and the operator, so it must never carry a secret.
	Credential(ctx context.Context, caller Caller, host, rest string) (string, error)
	// Problem is the operator-facing reason this connector cannot currently serve
	// agentID (0: any agent), or "" when nothing is known to be wrong.
	Problem(agentID int64) string
}

// ErrTargetRefused is a virtual connector's refusal of one request's target.
var ErrTargetRefused = errors.New("connectors: refused for this agent")

// ErrManaged refuses an operator write to a virtual connector's name.
var ErrManaged = fmt.Errorf("%w: this connector is provided by the server's configuration", ErrInvalid)

// WithVirtual returns s with v presented as a connector named v.Describe().Name.
// A nil v returns s unchanged.
func WithVirtual(s Store, v Virtual) Store {
	if v == nil {
		return s
	}
	return &virtualStore{Store: s, v: v, name: v.Describe().Name}
}

// VirtualOf returns the virtual connector s presents under name, if any. The
// proxy uses it to take the credential from the connector rather than from the
// sealed-secret table; the UI uses it to mark the connector as managed.
func VirtualOf(s Store, name string) (Virtual, bool) {
	vs, ok := s.(*virtualStore)
	if !ok || vs.name != name {
		return nil, false
	}
	return vs.v, true
}

type virtualStore struct {
	Store
	v    Virtual
	name string
}

func (s *virtualStore) ListConnectors(ctx context.Context) ([]Connector, error) {
	list, err := s.Store.ListConnectors(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Connector, 0, len(list)+1)
	for _, c := range list {
		if c.Name != s.name {
			out = append(out, c)
		}
	}
	out = append(out, s.v.Describe())
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *virtualStore) GetConnector(ctx context.Context, name string) (Connector, error) {
	if name == s.name {
		return s.v.Describe(), nil
	}
	return s.Store.GetConnector(ctx, name)
}

func (s *virtualStore) managed() error {
	return fmt.Errorf("%w (%s): it cannot be created, rotated or deleted here — unset the configuration to "+
		"manage a stored connector of that name again", ErrManaged, s.name)
}

func (s *virtualStore) CreateConnector(ctx context.Context, nc NewConnector) (Connector, error) {
	if nc.Name == s.name {
		return Connector{}, s.managed()
	}
	return s.Store.CreateConnector(ctx, nc)
}

func (s *virtualStore) RotateSecret(ctx context.Context, name, secret string) error {
	if name == s.name {
		return s.managed()
	}
	return s.Store.RotateSecret(ctx, name, secret)
}

func (s *virtualStore) DeleteConnector(ctx context.Context, name string) error {
	if name == s.name {
		return s.managed()
	}
	return s.Store.DeleteConnector(ctx, name)
}

// Credential refuses the virtual name: its secret is per request, and only the
// proxy (through VirtualOf) may ask for it.
func (s *virtualStore) Credential(ctx context.Context, name string) (Connector, string, error) {
	if name == s.name {
		return Connector{}, "", fmt.Errorf("connectors: %s is minted per request, not stored", s.name)
	}
	return s.Store.Credential(ctx, name)
}

func (s *virtualStore) TouchConnector(ctx context.Context, name string) error {
	if name == s.name {
		return nil
	}
	return s.Store.TouchConnector(ctx, name)
}

// VirtualCreatedAt is a stable "created" time for a virtual connector's
// description: when this process started.
var VirtualCreatedAt = time.Now()
