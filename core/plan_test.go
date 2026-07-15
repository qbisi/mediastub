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
