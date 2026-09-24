package router

import (
	"context"

	"github.com/ZacxDev/muster/internal/api"
)

// Port adapts Client to the interfaces internal/api declares.
//
// 🔴 THE INTERFACES LIVE CONSUMER-SIDE AND THE ADAPTER LIVES HERE, WHICH IS THE
// ONLY ARRANGEMENT THAT KEEPS THE DEPENDENCY POINTING ONE WAY. internal/api
// declares what it needs (api.RouterPort, api.GatePort) and imports nothing to
// say so; this package knows about api and converts. Inverting that — api
// importing router for a concrete type — would make the domain layer depend on
// a transport, and would make api untestable without an HTTP server.
//
// ⚠ THE TYPE CONVERSIONS BELOW LOOK LIKE BOILERPLATE AND ARE NOT. Each one is
// the place a wire spelling meets a domain spelling, and each is the place a
// rename on either side becomes a compile error instead of a field that
// silently marshals to null. GateSpec is the worked example: the wire wants
// `sessionId` and the domain calls it Session, and exactly one line knows that.
type Port struct{ C *Client }

// Compile-time proof the adapter satisfies both ports. Without these, "the
// client is what the server needs" is a comment rather than a fact the compiler
// enforces — and a method renamed on either side would surface as a nil
// interface at wiring time rather than as a build failure here.
var (
	_ api.RouterPort = Port{}
	_ api.GatePort   = Port{}
)

// New builds a Port, or returns a zero Port whose C is nil when the
// configuration is incomplete. Callers should check Configured before wiring.
func NewPort(cfg Config) Port { return Port{C: New(cfg)} }

// Configured reports whether this port can reach a router.
func (p Port) Configured() bool { return p.C != nil }

// --- api.SessionLivenessProbe ---

func (p Port) SessionsExisting(ctx context.Context, ids []string) (map[string]bool, error) {
	return p.C.SessionsExisting(ctx, ids)
}

// --- api.RouterPort ---

func (p Port) PublishEvent(ctx context.Context, name, data string) error {
	return p.C.PublishEvent(ctx, name, data)
}

func (p Port) Notify(ctx context.Context, n api.RouterNotification) error {
	return p.C.Notify(ctx, Notification{
		Type:  n.Type,
		ID:    n.ID,
		Title: n.Title,
		Body:  n.Body,
		Tag:   n.Tag,
		Data:  n.Data,
	})
}

func (p Port) Directories(ctx context.Context, query string, limit int) ([]string, error) {
	return p.C.Directories(ctx, query, limit)
}

func (p Port) SessionMeta(ctx context.Context, sessionID string) (api.SessionMeta, bool, error) {
	m, ok, err := p.C.SessionMeta(ctx, sessionID)
	if err != nil || !ok {
		return api.SessionMeta{}, false, err
	}
	return api.SessionMeta{
		SessionID: m.SessionID,
		Project:   m.Project,
		Cwd:       m.Cwd,
		Host:      m.Host,
	}, true, nil
}

// --- api.GatePort ---

func (p Port) MintGate(ctx context.Context, spec api.GateSpec) (string, error) {
	return p.C.MintGate(ctx, GateSpec{
		Type:    spec.Type,
		Tool:    spec.Tool,
		Command: spec.Command,
		Host:    spec.Host,
		Project: spec.Project,
		Cwd:     spec.Cwd,
		// 🔴 `Session` HERE, `sessionId` ON THE WIRE. See GateSpec's tag.
		SessionID: spec.Session,
		Context:   spec.Context,
	})
}

func (p Port) ReadGate(ctx context.Context, id string) (api.GateDecision, error) {
	d, err := p.C.ReadGate(ctx, id)
	if err != nil {
		return api.GateDecision{}, err
	}
	return api.GateDecision{State: d.State, Response: d.Response, Comment: d.Comment}, nil
}

func (p Port) ClearGate(ctx context.Context, id string) error {
	return p.C.ClearGate(ctx, id)
}
