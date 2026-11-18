package mdfence

import (
	"reflect"
	"testing"
)

func TestBlocksListsEveryFencedBlockInOrder(t *testing.T) {
	// The handoff skill's kickoff shape: prose, then one untagged block.
	kick := "Done. Here is the kickoff:\n\n```\n/resume — continue the x work. Canonical handoff (read first): /a/b.md\nship it\n```\n"
	got := Blocks(kick)
	want := []Block{{Lang: "", Body: "/resume — continue the x work. Canonical handoff (read first): /a/b.md\nship it", Line: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kickoff: got %#v, want %#v", got, want)
	}

	two := "a\n~~~text info\nT1\n~~~\nb\n```go\nfunc x() {}\n````\nc"
	got = Blocks(two)
	want = []Block{{Lang: "text", Body: "T1", Line: 1}, {Lang: "go", Body: "func x() {}", Line: 5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("two blocks: got %#v, want %#v", got, want)
	}
}

func TestBlocksFollowsTheFenceRules(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []Block
	}{
		{"a shorter fence does not close", "````\na\n```\nb\n````", []Block{{Body: "a\n```\nb", Line: 0}}},
		{"the other character does not close", "```\na\n~~~\n```", []Block{{Body: "a\n~~~", Line: 0}}},
		{"a closing fence with text is content", "```\na\n``` x\n```", []Block{{Body: "a\n``` x", Line: 0}}},
		{"an unclosed block runs to the end", "x\n```sh\nls\npwd", []Block{{Lang: "sh", Body: "ls\npwd", Line: 1}}},
		{"two backticks is not a fence", "``\na\n``", nil},
		{"a backtick info string with a backtick is not a fence", "``` a`b\nx\n```", []Block{{Body: "", Line: 2}}},
		{"indented four is not a fence", "    ```\nx\n    ```", nil},
		{"indented three is", "   ```\nx\n   ```", []Block{{Body: "x", Line: 0}}},
		{"CRLF lines", "```\r\nx\r\n```\r\n", []Block{{Body: "x", Line: 0}}},
		{"no blocks", "plain prose", nil},
	}
	for _, c := range cases {
		if got := Blocks(c.text); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.name, got, c.want)
		}
	}
}
