package ui

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// stepUpNode is the first element under n whose id is id; nil for none.
func stepUpNode(n *html.Node, id string) *html.Node {
	if n.Type == html.ElementNode {
		for _, a := range n.Attr {
			if a.Key == "id" && a.Val == id {
				return n
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := stepUpNode(c, id); f != nil {
			return f
		}
	}
	return nil
}

func stepUpHas(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

// within reports whether n is inside anc.
func within(n, anc *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p == anc {
			return true
		}
	}
	return false
}

// TestTheConfirmDialogPutsThePasswordInACollapse: the password field and its
// button are inside ONE <details>, drawn SHUT with its summary shown — so a
// passkey user never sees the password field flash; the script opens it (and
// hides the summary) only once no passkey is usable. The passkey button is
// outside it, drawn hidden until the script decides; Cancel is outside it too,
// so the shut password never hides the way out.
//
// REGRESSION TEST: red at the commit before the collapse (no
// term-confirm-password-box at all; the password field sat in the form).
func TestTheConfirmDialogPutsThePasswordInACollapse(t *testing.T) {
	page := renderString(t, confirmDialog(&StepUpView{ConfirmPath: "/auth/confirm", PasskeyPath: "/auth/confirm/passkey", Window: "12 hours"}))
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	box := stepUpNode(doc, stepUpPasswordBoxID)
	if box == nil || box.Data != "details" {
		t.Fatalf("no <details id=%q> holds the password:\n%s", stepUpPasswordBoxID, page)
	}
	if stepUpHas(box, "open") {
		t.Error("the password's collapse is drawn open: a passkey user would see the password field before the passkey is offered")
	}
	sum := stepUpNode(doc, stepUpPasswordToggleID)
	if sum == nil || sum.Data != "summary" || sum.Parent != box {
		t.Fatalf("the collapse's summary is not its own first-level <summary>")
	}
	if stepUpHas(sum, "hidden") {
		t.Error("the summary is drawn hidden: the shut password would have no way in")
	}
	for _, id := range []string{"term-confirm-password", "term-confirm-submit"} {
		if n := stepUpNode(doc, id); n == nil || !within(n, box) {
			t.Errorf("#%s is not inside the password's collapse", id)
		}
	}
	for _, id := range []string{"term-confirm-passkey", "term-confirm-cancel", "term-confirm-error"} {
		if n := stepUpNode(doc, id); n == nil || within(n, box) {
			t.Errorf("#%s is missing, or inside the password's collapse (it must show while the password is shut)", id)
		}
	}
	if pk := stepUpNode(doc, "term-confirm-passkey-box"); pk == nil || !stepUpHas(pk, "hidden") {
		t.Error("the passkey box is not drawn hidden until the account is known to have a passkey")
	}
}
