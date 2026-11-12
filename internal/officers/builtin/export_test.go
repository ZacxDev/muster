package builtin

// FaultDoubleClaim turns on the conformance suite's fault: a claim of an
// item another instance holds under a live lease succeeds. Test-only.
func (b *Backend) FaultDoubleClaim() { b.faultDoubleClaim = true }
