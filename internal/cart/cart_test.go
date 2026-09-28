package cart

import (
	"errors"
	"testing"

	"github.com/gorredinesh21/orderpilot/internal/data"
)

func fakeItem(id, rest string, price int) data.MenuItem {
	return data.MenuItem{ID: id, RestID: rest, RestName: "R" + rest, Name: "Item " + id, Price: price, Veg: true}
}

func TestAddAndTotal(t *testing.T) {
	c := New(500)
	if err := c.Add(fakeItem("i1", "r1", 120), 2); err != nil {
		t.Fatal(err)
	}
	if err := c.Add(fakeItem("i2", "r1", 60), 1); err != nil {
		t.Fatal(err)
	}
	if c.Total() != 300 {
		t.Fatalf("total = %d, want 300", c.Total())
	}
	if c.Count() != 3 {
		t.Fatalf("count = %d, want 3", c.Count())
	}
}

func TestBudgetGuardrail(t *testing.T) {
	c := New(200)
	if err := c.Add(fakeItem("i1", "r1", 120), 1); err != nil {
		t.Fatal(err)
	}
	err := c.Add(fakeItem("i2", "r2", 100), 1)
	if !errors.Is(err, ErrOverBudget) {
		t.Fatalf("want ErrOverBudget, got %v", err)
	}
	// cart unchanged after rejected add
	if c.Total() != 120 {
		t.Fatalf("total after rejected add = %d, want 120", c.Total())
	}
	// raise budget, retry succeeds
	_ = c.SetBudget(250)
	if err := c.Add(fakeItem("i2", "r2", 100), 1); err != nil {
		t.Fatalf("add after budget raise failed: %v", err)
	}
}

func TestGuardrailCountsQty(t *testing.T) {
	c := New(300)
	err := c.Add(fakeItem("i1", "r1", 120), 3) // 360 > 300
	if !errors.Is(err, ErrOverBudget) {
		t.Fatalf("want ErrOverBudget for qty push over, got %v", err)
	}
	if err := c.Add(fakeItem("i1", "r1", 120), 2); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveAndClear(t *testing.T) {
	c := New(500)
	_ = c.Add(fakeItem("i1", "r1", 100), 3)
	if err := c.Remove("i1", 1); err != nil {
		t.Fatal(err)
	}
	if c.Count() != 2 {
		t.Fatalf("count = %d, want 2", c.Count())
	}
	if err := c.Remove("i1", 5); err != nil {
		t.Fatal(err)
	}
	if !c.Empty() {
		t.Fatal("cart should be empty")
	}
	if err := c.Remove("i1", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSnapshotGroupsByRestaurant(t *testing.T) {
	c := New(1000)
	_ = c.Add(fakeItem("i1", "r1", 100), 1)
	_ = c.Add(fakeItem("i2", "r1", 50), 2)
	_ = c.Add(fakeItem("i3", "r2", 70), 1)
	groups := c.Snapshot()
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	var total int
	for _, g := range groups {
		if g.RestID == "r1" && g.Subtotal != 200 {
			t.Errorf("r1 subtotal = %d, want 200", g.Subtotal)
		}
		total += g.Subtotal
	}
	if total != 270 {
		t.Fatalf("grouped total = %d, want 270", total)
	}
}

func TestDefaultBudget(t *testing.T) {
	if New(0).Budget != 600 {
		t.Fatal("default budget must be 600")
	}
	if err := New(100).SetBudget(0); err == nil {
		t.Fatal("zero budget must be rejected")
	}
}
