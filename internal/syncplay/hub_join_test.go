package syncplay

import "testing"

func TestJoinFailureWindowBlocksGuessing(t *testing.T) {
	h := &Hub{}
	for i := 0; i < joinFailureLimit; i++ {
		if !h.joinAllowed("user-a") {
			t.Fatalf("blocked after only %d failures", i)
		}
		h.noteJoinFailure("user-a")
	}
	if h.joinAllowed("user-a") {
		t.Fatal("the join budget was not exhausted")
	}
	if !h.joinAllowed("user-b") {
		t.Fatal("one account's failures blocked another account")
	}
	h.clearJoinFailures("user-a")
	if !h.joinAllowed("user-a") {
		t.Fatal("a successful join did not reset the account budget")
	}
}
