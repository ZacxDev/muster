package main

import (
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/officers"
)

// TestTheChatSessionDefaultIsCcdsLaneDefault: an officer that declares no
// chat_sessions sets no CCD_MAX_SESSIONS on its pod (agentspec), so its
// chats get ccd's own lane default — which must be the default muster counts
// with (officers.DefaultChatSessions), or muster would dispatch more chats to
// an instance than its ccd runs (or fewer). And the variable muster sets is the
// one ccd reads.
func TestTheChatSessionDefaultIsCcdsLaneDefault(t *testing.T) {
	if defaultMaxLanes != officers.DefaultChatSessions {
		t.Fatalf("ccd runs %d lanes by default, muster counts %d chat sessions per instance by default", defaultMaxLanes, officers.DefaultChatSessions)
	}
	if headerCCMCPSession != agents.HeaderCCMCPSession {
		t.Fatalf("ccd reads a chat lane's MCP-session ask as %q, muster sends %q", headerCCMCPSession, agents.HeaderCCMCPSession)
	}
	if envMaxLanes != agentspec.EnvCCDMaxSessions {
		t.Fatalf("ccd reads its lane cap from %q, muster sets %q", envMaxLanes, agentspec.EnvCCDMaxSessions)
	}
}
