// Package k8s provisions muster instances onto a Kubernetes cluster by
// rendering its own minimal manifests.
//
// # 🔴 IT RENDERS MANIFESTS RATHER THAN INSTALLING A CHART, AND THAT IS THE
// CENTRAL DESIGN DECISION HERE
//
// The project muster was extracted from installed a vendored 1,402-line Helm
// chart. Shipping that chart was measured to be impossible: it carried a
// hardcoded third-party service URL reachable only on one private network, five
// calls to a consumer chat API's send-message endpoint, `alpine:latest` and
// `busybox:1.36` as literal images, a maintainer email address, and a Helm
// `fail` whose text asserts which CNI the author's cluster runs. Vendoring it
// unchanged would have shipped somebody's private service URL and a chat
// integration into a stranger's cluster.
//
// The second measurement is what makes rendering cheap rather than a rewrite:
// that project used roughly THIRTY of the chart's values. Everything below is
// the manifest set those thirty values actually produced —
//
//   - a Namespace, when the driver owns one per instance;
//   - a ServiceAccount, which is the identity a policy grant attaches to;
//   - a Secret for confidential environment, and a SEPARATE one for
//     confidential files — separate because the first is consumed with
//     `envFrom`, which exports every key of it as an environment variable, so a
//     file sharing that object would be in the process environment;
//   - a ConfigMap for the rest of the files;
//   - a PersistentVolumeClaim, only when a persistent workspace was asked for;
//   - a Deployment with exactly ONE container, plus an init container when the
//     spec has imperative steps;
//   - a Service, when the spec declares ports;
//   - a NetworkPolicy, only when the spec asks for network isolation and the
//     driver was configured to render one (see network.go).
//
// — and nothing else. It also removes a build-time `go:embed` of a chart
// directory from the release path, and the chart-sync tooling that went with
// it, whose known trap was that its verification step silently tested the wrong
// tree unless a sibling checkout had been synced first.
//
// # What this driver does NOT do
//
//   - 🔴 IT DOES NOT CLONE THE REPOSITORY. Spec.Repo is declared to the
//     instance as environment; the checkout is the instance runtime's job or an
//     explicit Init step. The chart this replaces cloned it about 250 lines
//     BEFORE init commands ran, which is the whole reason that project's git
//     credential handling had to work in two different environments at once.
//   - 🔴 IT DOES NOT RESTRICT EGRESS BY DNS NAME. That is the control that
//     addresses exfiltration by a prompt-injected model, and this driver does
//     not implement it. Saying so is the point: a driver that rendered an
//     address-range policy and called it egress control would report a
//     mitigation nobody has. It DOES now render an address-range policy, for a
//     spec that asks for network isolation — and what that buys is narrower and
//     is named for what it is (Capabilities.NetworkIsolation): the policy admits
//     no pod but muster's, and allows no destination in a private address range.
//     Whether that puts the cluster's API and its other namespaces out of reach
//     depends on their addresses being private, which this driver does not
//     check (network.go, nonPublicIPv4). The instance can still send anything it
//     holds to any public host on the allowed ports.
//   - 🔴 IT DOES NOT ENFORCE THE NetworkPolicy IT WRITES. The cluster's network
//     plugin does, or does not, and nothing here can tell which.
//   - It does not run sidecars. One container, plus an init container. The
//     chart it replaces could run three log tailers, which were structurally
//     invisible to that project anyway — its log reads never named a container,
//     so they always read the first one.
//   - It does not manage ingress, TLS, autoscaling or pod disruption budgets.
package k8s
