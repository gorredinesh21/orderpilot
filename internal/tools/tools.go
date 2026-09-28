// Package tools defines the agent's typed tool registry. Each tool parses
// its own args from JSON, runs against session state, and returns
// JSON-friendly results the LLM can reason over.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gorredinesh21/orderpilot/internal/data"
	"github.com/gorredinesh21/orderpilot/internal/order"
	"github.com/gorredinesh21/orderpilot/internal/session"
)

// flexInt accepts JSON numbers, floats and numeric strings ("200", 4.0, 200)
// because small instruct models routinely emit tool args in the wrong shape.
type flexInt int

// UnmarshalJSON coerces whatever the model emitted into an int.
func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("not a number: %s", s)
	}
	*f = flexInt(v)
	return nil
}

// flexBool accepts true/false, "true"/"false", 1/0 — same reason as flexInt.
type flexBool bool

// UnmarshalJSON coerces loose boolean spellings.
func (f *flexBool) UnmarshalJSON(b []byte) error {
	s := strings.ToLower(strings.Trim(strings.TrimSpace(string(b)), `"`))
	switch s {
	case "true", "1", "yes":
		*f = true
	case "false", "0", "no", "", "null":
		*f = false
	default:
		return fmt.Errorf("not a boolean: %s", s)
	}
	return nil
}

// Param describes one tool argument for the system prompt.
type Param struct {
	Name     string
	Type     string
	Desc     string
	Required bool
}

// Tool is one agent-callable capability.
type Tool struct {
	Name        string
	Description string
	Params      []Param
	Run         func(ctx context.Context, s *session.Session, args json.RawMessage) (any, error)
}

// Registry holds all tools by name.
type Registry struct {
	byName map[string]Tool
	order  []string
}

// New builds the registry with every production tool.
func New() *Registry {
	r := &Registry{byName: map[string]Tool{}}
	r.add(searchRestaurants())
	r.add(searchDishes())
	r.add(getMenu())
	r.add(addToCart())
	r.add(removeFromCart())
	r.add(getCart())
	r.add(setBudget())
	r.add(placeOrder())
	r.add(trackOrder())
	return r
}

func (r *Registry) add(t Tool) {
	r.byName[t.Name] = t
	r.order = append(r.order, t.Name)
}

