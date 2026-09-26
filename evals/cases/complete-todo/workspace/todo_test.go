package todo

import "testing"

func TestAdd(t *testing.T) {
	var list List
	first := list.Add("buy milk")
	second := list.Add("write tests")
	if first != 1 || second != 2 || len(list.Tasks) != 2 {
		t.Fatalf("unexpected list: %#v", list)
	}
}
