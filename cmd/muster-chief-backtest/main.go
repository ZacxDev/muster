// Command muster-chief-backtest replays real chief inputs through the chief's
// own harness — the Claude Code CLI, the chief's standing instructions
// (agentspec.ChiefInstructionsClaudeCode) and its muster MCP tool schemas —
// with the model reached through muster's LLM proxy (internal/llmproxy, the
// code a chief on MUSTER_CHIEF_LLM=openrouter runs on) and pinned to an
// OpenRouter model, and scores each replay against what the Claude-backed
// chief actually did.
//
// 🔴 NOTHING IS SENT TO MUSTER. Every muster tool is answered by a STUB MCP
// server (the `mcp` subcommand): a side-effecting tool records its call and
// arguments and returns a plausible success; a read tool returns what the
// real chief was given for that tool in that turn when it called it (the
// transcript's tool_result), otherwise an empty answer the report marks as
// stubbed. The CLI is allowed the muster tools and ToolSearch only: every
// built-in tool (Bash, Write, WebFetch, …) is denied, and an attempt is
// recorded.
//
// The steps, each its own subcommand:
//
//	extract.sh > cases.jsonl                       # READ ONLY, from the database
//	muster-chief-backtest instructions > CLAUDE.md # the chief's prose (plugins + routing on)
//	muster-chief-backtest run   -cases cases.jsonl -tools tools.json -init init.json -out DIR
//	muster-chief-backtest score -cases cases.jsonl -out DIR > report.md
//
// tools.json and init.json are an MCP tools/list and initialize answer for
// the chief, read from a LOCAL muster started with production's feature flags
// (never from production).
//
// 🔴 NEVER RUN IT ON PRODUCTION'S MUSTER_AGENT_OPENROUTER_KEY. That key has a
// $5/day cap that production depends on (focus summaries, the stall
// classifier, gateway agents); on 2026-11-08 this backtest exhausted it and
// broke production's focus summaries for about five hours. The key is read
// from BT_OPENROUTER_KEY only — make a key for the backtest, with its own
// credit limit — and run refuses when it is unset, or equal to
// MUSTER_AGENT_OPENROUTER_KEY in its own environment (backtestKey). Nothing
// in this command reads a Kubernetes secret. The key is never printed or
// written. A replay costs from under a cent (Flash) to about five cents (V4
// Pro) per input, so size the backtest key's limit to the run.
//
// cases.jsonl holds production message text: keep it, and -out, outside the
// repository. Nothing in CI runs this command; `go test` covers its scoring.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "instructions":
		err = cmdInstructions(os.Args[2:])
	case "mcp":
		err = cmdMCP(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "score":
		err = cmdScore(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "muster-chief-backtest:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: muster-chief-backtest instructions|mcp|run|score [flags]")
	os.Exit(2)
}
