package builtin

import "github.com/ZacxDev/muster/internal/cronspec"

// Cron is a parsed five-field cron expression, evaluated in UTC. The grammar
// lives in internal/cronspec, which scheduled host jobs share.
type Cron = cronspec.Cron

// ParseCron parses expr (cronspec.Parse), refusing (naming the field)
// anything outside the grammar.
func ParseCron(expr string) (Cron, error) { return cronspec.Parse(expr) }
