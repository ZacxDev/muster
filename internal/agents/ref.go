package agents

import "github.com/ZacxDev/muster/internal/provision"

// RefOf is the one place an agent row becomes a driver reference.
//
// 🔴 IT KEYS ON Name, NOT ON Namespace, AND THE DIFFERENCE IS A SILENT ONE.
// api.Server.instanceIndex indexes live instances by provision.Instance.Ref.Name
// — [InstanceIndex]'s own doc says Group is empty for a driver with no
// namespacing concept, so a group-keyed index collapses every instance onto "".
// A reference built from the namespace would not fail: it would resolve to
// nothing, and every agent would render stopped while running perfectly.
//
// 🔴 IT LIVES HERE, NOT IN THE PACKAGE THAT FIRST NEEDED IT, BECAUSE THERE ARE
// NOW TWO CONSUMERS AND A SECOND COPY WOULD BE WRONG IN THE SAME WAY. It was an
// unexported refOf in internal/agentprovision; internal/agentgateway needs the
// same mapping to ask the driver where an agent is reachable, and a lifecycle
// call and a chat call that disagreed about an instance's identity would each
// work in isolation — the pod would start, and chat would resolve no endpoint for
// it. One function is what makes them agree by construction.
func RefOf(a Agent) provision.Ref {
	return provision.Ref{Name: a.Name, ID: a.ID}
}
