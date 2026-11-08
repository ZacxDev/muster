package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Case is one extracted chief input (extract.sql's row).
type Case struct {
	TurnID       int64         `json:"turn_id"`
	MessageID    *int64        `json:"message_id"`
	AutoRouted   bool          `json:"auto_routed"`
	OwnerTyped   bool          `json:"owner_typed"`
	At           string        `json:"at"`
	Input        string        `json:"input"`
	ClaudeEvents []ClaudeEvent `json:"claude_events"`
	DB           CaseDB        `json:"db"`
}

// ClaudeEvent is one assistant row of the real chief's transcript in the turn.
type ClaudeEvent struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"` // text | tool_call | tool_result
	Tool    string `json:"tool"`
	OK      bool   `json:"ok"`
	Content string `json:"content"`
}

// CaseDB is the side effects the database holds for the case's message.
type CaseDB struct {
	Message *struct {
		Status         string  `json:"status"`
		TaskID         *int64  `json:"task_id"`
		SettleReason   *string `json:"settle_reason"`
		RoutedTo       *string `json:"routed_to"`
		NoReply        bool    `json:"no_reply"`
		SenderVerified bool    `json:"sender_verified"`
		PluginID       int64   `json:"plugin_id"`
	} `json:"message"`
	Routes []struct {
		Target string `json:"target"`
		State  string `json:"state"`
	} `json:"routes"`
	Drafts []struct {
		Agent   string  `json:"agent"`
		RouteID *string `json:"route_id"`
		Status  string  `json:"status"`
	} `json:"drafts"`
	Asks []struct {
		ID      int64  `json:"id"`
		AgentID *int64 `json:"agent_id"`
		System  bool   `json:"system"`
		Kind    string `json:"kind"`
	} `json:"asks"`
}

// Source is the input's origin, for the report (no content).
func (c Case) Source() string {
	switch {
	case c.OwnerTyped:
		return "owner-typed"
	case strings.Contains(c.Input[:min(len(c.Input), 120)], "task event for task_id"):
		return "task event"
	}
	head := c.Input[:min(len(c.Input), 60)]
	for _, p := range []string{"Agent mail", "Discord DMs", "Civitai feedback"} {
		if strings.HasPrefix(head, "Plugin "+p+":") {
			return p
		}
	}
	return "plugin"
}

// ClaudeCalls is the real chief's tool calls in the turn, in order.
func (c Case) ClaudeCalls() []Call {
	var out []Call
	for _, e := range c.ClaudeEvents {
		if e.Kind == "tool_call" {
			out = append(out, Call{Tool: shortTool(e.Tool), Args: json.RawMessage(validJSON(e.Content))})
		}
	}
	return out
}

// ClaudeResult is the result the real chief got for its first call of tool in
// the turn, or "".
func (c Case) ClaudeResult(tool string) string {
	for i, e := range c.ClaudeEvents {
		if e.Kind == "tool_call" && shortTool(e.Tool) == tool {
			for _, r := range c.ClaudeEvents[i+1:] {
				if r.Kind == "tool_result" && shortTool(r.Tool) == tool {
					return r.Content
				}
			}
		}
	}
	return ""
}

// shortTool is a tool name without the muster MCP server's prefix.
func shortTool(name string) string { return strings.TrimPrefix(name, "mcp__muster__") }

func validJSON(s string) string {
	if json.Valid([]byte(s)) {
		return s
	}
	b, _ := json.Marshal(s)
	return string(b)
}

func readCases(path string) ([]Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Case
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c Case
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, c)
	}
	return out, sc.Err()
}
