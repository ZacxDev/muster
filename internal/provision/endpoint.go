package provision

import (
	"errors"
	"fmt"
	"strings"
	"text/template"
)

// ErrNoEndpoint — the instance exists but declares no address a caller could
// reach it at. Distinct from ErrNotFound (the instance is gone) and from
// ErrBlind (the driver cannot see), because "running but not reachable" is a
// real, diagnosable state and collapsing it into either of the others hides it.
var ErrNoEndpoint = errors.New("provision: instance declares no reachable endpoint")

// EndpointTemplate renders an instance's HOST from its identity.
//
// 🔴 THIS TYPE IS THE OVERRIDE THAT DID NOT EXIST. The address of an agent in
// the project muster was extracted from was one hardcoded format string, in one
// file, reachable from nowhere else — no environment variable, no config field,
// no per-instance override. Running an agent anywhere but that project's own
// cluster was therefore not expressible, and the single most valuable thing
// this package does is make that a parameter.
//
// The template is text/template over [EndpointVars]. A driver parses one at
// construction, so a bad template is a startup failure naming itself rather
// than a per-request error.
type EndpointTemplate struct {
	raw string
	t   *template.Template
}

// EndpointVars is what an endpoint template may reference.
type EndpointVars struct {
	// Name is the instance name.
	Name string
	// Group is the driver's scoping unit — a namespace, a project. Empty where
	// the driver has none.
	Group string
	// ID is the correlation id, which may be zero. A template that depends on
	// it is a template that breaks for every caller who does not set one.
	ID int64
}

// ParseEndpointTemplate compiles raw. An empty raw yields a zero
// EndpointTemplate, which ResolveEndpoint treats as "the driver has no template
// and must supply a host another way".
//
// `missingkey=error` is set so a template naming a field that does not exist
// fails loudly instead of rendering the string "<no value>" into a hostname —
// which would produce an address that resolves to nothing and an error message
// about DNS rather than about the template.
func ParseEndpointTemplate(raw string) (EndpointTemplate, error) {
	if strings.TrimSpace(raw) == "" {
		return EndpointTemplate{}, nil
	}
	t, err := template.New("endpoint").Option("missingkey=error").Parse(raw)
	if err != nil {
		return EndpointTemplate{}, fmt.Errorf("parse endpoint template %q: %w", raw, err)
	}
	return EndpointTemplate{raw: raw, t: t}, nil
}

// IsZero reports whether no template was configured.
func (e EndpointTemplate) IsZero() bool { return e.t == nil }

// String returns the template source, for logs and for an operator asking what
// a driver is configured with.
func (e EndpointTemplate) String() string { return e.raw }

// Host renders the template.
func (e EndpointTemplate) Host(v EndpointVars) (string, error) {
	if e.t == nil {
		return "", nil
	}
	var sb strings.Builder
	if err := e.t.Execute(&sb, v); err != nil {
		return "", fmt.Errorf("render endpoint template %q: %w", e.raw, err)
	}
	host := strings.TrimSpace(sb.String())
	if host == "" {
		return "", fmt.Errorf("endpoint template %q rendered empty for %q", e.raw, v.Name)
	}
	return host, nil
}

// ResolveEndpoint is the three-layer precedence every driver uses, stated once.
//
//  1. override — the per-instance Spec.Endpoint, recorded at create time. Wins
//     outright.
//  2. tmpl — the driver's configured template.
//  3. neither — ErrNoEndpoint.
//
// port and scheme are the driver's answers for the layers the override does not
// supply; an override that omits them inherits them rather than being rejected,
// because "reach it at this host instead" is a much more common intent than
// "reach it at this host, on a port I will now restate".
//
// ⚠ ONE RULE, ONE PLACE. This precedence open-coded per driver is a predicate
// that will be subtly different in each of them, and the difference only shows
// up for whoever set the override.
func ResolveEndpoint(override *Endpoint, tmpl EndpointTemplate, vars EndpointVars, port int, scheme string) (Endpoint, error) {
	if scheme == "" {
		scheme = "http"
	}
	if override != nil && !override.IsZero() {
		out := *override
		if out.Scheme == "" {
			out.Scheme = scheme
		}
		if out.Port == 0 {
			out.Port = port
		}
		if out.Port == 0 {
			return Endpoint{}, fmt.Errorf("%w: %q has an endpoint override with no port, and the instance declares none", ErrNoEndpoint, vars.Name)
		}
		return out, nil
	}
	if tmpl.IsZero() {
		return Endpoint{}, fmt.Errorf("%w: %q has no endpoint override and its driver has no endpoint template configured", ErrNoEndpoint, vars.Name)
	}
	if port == 0 {
		return Endpoint{}, fmt.Errorf("%w: %q declares no port", ErrNoEndpoint, vars.Name)
	}
	host, err := tmpl.Host(vars)
	if err != nil {
		return Endpoint{}, err
	}
	return Endpoint{Scheme: scheme, Host: host, Port: port}, nil
}