// Get looks a tool up.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Spec renders the tool documentation block for the LLM system prompt.
func (r *Registry) Spec() string {
	var b strings.Builder
	for _, name := range r.order {
		t := r.byName[name]
		fmt.Fprintf(&b, "### %s\n%s\n", t.Name, t.Description)
		if len(t.Params) > 0 {
			b.WriteString("Parameters:\n")
			for _, p := range t.Params {
				req := ""
				if p.Required {
					req = " (required)"
				}
				fmt.Fprintf(&b, "- %s (%s)%s: %s\n", p.Name, p.Type, req, p.Desc)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("bad args for tool: %w", err)
	}
	return nil
}

// ---- search_restaurants ----

type searchRestArgs struct {
	Cuisine        string `json:"cuisine"`
	Area           string `json:"area"`
	MaxPriceForTwo flexInt `json:"max_price_for_two"`
	MinRating      flexInt `json:"min_rating"`
	VegOnly        flexBool `json:"veg_only"`
}

func searchRestaurants() Tool {
	return Tool{
		Name: "search_restaurants",
		Description: "Search the marketplace for restaurants. Returns up to 5 matches ranked by rating with id, cuisines, area, rating and ETA minutes. Follow up with get_menu(restaurant_id) to see dishes.",
		Params: []Param{
			{"cuisine", "string", "cuisine keyword: biryani, andhra, southindian, northindian, chinese, pizza, burgers, kebab, thai, seafood, continental, desserts, beverages", false},
			{"area", "string", "Bangalore area: Koramangala, Indiranagar, HSR Layout, Whitefield, Jayanagar, JP Nagar, Marathahalli, Church Street", false},
			{"max_price_for_two", "integer", "max cost for two in rupees", false},
			{"min_rating", "number", "minimum restaurant rating (e.g. 4.0)", false},
			{"veg_only", "boolean", "pure-veg restaurants only", false},
		},
		Run: func(ctx context.Context, s *session.Session, args json.RawMessage) (any, error) {
			var a searchRestArgs
			if err := decode(args, &a); err != nil {
				return nil, err
			}
			rs := data.Get().SearchRestaurants(a.Cuisine, a.Area, int(a.MaxPriceForTwo), int(a.MinRating), bool(a.VegOnly))
			total := len(rs)
			if len(rs) > 5 {
				rs = rs[:5]
			}
			type row struct {
				ID          string   `json:"id"`
				Name        string   `json:"name"`
				Cuisines    []string `json:"cuisines"`
				Area        string   `json:"area"`
				Rating      float64  `json:"rating"`
				PriceForTwo int      `json:"price_for_two"`
				VegOnly     bool     `json:"veg_only"`
				ETAMin      int      `json:"eta_min"`
			}
			out := []row{}
			for _, r := range rs {
				eta, _ := order.ETA(r, s.UserLat, s.UserLng)
				out = append(out, row{r.ID, r.Name, r.Cuisines, r.Area, r.Rating, r.PriceForTwo, r.VegOnly, eta})
			}
			return map[string]any{"matches": total, "restaurants": out,
				"hint": "call get_menu with a restaurant id to see its dishes"}, nil
		},
	}
}

// ---- search_dishes ----

type searchDishArgs struct {
	Query    string `json:"query"`
	MaxPrice flexInt `json:"max_price"`
	VegOnly  flexBool `json:"veg_only"`
}

func searchDishes() Tool {
	return Tool{
		Name: "search_dishes",
		Description: "Search dishes across all restaurants by keyword (e.g. 'paneer', 'biryani', 'momos'). Returns top 8 dishes with item_id, restaurant, price and rating. Use add_to_cart(item_id) to add.",
		Params: []Param{
			{"query", "string", "dish keyword", true},
			{"max_price", "integer", "max price per dish in rupees", false},
			{"veg_only", "boolean", "veg dishes only", false},
		},
		Run: func(ctx context.Context, s *session.Session, args json.RawMessage) (any, error) {
			var a searchDishArgs
			if err := decode(args, &a); err != nil {
				return nil, err
			}
			if strings.TrimSpace(a.Query) == "" {
				return nil, fmt.Errorf("query is required")
			}
			items := data.Get().SearchDishes(a.Query, int(a.MaxPrice), bool(a.VegOnly), 0)
			if len(items) > 8 {
				items = items[:8]
			}
			type row struct {
				ItemID   string  `json:"item_id"`
				Name     string  `json:"name"`
				RestName string  `json:"restaurant"`
				Area     string  `json:"area"`
				Price    int     `json:"price"`
				Veg      bool    `json:"veg"`
				Spice    int     `json:"spice"`
				Rating   float64 `json:"rating"`
			}
			out := []row{}
			for _, it := range items {
				r, _ := data.Get().RestByID(it.RestID)
				out = append(out, row{it.ID, it.Name, it.RestName, r.Area, it.Price, it.Veg, it.Spice, it.Rating})
			}
			return map[string]any{"dishes": out}, nil
		},
	}
}

// ---- get_menu ----

type getMenuArgs struct {
	RestaurantID string `json:"restaurant_id"`
	Category     string `json:"category"`
	VegOnly      bool   `json:"veg_only"`
}

func getMenu() Tool {
	return Tool{
		Name: "get_menu",
		Description: "List the menu of one restaurant (up to 20 dishes with item_id, price, veg, spice, rating, bestseller). Filter by category (e.g. Biryani, Main Course, Dessert) and veg_only.",
		Params: []Param{
			{"restaurant_id", "string", "restaurant id from search_restaurants", true},
			{"category", "string", "optional category filter", false},
			{"veg_only", "boolean", "veg dishes only", false},
		},
		Run: func(ctx context.Context, s *session.Session, args json.RawMessage) (any, error) {
			var a getMenuArgs
			if err := decode(args, &a); err != nil {
				return nil, err
			}
			r, ok := data.Get().RestByID(a.RestaurantID)
			if !ok {
				return nil, fmt.Errorf("unknown restaurant_id %q", a.RestaurantID)
			}
			items := data.Get().Menu(a.RestaurantID)
			var out []data.MenuItem
			for _, it := range items {
				if a.Category != "" && !strings.EqualFold(it.Category, a.Category) {
					continue
				}
				if bool(a.VegOnly) && !it.Veg {
					continue
				}
				out = append(out, it)
			}
			if len(out) > 20 {
				out = out[:20]
			}
			return map[string]any{"restaurant": r.Name, "area": r.Area, "rating": r.Rating,
				"items": out, "note": "add with add_to_cart(item_id, qty)"}, nil
		},
	}
}

// ---- add_to_cart / remove_from_cart / get_cart ----

type addArgs struct {
	ItemID string `json:"item_id"`
	Qty    flexInt `json:"qty"`
}

func cartSummary(s *session.Session) map[string]any {
	groups := s.Cart.Snapshot()
	total := s.Cart.Total()
	return map[string]any{
		"groups":          groups,
		"total":           total,
		"budget":          s.Cart.Budget,
		"budget_remaining": s.Cart.Budget - total,
		"item_count":      s.Cart.Count(),
	}
}

func addToCart() Tool {
	return Tool{
		Name: "add_to_cart",
		Description: "Add a dish to the cart (default qty 1). The cart supports multiple restaurants; each restaurant becomes its own order at checkout. Budget is enforced — an add that would exceed the budget fails with the reason.",
		Params: []Param{
			{"item_id", "string", "menu item id", true},
			{"qty", "integer", "quantity, default 1", false},
		},
		Run: func(ctx context.Context, s *session.Session, args json.RawMessage) (any, error) {
			var a addArgs
			if err := decode(args, &a); err != nil {
				return nil, err
			}
			it, ok := data.Get().ItemByID(a.ItemID)
			if !ok {
				return map[string]any{"error": fmt.Sprintf("unknown item_id %q — get ids from search_dishes or get_menu", a.ItemID)}, nil
			}
			s.Mu.Lock()
			defer s.Mu.Unlock()
			if err := s.Cart.Add(it, int(a.Qty)); err != nil {
				return map[string]any{"error": err.Error(),
					"hint": "suggest removing an item or ask the user to raise the budget"}, nil
			}
			return map[string]any{"added": map[string]any{"name": it.Name, "qty": int(a.Qty), "price": it.Price},
				"cart": cartSummary(s)}, nil
		},
	}
}

type removeArgs struct {
	ItemID string `json:"item_id"`
	Qty    flexInt `json:"qty"`
}

func removeFromCart() Tool {
	return Tool{
		Name: "remove_from_cart",
		Description: "Remove a dish (or decrement its quantity). Pass qty to remove fewer than all.",
		Params: []Param{
			{"item_id", "string", "menu item id in cart", true},
			{"qty", "integer", "quantity to remove (default all)", false},
		},
		Run: func(ctx context.Context, s *session.Session, args json.RawMessage) (any, error) {
			var a removeArgs
			if err := decode(args, &a); err != nil {
				return nil, err
			}
			s.Mu.Lock()
			defer s.Mu.Unlock()
			if err := s.Cart.Remove(a.ItemID, int(a.Qty)); err != nil {
				return map[string]any{"error": err.Error()}, nil
			}
			return cartSummary(s), nil
		},
	}
}

func getCart() Tool {
	return Tool{
		Name:        "get_cart",
		Description: "Show the current cart grouped by restaurant, with totals and budget remaining.",
		Params:      nil,
		Run: func(ctx context.Context, s *session.Session, args json.RawMessage) (any, error) {
			s.Mu.Lock()
			defer s.Mu.Unlock()
			return cartSummary(s), nil
		},
	}
}

// ---- set_budget ----

type budgetArgs struct {
	Amount flexInt `json:"amount"`
}

func setBudget() Tool {
	return Tool{
		Name:        "set_budget",
		Description: "Set or change the session spending cap in rupees (the guardrail for the cart). Default is 600.",
		Params:      []Param{{"amount", "integer", "budget in rupees, e.g. 500", true}},
		Run: func(ctx context.Context, s *session.Session, args json.RawMessage) (any, error) {
			var a budgetArgs
			if err := decode(args, &a); err != nil {
				return nil, err
			}
			s.Mu.Lock()
			defer s.Mu.Unlock()
			if err := s.Cart.SetBudget(int(a.Amount)); err != nil {
				return map[string]any{"error": err.Error()}, nil
			}
			return map[string]any{"budget": s.Cart.Budget, "total": s.Cart.Total(),
				"budget_remaining": s.Cart.Budget - s.Cart.Total()}, nil
		},
	}
}

// ---- place_order ----

func placeOrder() Tool {
	return Tool{
		Name: "place_order",
		Description: "Place one order per restaurant in the cart. Assigns a delivery partner and returns order ids with ETA minutes. Only call after the user confirmed. Cart is cleared on success.",
		Params:      nil,
		Run: func(ctx context.Context, s *session.Session, args json.RawMessage) (any, error) {
			s.Mu.Lock()
			defer s.Mu.Unlock()
			if s.Cart.Empty() {
				return map[string]any{"error": "cart is empty — add items first"}, nil
			}
			groups := s.Cart.Snapshot()
			var placed []order.Order
			for _, g := range groups {
				in := order.PlaceInput{RestID: g.RestID, RestName: g.RestName, Subtotal: g.Subtotal}
				for _, l := range g.Lines {
					in.Lines = append(in.Lines, order.PlaceLine{ItemID: l.ItemID, Name: l.Name, Price: l.Price, Qty: l.Qty})
				}
				placed = append(placed, s.Orders.Place(in, s.UserLat, s.UserLng))
			}
			s.Cart.Clear()
			type row struct {
				OrderID   string `json:"order_id"`
				RestName  string `json:"restaurant"`
				Subtotal  int    `json:"subtotal"`
				DEName    string `json:"de_name"`
				State     string `json:"state"`
				ETAMin    int    `json:"eta_min"`
			}
			out := []row{}
			for _, o := range placed {
				out = append(out, row{o.ID, o.RestName, o.Subtotal, o.DEName, o.State, o.ETAMin})
			}
			return map[string]any{"orders": out,
				"hint": "user can follow up with track_order(order_id); UI also live-tracks"}, nil
		},
	}
}

// ---- track_order ----

type trackArgs struct {
	OrderID string `json:"order_id"`
}

func trackOrder() Tool {
	return Tool{
		Name: "track_order",
		Description: "Get live status and remaining ETA seconds of an order. With no order_id, tracks the most recent order.",
		Params:      []Param{{"order_id", "string", "order id like ORD1 (optional)", false}},
		Run: func(ctx context.Context, s *session.Session, args json.RawMessage) (any, error) {
			var a trackArgs
			if err := decode(args, &a); err != nil {
				return nil, err
			}
			all := s.Orders.All()
			if len(all) == 0 {
				return map[string]any{"error": "no orders yet in this session"}, nil
			}
			var o order.Order
			if a.OrderID == "" {
				sort.Slice(all, func(i, j int) bool { return all[i].PlacedAt.After(all[j].PlacedAt) })
				o = all[0]
			} else {
				for _, cand := range all {
					if cand.ID == a.OrderID {
						o = cand
						break
					}
				}
				if o.ID == "" {
					return map[string]any{"error": fmt.Sprintf("unknown order %q", a.OrderID)}, nil
				}
			}
			return map[string]any{
				"order_id": o.ID, "restaurant": o.RestName, "state": o.State,
				"de_name": o.DEName, "eta_min": o.ETAMin,
				"eta_seconds_remaining": o.ETASecondsRemaining(),
				"subtotal":              o.Subtotal,
			}, nil
		},
	}
}
