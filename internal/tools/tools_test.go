package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gorredinesh21/orderpilot/internal/cart"
	"github.com/gorredinesh21/orderpilot/internal/order"
	"github.com/gorredinesh21/orderpilot/internal/session"
)

func newSession() *session.Session {
	s := &session.Session{ID: "t1", Area: "Koramangala", UserLat: 12.9345, UserLng: 77.6266}
	s.Cart = cart.New(0)
	s.Orders = order.NewTracker(nil)
	return s
}

// runTool executes a tool and returns its result the way the LLM/HTTP
// consumer sees it — round-tripped through JSON into generic maps.
func runTool(t *testing.T, reg *Registry, s *session.Session, name string, args map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(args)
	tool, ok := reg.Get(name)
	if !ok {
		t.Fatalf("tool %s not registered", name)
	}
	res, err := tool.Run(context.Background(), s, raw)
	if err != nil {
		t.Fatalf("tool %s hard-errored: %v", name, err)
	}
	b, _ := json.Marshal(res)
	var generic map[string]any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("tool %s result not JSON-serializable: %v", name, err)
	}
	return generic
}

func TestSearchRestaurantsTool(t *testing.T) {
	reg := New()
	s := newSession()
	res := runTool(t, reg, s, "search_restaurants", map[string]any{"cuisine": "biryani", "area": "HSR Layout"})
	rests, _ := res["restaurants"].([]interface{})
	if len(rests) == 0 {
		t.Fatal("expected HSR biryani restaurants")
	}
	if _, hasHint := res["hint"]; !hasHint {
		t.Error("search results should hint the next tool")
	}
}

func TestSearchDishesToolRequiresQuery(t *testing.T) {
	reg := New()
	s := newSession()
	raw, _ := json.Marshal(map[string]any{"query": "  "})
	tool, _ := reg.Get("search_dishes")
	if _, err := tool.Run(context.Background(), s, raw); err == nil {
		t.Fatal("empty query must error")
	}
}

func TestFullOrderFlowThroughTools(t *testing.T) {
	reg := New()
	s := newSession()

	// search → dish
	res := runTool(t, reg, s, "search_dishes", map[string]any{"query": "masala dosa", "veg_only": true})
	dishes, _ := res["dishes"].([]interface{})
	if len(dishes) == 0 {
		t.Fatal("no dosa found")
	}
	d0 := dishes[0].(map[string]any)
	itemID := d0["item_id"].(string)

	// add to cart
	add := runTool(t, reg, s, "add_to_cart", map[string]any{"item_id": itemID, "qty": 2})
	cart := add["cart"].(map[string]any)
	if cart["total"].(float64) != d0["price"].(float64)*2 {
		t.Fatalf("cart total = %v, want %v", cart["total"], d0["price"].(float64)*2)
	}

	// unknown item id must be a soft error the model can see
	bad := runTool(t, reg, s, "add_to_cart", map[string]any{"item_id": "bogus"})
	if _, ok := bad["error"]; !ok {
		t.Fatal("unknown item must return soft error")
	}

	// place order
	placed := runTool(t, reg, s, "place_order", map[string]any{})
	orders, _ := placed["orders"].([]interface{})
	if len(orders) != 1 {
		t.Fatalf("placed %d orders, want 1", len(orders))
	}
	o := orders[0].(map[string]any)
	if o["eta_min"].(float64) < 6 {
		t.Errorf("ETA below floor: %v", o["eta_min"])
	}

	// cart cleared after placement
	s.Mu.Lock()
	empty := s.Cart.Empty()
	s.Mu.Unlock()
	if !empty {
		t.Fatal("cart must clear after placing")
	}

	// track it
	tracked := runTool(t, reg, s, "track_order", map[string]any{})
	if tracked["order_id"] != o["order_id"] {
		t.Fatalf("tracking picked %v, want %v", tracked["order_id"], o["order_id"])
	}
	if _, ok := tracked["eta_seconds_remaining"]; !ok {
		t.Error("tracking must expose remaining ETA")
	}
}

func TestBudgetGuardrailThroughTools(t *testing.T) {
	reg := New()
	s := newSession()

	// find the cheapest dosa in the corpus and set the budget exactly there
	res := runTool(t, reg, s, "search_dishes", map[string]any{"query": "dosa"})
	dishes, _ := res["dishes"].([]interface{})
	if len(dishes) == 0 {
		t.Fatal("no dosa found")
	}
	cheapest := -1
	var cheapestID string
	for _, d := range dishes {
		dm := d.(map[string]any)
		p := int(dm["price"].(float64))
		if cheapest == -1 || p < cheapest {
			cheapest, cheapestID = p, dm["item_id"].(string)
		}
	}
	runTool(t, reg, s, "set_budget", map[string]any{"amount": cheapest})

	// the affordable add goes through
	out := runTool(t, reg, s, "add_to_cart", map[string]any{"item_id": cheapestID, "qty": 1})
	if _, ok := out["error"]; ok {
		t.Fatalf("exact-budget add must succeed: %v", out["error"])
	}

	// any second add must trip the guardrail with a budget-flavored error
	for _, d := range dishes {
		dm := d.(map[string]any)
		id := dm["item_id"].(string)
		if id == cheapestID {
			continue
		}
		out := runTool(t, reg, s, "add_to_cart", map[string]any{"item_id": id, "qty": 1})
		errTxt := ""
		if e, ok := out["error"].(string); ok {
			errTxt = e
		}
		if !strings.Contains(errTxt, "budget") {
			t.Fatalf("guardrail error should mention budget, got %q", errTxt)
		}
		return
	}
}

func TestGetMenuUnknownRestaurant(t *testing.T) {
	reg := New()
	s := newSession()
	tool, _ := reg.Get("get_menu")
	raw, _ := json.Marshal(map[string]any{"restaurant_id": "zzz"})
	if _, err := tool.Run(context.Background(), s, raw); err == nil {
		t.Fatal("unknown restaurant must hard-error")
	}
}

func TestSpecListsEveryTool(t *testing.T) {
	reg := New()
	spec := reg.Spec()
	for _, name := range []string{"search_restaurants", "search_dishes", "get_menu", "add_to_cart",
		"remove_from_cart", "get_cart", "set_budget", "place_order", "track_order"} {
		if !strings.Contains(spec, name) {
			t.Errorf("spec missing tool %s", name)
		}
	}
}

func TestPlaceOrderEmptyCartSoftError(t *testing.T) {
	reg := New()
	s := newSession()
	res := runTool(t, reg, s, "place_order", map[string]any{})
	if _, ok := res["error"]; !ok {
		t.Fatal("empty-cart placement must return soft error")
	}
}
