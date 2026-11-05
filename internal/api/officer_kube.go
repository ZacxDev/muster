package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/kuberead"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// officer_kube_read, BOUND TO THE SERVER (docs/officers.md "Reading a
// cluster: officer_kube_read"; internal/kuberead does the reading).
//
// The order of one call, each step refusing before the next:
//
//  1. the binding (dispatchOfficerTool): an officer instance whose pool is
//     offered the tool — the JUDGEMENT pool of an officer with `kube`
//     (officers.ToolOffered); a routine instance is refused there;
//  2. the arguments: typed only (kuberead.ParseRequest), the cluster one the
//     officer declares;
//  3. the grant: the instance's own approved grant for the kube-read
//     connector, through the proxy's CheckAccess — the same grant row,
//     reconciliation and implicit request on first use as /connect/;
//  4. the item: the instance must hold one live claim in HQ (or name which
//     of its claims, item_id), which the read is counted against;
//  5. the caps: 30 calls a minute per officer (kuberead.Limiter), and 200
//     reads per item (officers.ReadBudget, the one per-item counter);
//  6. the read (kuberead.Reader.Read): path built from the allowlist, a token
//     minted for this call, the answer shaped and capped.
//
// Every call that reaches step 5 writes ONE connector-audit row of kind `kube`
// (officer, instance, item, cluster, verb, GVR, namespace, name, selectors,
// jti, status, count, bytes, ms, and the refusal when muster refused it —
// never the answer). That row is the read audit: the API servers do not log
// reads. A call refused before it (arguments, cluster, grant, item) reaches no
// cluster and spends nothing.
// ---------------------------------------------------------------------------

// kubeRuntime is officer_kube_read's server state; nil when no declared
// officer has a `kube` block.
type kubeRuntime struct {
	reader  *kuberead.Reader
	limiter *kuberead.Limiter
	budget  *officers.ReadBudget
	// clusters are the built cluster readers' configuration, by officer id
	// then cluster name.
	clusters map[string]map[string]kuberead.Cluster
}

// newKubeRuntime builds the runtime for every officer with a `kube` block,
// validating each cluster's sealed configuration, or nil when none has one.
func newKubeRuntime(list []officers.Officer, now func() time.Time) (*kubeRuntime, error) {
	rt := &kubeRuntime{clusters: map[string]map[string]kuberead.Cluster{}}
	for _, o := range list {
		if o.Kube == nil {
			continue
		}
		byName := map[string]kuberead.Cluster{}
		for _, kc := range o.Kube.Clusters {
			c, err := kuberead.NewCluster(kc.Name, kc.Endpoint, kc.CA, kc.MinterToken())
			if err != nil {
				return nil, fmt.Errorf("officers: %s: %w", o.ID, err)
			}
			byName[kc.Name] = c
		}
		rt.clusters[o.ID] = byName
	}
	if len(rt.clusters) == 0 {
		return nil, nil
	}
	rt.reader = kuberead.NewReader(kuberead.Default())
	rt.limiter = kuberead.NewLimiter(kuberead.CallsPerMinute, time.Minute, now)
	rt.budget = officers.NewReadBudget(officers.ReadsPerItem, now)
	return rt, nil
}

// kubeReadToolDef is officer_kube_read's definition (offered by
// officers.ToolOffered: judgement instances of an officer with `kube`).
func kubeReadToolDef() agents.ToolDef {
	str := func(d string) map[string]any { return strProp(d) }
	return agents.ToolDef{Type: "function", Name: officers.ToolKubeRead,
		Description: "Read ONE thing from a Kubernetes cluster your officer declares, read-only, as your officer's own ServiceAccount " +
			"(muster reads it; you never hold a credential or name an address). Either a resource read {cluster, verb: get|list, group " +
			"(\"\" for core), version, resource, namespace?, name? (get), label_selector?, field_selector?, limit?} or a pod log " +
			"{cluster, log: {namespace, pod, container?, previous?, since_seconds? (≤86400), tail_lines? (≤2000)}}. " +
			"Secrets, exec, port-forward, proxies, ServiceAccount tokens and RBAC are never readable; watch is not offered; a list " +
			"across all namespaces needs a selector. An answer over 1 MiB or a list over 500 objects is REFUSED (narrow it), never cut. " +
			"Log lines are scrubbed of credential shapes. 30 calls a minute; 200 reads per item. Needs an approved grant for the " +
			"kube-read connector (connector_request) and a claimed item. NEVER quote a credential-shaped value you read.",
		Parameters: objSchema(map[string]any{
			"cluster":        str("The cluster's name, as your officer declares it."),
			"verb":           map[string]any{"type": "string", "enum": []string{"get", "list"}},
			"group":          str("The API group: \"\" for core (pods, configmaps, events, nodes …), \"apps\", \"batch\", …"),
			"version":        str("The API version, e.g. v1, v2."),
			"resource":       str("The plural resource, e.g. pods, deployments. No subresources: status is on the object, and a pod's log is the log form."),
			"namespace":      str("The namespace (omit for a cluster-scoped resource, or for an all-namespace list with a selector)."),
			"name":           str("The object's name (get only)."),
			"label_selector": str("A label selector (list only), e.g. app=api."),
			"field_selector": str("A field selector (list only), e.g. status.phase=Failed."),
			"limit":          intProp("At most this many objects (list only, 1-500); the answer says `more` when there are more.", 1, kuberead.MaxObjects),
			"item_id":        str("Optional: the claimed item this read is for, when you hold more than one."),
			"log": objSchema(map[string]any{
				"namespace":     str("The pod's namespace."),
				"pod":           str("The pod's name."),
				"container":     str("The container (omit for a single-container pod)."),
				"previous":      map[string]any{"type": "boolean", "description": "The previous (crashed) container's log."},
				"since_seconds": intProp("Only lines newer than this many seconds (at most 86400).", 1, kuberead.MaxSinceSeconds),
				"tail_lines":    intProp("The last this many lines (default 500, at most 2000).", 1, kuberead.MaxTailLines),
			}, "namespace", "pod"),
		}, "cluster")}
}

