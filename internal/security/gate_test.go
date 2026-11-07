package security

import "testing"

func TestGateBoundsRequestsAndReleasesCapacity(t *testing.T) {
	var gate Gate
	for range 128 {
		if !gate.Try() {
			t.Fatal("capacity lost")
		}
	}
	if gate.Try() {
		t.Fatal("unbounded requests")
	}
	gate.Leave()
	if !gate.Try() {
		t.Fatal("capacity not released")
	}
	for range 128 {
		gate.Leave()
	}
}
