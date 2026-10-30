package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/flows"
)

// The fleet-report request's parser and the reply's encoding (fleet_report.go),
// with no database. Fixture values are invented.

const goodRequest = `{"type":"fleet_report_request","v":1,"request_id":"req-77","member":"example-member",` +
	`"window":{"since":"2000-01-03T00:00:00Z","until":"2000-01-04T12:00:00Z","zone":"America/Argentina/Buenos_Aires"}}`

func TestARequestParsesAndEverythingElseIsNotOne(t *testing.T) {
	p, ok := parseFleetReportRequest("\n" + goodRequest + "\n")
	if !ok || p.problem != "" || p.requestID != "req-77" || p.member != "example-member" ||
		p.window.Zone != "America/Argentina/Buenos_Aires" || p.until.Sub(p.since).Hours() != 36 {
		t.Fatalf("control: %+v %v", p, ok)
	}
	for name, body := range map[string]string{
		"prose":           "please send the fleet report",
		"another type":    `{"type":"fleet_report","v":1}`,
		"no type":         `{"v":1,"request_id":"x"}`,
		"an array":        `[` + goodRequest + `]`,
		"trailing prose":  goodRequest + " thanks",
		"a second object": goodRequest + goodRequest,
	} {
		if _, ok := parseFleetReportRequest(body); ok {
			t.Errorf("%s: read as a request", name)
		}
	}
}

func TestABrokenRequestIsAnErrorThatEchoesItsID(t *testing.T) {
	for name, c := range map[string]struct{ from, to, id, problem string }{
		"version":   {`"v":1`, `"v":2`, "req-77", "unsupported request version"},
		"no id":     {`"request_id":"req-77"`, `"request_id":""`, "", "request_id must be"},
		"long id":   {`"req-77"`, `"` + strings.Repeat("r", 65) + `"`, "", "request_id must be"},
		"member":    {`"example-member"`, `"not a login"`, "req-77", "member must be a GitHub login"},
		"since":     {`"2000-01-03T00:00:00Z"`, `"yesterday"`, "req-77", "RFC 3339"},
		"zone":      {`"America/Argentina/Buenos_Aires"`, `"../etc/passwd"`, "req-77", "IANA zone"},
		"inverted":  {`"2000-01-03T00:00:00Z"`, `"2000-01-05T00:00:00Z"`, "req-77", "the window is empty"},
		"too long":  {`"2000-01-03T00:00:00Z"`, `"1999-12-25T00:00:00Z"`, "req-77", "longer than 4 days"},
		"id is int": {`"request_id":"req-77"`, `"request_id":77`, "", "request_id must be"},
	} {
		body := strings.Replace(goodRequest, c.from, c.to, 1)
		if body == goodRequest {
			t.Fatalf("%s: the fixture edit changed nothing", name)
		}
		p, ok := parseFleetReportRequest(body)
		if !ok || p.requestID != c.id || !strings.Contains(p.problem, c.problem) {
			t.Errorf("%s: %+v %v; want id %q and a problem saying %q", name, p, ok, c.id, c.problem)
		}
	}
}

// 🔴 The reply is ASCII, so folding it (NFKC) changes nothing — and a field
// holding a character NFKC turns into a quote cannot change its structure.
func TestTheReplyEncodingSurvivesFolding(t *testing.T) {
	title := "say ＂hi＂ to été \U0001F600"
	out := asciiJSON(fleetFocus{Title: title, At: "x"})
	for _, r := range out {
		if r > 0x7E || r < 0x20 {
			t.Fatalf("the encoding carries %U: %s", r, out)
		}
	}
	if flows.NormalizeAgentText(out) != out {
		t.Fatalf("the encoding is not its own normal form: %s", out)
	}
	var back fleetFocus
	if err := json.Unmarshal([]byte(out), &back); err != nil || back.Title != title {
		t.Fatalf("round trip: %q %v", back.Title, err)
	}
	// Control: the plain encoding is NOT stable under folding, so the test
	// above is not passing on an input that never needed the escaping.
	plain, _ := json.Marshal(fleetFocus{Title: title})
	var folded fleetFocus
	if err := json.Unmarshal([]byte(flows.NormalizeAgentText(string(plain))), &folded); err == nil && folded.Title == title {
		t.Fatal("control: folding the plain encoding kept the title, so this fixture proves nothing")
	}
}