// kubeReadManagedNote is the Connectors tab's line for the kube-read connector.
const kubeReadManagedNote = "Provided by this server's configuration for officer_kube_read: read-only reads of the clusters an " +
	"officer's `kube` block names, run by muster-server itself as the officer's own ServiceAccount, with a 10-minute token " +
	"minted per call. It has no HTTP face and no stored secret to rotate or delete."

// kubeGrantsDescription says what approving kube-read request g grants.
func (s *Server) kubeGrantsDescription(ctx context.Context, g connectors.Grant) string {
	none := "NOTHING usable: only the judgement instances of an officer with a `kube` block may read a cluster, and this agent is not one"
	if s.ext.Agents == nil {
		return none
	}
	a, err := s.ext.Agents.Get(ctx, g.AgentID)
	if err != nil {
		return none
	}
	o, _, ok := s.officerOf(a)
	cs := o.KubeClusters()
	if !ok || len(cs) == 0 {
		return none
	}
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, c.Name+" as "+c.ServiceAccount)
	}
	return "officer_kube_read for this instance only: read-only GETs of the allowlisted resources (never secrets, exec, proxies or " +
		"tokens) and pod logs on " + strings.Join(parts, "; ") + ", run by muster-server with a token minted per call; ConfigMaps are " +
		"returned whole"
}

