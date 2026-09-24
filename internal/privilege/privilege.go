// Package privilege is the persistence domain for agent privilege requests: an
// agent asking for elevated access (e.g. Kubernetes) it doesn't currently hold.
// Phase 3.1 captures and surfaces the requests; Phase 3.2 adds the profiles
// registry and the live RBAC grant/apply that satisfies them.
package privilege

import (
	"context"
	"time"
)

// Request lifecycle states.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusDenied   = "denied"
)

// PolicyRule mirrors a Kubernetes RBAC PolicyRule (the subset we render). The
// applier translates these into rbacv1 rules verbatim.
type PolicyRule struct {
	APIGroups     []string `json:"apiGroups,omitempty"`
	Resources     []string `json:"resources,omitempty"`
	Verbs         []string `json:"verbs,omitempty"`
	ResourceNames []string `json:"resourceNames,omitempty"`
}

// EnvVar is a plain environment variable injected into the agent (applied at the
// agent's next dispatch, not live).
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Spec is what a privilege profile grants. RBAC rules apply live (no pod
// restart); Env/KubeconfigSecret are recorded and applied at the agent's next
// dispatch (documented in the UI).
type Spec struct {
	ClusterRules     []PolicyRule `json:"clusterRules,omitempty"`
	NamespaceRules   []PolicyRule `json:"namespaceRules,omitempty"`
	Env              []EnvVar     `json:"env,omitempty"`
	KubeconfigSecret string       `json:"kubeconfigSecret,omitempty"`
}

// HasRBAC reports whether the spec grants any Kubernetes RBAC (the part applied
// live).
func (s Spec) HasRBAC() bool {
	return len(s.ClusterRules) > 0 || len(s.NamespaceRules) > 0
}

// Profile is a reusable, named access bundle.
type Profile struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	DisplayName string    `json:"displayName"`
	Description string    `json:"description"`
	Spec        Spec      `json:"spec"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Grant is a profile granted to an agent. Spec carries the granted profile's
// spec (env + kubeconfig + RBAC) joined in at list time, so callers resolving an
// agent's effective access don't re-fetch each profile (avoids an N+1).
type Grant struct {
	ID          int64     `json:"id"`
	AgentID     int64     `json:"agentId"`
	ProfileID   int64     `json:"profileId"`
	ProfileName string    `json:"profileName"`
	Spec        Spec      `json:"spec"`
	GrantedBy   string    `json:"grantedBy"`
	GrantedAt   time.Time `json:"grantedAt"`
}

// Request is an agent's ask for an elevated privilege profile.
type Request struct {
	ID        int64      `json:"id"`
	AgentID   int64      `json:"agentId"`
	AgentName string     `json:"agentName"`
	Profile   string     `json:"profile"`
	Reason    string     `json:"reason"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"createdAt"`
	DecidedAt *time.Time `json:"decidedAt,omitempty"`
	DecidedBy string     `json:"decidedBy,omitempty"`
}

// Store is the privilege persistence behaviour: the request inbox (3.1) plus the
// profiles registry + per-agent grants (3.2).
type Store interface {
	// --- requests (3.1) ---
	// CreateRequest records a new pending privilege request.
	CreateRequest(ctx context.Context, r Request) (Request, error)
	// ListPendingRequests returns undecided requests, newest first.
	ListPendingRequests(ctx context.Context) ([]Request, error)
	// ListRequests returns all requests, newest first.
	ListRequests(ctx context.Context) ([]Request, error)
	// GetRequest returns a single request by id.
	GetRequest(ctx context.Context, id int64) (Request, error)
	// DecideRequest marks a request approved/denied with the decider's name.
	DecideRequest(ctx context.Context, id int64, status, decidedBy string) error

	// --- profiles (3.2) ---
	// CreateProfile inserts a reusable named access profile.
	CreateProfile(ctx context.Context, p Profile) (Profile, error)
	// ListProfiles returns all profiles, by name.
	ListProfiles(ctx context.Context) ([]Profile, error)
	// GetProfile / GetProfileByName fetch a single profile.
	GetProfile(ctx context.Context, id int64) (Profile, error)
	GetProfileByName(ctx context.Context, name string) (Profile, error)
	// DeleteProfile removes a profile (and, via cascade, its grants).
	DeleteProfile(ctx context.Context, id int64) error

	// --- grants (3.2) ---
	// Grant records that an agent holds a profile (idempotent on the pair).
	Grant(ctx context.Context, agentID, profileID int64, grantedBy string) (Grant, error)
	// Revoke removes a grant by agent+profile.
	Revoke(ctx context.Context, agentID, profileID int64) error
	// ListGrantsForAgent returns the profiles granted to an agent (with name).
	ListGrantsForAgent(ctx context.Context, agentID int64) ([]Grant, error)

	// --- retention ---
	// DeleteResolvedRequestsOlderThan deletes decided (non-pending) privilege
	// requests created before cutoff and returns the number removed. Pending
	// requests are never swept.
	DeleteResolvedRequestsOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}
