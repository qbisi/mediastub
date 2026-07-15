package core

import "testing"

func TestPlanExtentsReturnsDeepCopy(t *testing.T) {
	plan, err := NewPlan(16, []Extent{{Offset: 3, Data: []byte("abc")}})
	if err != nil {
		t.Fatal(err)
	}
	extents := plan.Extents()
	extents[0].Offset = 0
	extents[0].Data[0] = 'x'
	again := plan.Extents()
	if again[0].Offset != 3 || string(again[0].Data) != "abc" {
		t.Fatalf("Plan was mutated through Extents: %+v", again)
	}
}

func TestPlanHashIsStableAndContentSensitive(t *testing.T) {
	first, err := NewPlan(16, []Extent{{Offset: 8, Data: []byte("tail")}, {Offset: 0, Data: []byte("head")}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewPlan(16, []Extent{{Offset: 0, Data: []byte("head")}, {Offset: 8, Data: []byte("tail")}})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := NewPlan(16, []Extent{{Offset: 0, Data: []byte("HEAD")}, {Offset: 8, Data: []byte("tail")}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash() != second.Hash() {
		t.Fatal("equivalent normalized plans have different hashes")
	}
	if first.Hash() == changed.Hash() {
		t.Fatal("different plan content has the same hash")
	}
}
