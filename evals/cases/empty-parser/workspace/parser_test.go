package parser

import "testing"

func TestParsePairs(t *testing.T) {
	got := ParsePairs("name=mai\nmode=fast")
	if got["name"] != "mai" || got["mode"] != "fast" || len(got) != 2 {
		t.Fatalf("unexpected pairs: %#v", got)
	}
}
