package order

import (
	"log/slog"
	"testing"
	"time"

	"github.com/gorredinesh21/orderpilot/internal/data"
)

func TestETASane(t *testing.T) {
	c := data.Get()
	for _, r := range c.Restaurants {
		eta, km := ETA(r, 12.9345, 77.6266) // user in Koramangala
		if eta < 6 || eta > 90 {
			t.Errorf("ETA %d min for %s (%.1f km) outside sane band", eta, r.Name, km)
		}
		if km < 0 {
			t.Errorf("negative distance for %s", r.Name)
		}
	}
	// farther restaurant must not beat a nearer one by prep time alone in
	// the extreme: Whitefield from Koramangala should exceed ~15 min
	w, _ := data.Get().RestByID("r08") // Coastal Canvas, Whitefield
	eta, _ := ETA(w, 12.9345, 77.6266)
	if eta < 15 {
		t.Errorf("Whitefield ETA only %d min — distance not factored", eta)
	}
}

func TestPlaceAdvancesState(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	// shrink the timeline so the test runs fast: simulate via a tiny ETA by
	// using a near restaurant; Place enforces a 20s minimum, so poll up to ~8s.
	// pick the lowest-prep restaurant so the compressed timeline is shortest
	rs := data.Get().SearchRestaurants("", "Koramangala", 0, 0, false)
	if len(rs) == 0 {
		t.Fatal("no Koramangala restaurant for the test")
	}
	best := rs[0]
	for _, r := range rs[1:] {
		if r.PrepMin < best.PrepMin {
			best = r
		}
	}
	in := PlaceInput{RestID: best.ID, RestName: best.Name, Subtotal: 250,
		Lines: []PlaceLine{{ItemID: "x", Name: "Dish", Price: 250, Qty: 1}}}
	o := tr.Place(in, 12.9345, 77.6266)
	if o.ID == "" || o.State != StatePlaced {
		t.Fatalf("order not created properly: %+v", o)
	}
	if o.DEName == "" {
		t.Fatal("delivery partner must be assigned at placement")
	}
	deadline := time.Now().Add(75 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		got, _ := tr.Get(o.ID)
		if got.State == StateDelivered {
			last = got.State
			break
		}
		// states must only move forward along AllStates
		if last != "" && stateIndex(got.State) < stateIndex(last) {
			t.Fatalf("state went backwards: %s -> %s", last, got.State)
		}
		last = got.State
		time.Sleep(150 * time.Millisecond)
	}
	if last != StateDelivered {
		t.Fatalf("order did not reach DELIVERED in compressed time, state=%s", last)
	}
}

func stateIndex(s string) int {
	for i, x := range AllStates {
		if x == s {
			return i
		}
	}
	return -1
}

func TestCancel(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	c := data.Get()
	rs := c.SearchRestaurants("biryani", "", 0, 0, false)
	in := PlaceInput{RestID: rs[0].ID, RestName: rs[0].Name, Subtotal: 100,
		Lines: []PlaceLine{{ItemID: "x", Name: "B", Price: 100, Qty: 1}}}
	o := tr.Place(in, 12.9345, 77.6266)
	if !tr.Cancel(o.ID) {
		t.Fatal("cancel of a fresh order must succeed")
	}
	got, _ := tr.Get(o.ID)
	if got.State != StateCancelled {
		t.Fatalf("state = %s, want CANCELLED", got.State)
	}
	if tr.Cancel(o.ID) {
		t.Fatal("double cancel must fail")
	}
	// cancelled orders must stop advancing
	time.Sleep(300 * time.Millisecond)
	got, _ = tr.Get(o.ID)
	if got.State != StateCancelled {
		t.Fatal("cancelled order mutated after cancel")
	}
}

func TestAllListsEverything(t *testing.T) {
	tr := NewTracker(slog.New(slog.DiscardHandler))
	c := data.Get()
	for _, r := range c.SearchRestaurants("pizza", "", 0, 0, false)[:2] {
		tr.Place(PlaceInput{RestID: r.ID, RestName: r.Name, Subtotal: 99,
			Lines: []PlaceLine{{ItemID: "x", Name: "P", Price: 99, Qty: 1}}}, 12.9345, 77.6266)
	}
	if len(tr.All()) != 2 {
		t.Fatalf("All() = %d orders, want 2", len(tr.All()))
	}
}

func TestETARemainingClamps(t *testing.T) {
	o := &Order{DeliverBy: time.Now().Add(-time.Hour)}
	if o.ETASecondsRemaining() != 0 {
		t.Fatal("past-due order must clamp ETA to 0")
	}
}