// officerKubeRead runs officer_kube_read for instance a of pool o.
func (s *Server) officerKubeRead(ctx context.Context, a agents.Agent, o officers.Officer, c *officers.Client, arguments string) string {
	kr := s.officerRT.kube
	if kr == nil {
		return toolErr("officer_kube_read is not configured on this server")
	}
	// item_id is the dispatcher's, not the read's: split it off before the
	// strict parse, which refuses every key it does not know.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &obj); err != nil {
		return toolErr("arguments must be a JSON object")
	}
	var itemArg officers.ItemID
	if v, ok := obj["item_id"]; ok {
		if err := json.Unmarshal(v, &itemArg); err != nil {
			return toolErr("item_id must be an item id")
		}
		delete(obj, "item_id")
	}
	rest, _ := json.Marshal(obj)
	req, err := kuberead.ParseRequest(rest)
	if err != nil {
		return toolErr("%v", err)
	}
	kc, ok := o.KubeCluster(req.Cluster)
	cluster, built := kr.clusters[o.ID][req.Cluster]
	if !ok || !built {
		names := make([]string, 0, len(o.KubeClusters()))
		for _, k := range o.KubeClusters() {
			names = append(names, k.Name)
		}
		return toolErr("cluster %q is not one of officer %s's clusters (%s)", req.Cluster, o.ID, strings.Join(names, ", "))
	}
	audit := kuberead.Audit{Cluster: req.Cluster, Verb: req.Verb, Group: req.Group, Version: req.Version, Resource: req.Resource,
		Namespace: req.Namespace, Name: req.Name, LabelSelector: req.LabelSelector, FieldSelector: req.FieldSelector}
	item := itemArg
	write := func(au kuberead.Audit) { s.auditKubeRead(ctx, a, o, item, au) }

	// The grant: this instance's own, approved by hand.
	if s.ext.Connectors == nil || s.connectProxy == nil {
		return toolErr("%s", ConnectorsUnconfigured)
	}
	if err := s.officerConnectorRefusal(a, officers.KubeConnector); err != nil {
		return toolErr("%s", err.Error())
	}
	acc, err := s.connectProxy.CheckAccess(ctx, s.callerOf(a), officers.KubeConnector, "officer_kube_read: read-only cluster reads for this officer's investigations")
	switch {
	case errors.Is(err, connectors.ErrRoleUncertain):
		return toolErr("%s", err.Error())
	case err != nil:
		s.logger.Printf("officers: %s: kube-read grant for %s: %v", o.ID, a.Name, err)
		return toolErr("the grant could not be checked")
	case !acc.Usable:
		return toolErr("this instance has no approved grant for connector %s (grant: %s): ask with connector_request{name: %q, reason}, "+
			"then wait with connector_status until it is approved", officers.KubeConnector, acc.Grant.Status, officers.KubeConnector)
	}

	// The item the read counts against: one live claim of this instance.
	held, err := s.kubeHeldItem(ctx, c, a.Name, itemArg)
	if err != nil {
		var hq *officers.HQError
		if errors.As(err, &hq) || errors.Is(err, officers.ErrHQUnavailable) {
			return officerToolResult(officers.ToolKubeRead, nil, err)
		}
		return toolErr("%v", err)
	}
	item = held

	if !kr.limiter.Allow(o.ID) {
		audit.Refusal = fmt.Sprintf("rate: over %d calls a minute", kuberead.CallsPerMinute)
		write(audit)
		return toolErr("officer %s has made %d cluster reads in the last minute, the cap: wait a minute", o.ID, kuberead.CallsPerMinute)
	}
	if _, err := kr.budget.Spend(o.ID, item); err != nil {
		audit.Refusal = "budget: " + err.Error()
		write(audit)
		return toolErr("%v", err)
	}
	ans, au, err := kr.reader.Read(ctx, cluster, kubeSA(kc), req)
	write(au)
	if err != nil {
		if !errors.Is(err, kuberead.ErrRefused) {
			s.logger.Printf("officers: %s: %s read of %s: %v", o.ID, a.Name, req.Cluster, err)
		}
		return toolErr("%v", err)
	}
	out, _ := json.Marshal(map[string]any{"cluster": req.Cluster, "path": au.Path, "jti": au.JTI, "item": item, "answer": ans})
	return string(out)
}

// kubeSA is a declared cluster's ServiceAccount, split.
func kubeSA(kc officers.KubeCluster) kuberead.ServiceAccount {
	ns, name, _ := strings.Cut(kc.ServiceAccount, "/")
	return kuberead.ServiceAccount{Namespace: ns, Name: name}
}

// kubeHeldItem is the item instance holds a LIVE claim on in HQ: the one it
// names (which must be among them), or its only one.
func (s *Server) kubeHeldItem(ctx context.Context, c *officers.Client, instance string, named officers.ItemID) (officers.ItemID, error) {
	claimed, err := c.AllItems(ctx, officers.StatusClaimed)
	if err != nil {
		return "", err
	}
	now := s.officerRT.clock()
	var held []officers.ItemID
	for _, it := range claimed {
		if it.ClaimedBy == instance && it.LeaseLive(now) {
			held = append(held, it.ID)
		}
	}
	switch {
	case named != "":
		for _, id := range held {
			if id == named {
				return id, nil
			}
		}
		return "", fmt.Errorf("you hold no live claim on item %s: a cluster read is counted against an item you hold", named)
	case len(held) == 1:
		return held[0], nil
	case len(held) == 0:
		return "", errors.New("you hold no claimed item: claim one first (officer_claim) — every cluster read is counted against the item it is for")
	}
	return "", fmt.Errorf("you hold %d items: name the one this read is for with item_id", len(held))
}

// auditKubeRead writes one connector-audit row of kind `kube`: what was read,
// by whom, for which item — never the answer (kuberead.Audit has no field for
// it).
func (s *Server) auditKubeRead(ctx context.Context, a agents.Agent, o officers.Officer, item officers.ItemID, au kuberead.Audit) {
	if s.ext.Connectors == nil {
		return
	}
	detail, _ := json.Marshal(struct {
		Officer string `json:"officer"`
		Item    string `json:"item,omitempty"`
		kuberead.Audit
	}{o.ID, string(item), au})
	method := au.Verb
	if method == "" {
		method = "read"
	}
	if err := s.ext.Connectors.Audit(context.WithoutCancel(ctx), connectors.AuditEntry{Kind: "kube", AgentID: a.ID, AgentName: a.Name,
		Connector: officers.KubeConnector, Method: method, Host: au.Cluster, Path: au.Path, Status: au.Status, Detail: string(detail)}); err != nil {
		s.logger.Printf("officers: %s: audit of %s's cluster read: %v", o.ID, a.Name, err)
	}
}
