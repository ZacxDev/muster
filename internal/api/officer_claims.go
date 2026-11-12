package api

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ---------------------------------------------------------------------------
// AN ITEM'S payload.claims (docs/officers.md "The review loop through HQ").
//
// In shadow a shepherd fences its claims blocks in its item summary, and HQ
// copies them into the NEXT audit round item's payload as `payload.claims`, a
// string. A read-only auditor officer's instance writes it to a file and hands
// it to its audit tool. muster passes it through as data: officer_brief,
// officer_items and officer_claim return HQ's body BYTE FOR BYTE.
//
// 🔴 THE ONE EXCEPTION IS SIZE, AND IT IS NEVER A SILENT CUT. A claims string
// over maxClaimsBytes is replaced WHOLE by claimsWithheldMarker naming its
// size, and the item gains `claims_withheld_bytes`: a cut mid-block would hand
// the audit tool a premise that reads complete and is not. Only then is the
// body re-encoded; every other answer is HQ's bytes unchanged.
// ---------------------------------------------------------------------------

// maxClaimsBytes bounds one payload.claims muster hands an instance.
const maxClaimsBytes = 64 << 10

// claimsWithheldMarker replaces a payload.claims over maxClaimsBytes.
func claimsWithheldMarker(n int) string {
	return fmt.Sprintf("[muster withheld payload.claims: %d bytes, over its %d-byte cap; nothing of it is here. Do not audit without it: complete the item escalated, saying the claims were too large]",
		n, maxClaimsBytes)
}

// capPayloadClaims is body with every payload.claims over maxClaimsBytes
// withheld (see the top of this file), and how many were. A body that cannot
// hold one (shorter than the cap) or holds none over it is returned as it is.
func capPayloadClaims(body []byte) ([]byte, int) {
	if len(body) <= maxClaimsBytes {
		return body, 0
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return body, 0
	}
	n := withholdClaims(v)
	if n == 0 {
		return body, 0
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return body, 0
	}
	return bytes.TrimRight(out.Bytes(), "\n"), n
}

// withholdClaims replaces, anywhere in v, each object's payload.claims string
// over maxClaimsBytes, returning how many it replaced.
func withholdClaims(v any) int {
	n := 0
	switch t := v.(type) {
	case map[string]any:
		if p, ok := t["payload"].(map[string]any); ok {
			if c, ok := p["claims"].(string); ok && len(c) > maxClaimsBytes {
				p["claims"] = claimsWithheldMarker(len(c))
				p["claims_withheld_bytes"] = len(c)
				n++
			}
		}
		for _, e := range t {
			n += withholdClaims(e)
		}
	case []any:
		for _, e := range t {
			n += withholdClaims(e)
		}
	}
	return n
}
