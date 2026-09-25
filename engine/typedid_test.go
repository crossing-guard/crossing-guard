package engine

import (
	"strings"
	"testing"
)

func TestNewTypedIDHasWireShapeAndIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id := NewTypedID("evt")
		if !IsTypedID(id) {
			t.Fatalf("id %q does not match the schema pattern", id)
		}
		if !strings.HasPrefix(id, "evt_") || len(id) != 4+26 {
			t.Fatalf("id %q: want evt_ + 26 chars", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id minted: %s", id)
		}
		seen[id] = true
	}
}

func TestNewTypedIDIsTimeOrderedAcrossMilliseconds(t *testing.T) {
	a := NewTypedID("evt")
	// The 10-char time prefix is monotonic; a later millisecond sorts after.
	b := NewTypedID("evt")
	if a[4:14] > b[4:14] {
		t.Fatalf("time prefix went backwards: %s then %s", a, b)
	}
}

func TestDeterministicTypedIDIsStableAndDistinct(t *testing.T) {
	x := DeterministicTypedID("evt", "dev_A:42")
	if x != DeterministicTypedID("evt", "dev_A:42") {
		t.Fatal("same material must yield the same id")
	}
	if x == DeterministicTypedID("evt", "dev_A:43") || x == DeterministicTypedID("evt", "dev_B:42") {
		t.Fatal("different material must yield a different id")
	}
	if !IsTypedID(x) {
		t.Fatalf("derived id %q does not match the schema pattern", x)
	}
}

func TestIsTypedIDRejectsTheSpoolShape(t *testing.T) {
	// The spool's hex ids are a different contract and must never pass as wire ids.
	if IsTypedID("obs_24d10dff5a6cc781de1f68ac0393d7f4") {
		t.Fatal("lowercase hex observation id must not be a typed id")
	}
	for _, bad := range []string{"evt_", "ev_0123456789", "evt_0123456789ILOU", "EVT_0123456789"} {
		if IsTypedID(bad) {
			t.Fatalf("%q must not be a typed id", bad)
		}
	}
}
