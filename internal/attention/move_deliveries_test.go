package attention

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
)

// TestTheMoveClassifiesEveryDeliveryState: every Delivery constant this
// package declares (read from its source, so a new one cannot be missed) but
// DeliveryNone is classified by the task move (notes.MoveRefusals' ask
// predicate) as exactly one of finished or live. A new delivery state is a
// decision for the move — finished only if no transition of this package's
// leaves it — never "finished" by default. Paired with notes'
// TestEveryAskDeliveryStateIsClassifiedForTheMove, which reads the database's
// own list.
func TestTheMoveClassifiesEveryDeliveryState(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "attention.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, sp := range g.Specs {
			v := sp.(*ast.ValueSpec)
			if id, ok := v.Type.(*ast.Ident); !ok || id.Name != "Delivery" {
				continue
			}
			for _, val := range v.Values {
				lit, ok := val.(*ast.BasicLit)
				if !ok {
					t.Fatalf("a Delivery constant that is not a literal: %#v", val)
				}
				s, _ := strconv.Unquote(lit.Value)
				states = append(states, s)
			}
		}
	}
	if len(states) < 7 || !slices.Contains(states, string(DeliveryPull)) {
		t.Fatalf("instrument: read %q from attention.go", states)
	}
	for _, s := range states {
		if s == string(DeliveryNone) {
			continue
		}
		fin, live := slices.Contains(notes.AskDeliveriesFinished, s), slices.Contains(notes.AskDeliveriesLive, s)
		if fin == live {
			t.Errorf("delivery state %q: finished %v, live %v for the task move; want exactly one", s, fin, live)
		}
	}
}
