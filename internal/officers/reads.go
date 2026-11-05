package officers

import "encoding/json"

// INERT SCAFFOLDING for the red-first tests (replaced by the implementation
// commit): the new symbols exist so the tests build, and do nothing.

// ToolObsRaw is the raw obs tool's name; nothing offers or dispatches it yet.
const ToolObsRaw = "officer_obs_raw"

// ReadBudgetNote is empty: the budget is not stated yet.
const ReadBudgetNote = ""

// CountsRead counts nothing yet.
func CountsRead(o Officer, tool string) bool { return false }

// ObsRawBody builds no body yet.
func ObsRawBody(args []byte, item ItemID, instance string) (json.RawMessage, error) { return nil, nil }
