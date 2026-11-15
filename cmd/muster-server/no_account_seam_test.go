package main

import (
	"testing"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
)

// TestTheAgentGatewayTakesTheNoAccountRule: internal/api installs its
// no-account rule (an officer instance on the LLM proxy marks and moves no
// shared Claude account) through a type assertion on Extensions.Gateway, which
// fails SILENTLY if the gateway this binary wires stops having the method. The
// gateway wired here is *agentgateway.Gateway, so pin that it satisfies the
// method api asserts.
func TestTheAgentGatewayTakesTheNoAccountRule(t *testing.T) {
	var gw any = &agentgateway.Gateway{}
	if _, ok := gw.(interface{ UseNoAccount(func(agents.Agent) bool) }); !ok {
		t.Fatal("*agentgateway.Gateway has no UseNoAccount(func(agents.Agent) bool): internal/api's no-account rule would never be installed")
	}
}
