package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gorredinesh21/orderpilot/internal/data"
	"github.com/gorredinesh21/orderpilot/internal/session"
	"github.com/gorredinesh21/orderpilot/internal/tools"
)

// cuisineMap maps free-text words to corpus cuisine keys.
var cuisineMap = map[string]string{
	"biryani": "biryani", "briyani": "biryani", "pulao": "biryani",
	"dosa": "southindian", "idli": "southindian", "vada": "southindian",
	"south indian": "southindian", "tiffin": "southindian", "filter coffee": "southindian", "kaapi": "southindian",
	"andhra": "andhra", "gongura": "andhra", "spicy chicken curry": "andhra",
	"north indian": "northindian", "paneer": "northindian", "butter chicken": "northindian",
	"dal": "northindian", "chole": "northindian", "roti": "northindian",
	"chinese": "chinese", "noodles": "chinese", "momos": "chinese", "manchurian": "chinese", "fried rice": "chinese",
	"pizza": "pizza",
	"burger": "burgers", "fries": "burgers",
	"kebab": "kebab", "tikka": "kebab", "tandoori": "kebab",
	"thai": "thai", "pad thai": "thai",
	"seafood": "seafood", "fish": "seafood", "prawn": "seafood", "squid": "seafood",
	"pasta": "continental", "salad": "continental", "continental": "continental",
	"dessert": "desserts", "sweet": "desserts", "gulab jamun": "desserts", "cake": "desserts", "halwa": "desserts",
	"coffee": "beverages", "chai": "beverages", "lassi": "beverages", "shake": "beverages", "juice": "beverages",
}

var budgetRe = regexp.MustCompile(`(?:under|below|within|less than|max|budget of|upto|up to)[^0-9₹]*(\d{2,5})`)

var areaNames = []string{"Koramangala", "Indiranagar", "HSR Layout", "HSR", "Whitefield",
	"Jayanagar", "JP Nagar", "Marathahalli", "Church Street"}

// RunFallback is the deterministic planner: regex intent extraction driving
// the same tool layer the LLM uses. Used when the model is unavailable or
// breaks protocol, so the product keeps functioning.
func RunFallback(s *session.Session, userMsg string, emit func(Event)) {
	msg := strings.ToLower(userMsg)
	reg := tools.New()
	// run executes a tool and returns the result the way the LLM sees it:
	// round-tripped through JSON into generic maps.
	run := func(name string, args map[string]any) map[string]any {
		raw, _ := json.Marshal(args)
		emit(Event{Type: EvToolStart, Tool: name, Args: args})
		tool, ok := reg.Get(name)
		if !ok {
			emit(Event{Type: EvToolResult, Tool: name, Result: map[string]any{"error": "no such tool"}})
			return map[string]any{"error": "no such tool"}
		}
		res, err := tool.Run(context.Background(), s, raw)
		if err != nil {
			res = map[string]any{"error": err.Error()}
		}
		b, _ := json.Marshal(res)
		var generic map[string]any
		if json.Unmarshal(b, &generic) != nil {
			generic = map[string]any{"error": "tool result not serializable"}
		}
		emit(Event{Type: EvToolResult, Tool: name, Result: generic})
		return generic
	}

	// 1) explicit order actions win
	switch {
	case hasAny(msg, []string{"track", "where is my order", "order status"}):
		res := run("track_order", map[string]any{})
		if m := res; m != nil {
			if errTxt, bad := m["error"].(string); bad && errTxt != "" {
				emit(Event{Type: EvSay, Mode: "fallback", Text: "No orders in this session yet."})
				return
			}
			emit(Event{Type: EvSay, Mode: "fallback",
				Text: fmt.Sprintf("Order %v from %v is %v. Delivery partner: %v.",
					m["order_id"], m["restaurant"], m["state"], m["de_name"])})
			return
		}
	case hasAny(msg, []string{"place the order", "place order", "checkout", "confirm the order", "order it", "go ahead and order", "place my order"}):
		res := run("place_order", map[string]any{})
		if m := res; m != nil {
			if _, bad := m["error"]; bad {
				emit(Event{Type: EvSay, Mode: "fallback", Text: "The cart is empty — tell me what you'd like first."})
				return
			}
			orders, _ := m["orders"].([]interface{})
			parts := []string{}
			for _, o := range orders {
				om, _ := o.(map[string]any)
				parts = append(parts, fmt.Sprintf("%v from %v, ETA %v min (₹%v)", om["order_id"], om["restaurant"], om["eta_min"], om["subtotal"]))
			}
			emit(Event{Type: EvSay, Mode: "fallback", Text: "Order placed! " + strings.Join(parts, " | ") + ". Track it live in the Orders panel."})
			return
		}
	case hasAny(msg, []string{"clear the cart", "clear cart", "empty the cart", "start over"}):
		s.Mu.Lock()
		s.Cart.Clear()
		s.Mu.Unlock()
		emit(Event{Type: EvSay, Mode: "fallback", Text: "Cart cleared."})
		return
	}

	// 2) budget
	if m := budgetRe.FindStringSubmatch(userMsg); m != nil {
		if amt, err := strconv.Atoi(m[1]); err == nil && amt >= 50 && amt <= 20000 {
			run("set_budget", map[string]any{"amount": amt})
		}
	}

	// 3) area
	area := ""
	for _, a := range areaNames {
		if strings.Contains(msg, strings.ToLower(a)) {
			if a == "HSR" {
				a = "HSR Layout"
			}
			area = a
			break
		}
	}

	// 4) veg / cuisine / dish
	vegOnly := regexp.MustCompile(`\bveg\b|vegetarian|pure veg`).MatchString(msg)
	cuisine := ""
	for k, v := range cuisineMap {
		if strings.Contains(msg, k) {
			if len(k) > len(cuisine) || (cuisine != "" && v != cuisine && len(k) > 3) {
				cuisine = v
			}
			if cuisine == "" {
				cuisine = v
			}
		}
	}

	// 5) direct dish search when the message names a dish
	dishQuery := ""
	for _, kw := range []string{"biryani", "dosa", "momos", "pizza", "burger", "paneer", "pasta", "salad", "kebab", "tikka", "fried rice", "noodles", "prawn", "fish", "cake", "lassi", "coffee", "chai"} {
		if strings.Contains(msg, kw) {
			dishQuery = kw
			break
		}
	}

	s.Mu.Lock()
	overBudget := s.Cart.Total() > 0
	budget := s.Cart.Budget
	s.Mu.Unlock()

	if overBudget && dishQuery == "" && cuisine == "" {
		s.Mu.Lock()
		summary := map[string]any{
			"total": s.Cart.Total(), "budget": s.Cart.Budget,
			"groups": s.Cart.Snapshot(),
		}
		s.Mu.Unlock()
		emit(Event{Type: EvToolResult, Tool: "get_cart", Result: summary})
		emit(Event{Type: EvSay, Mode: "fallback",
			Text: fmt.Sprintf("Your cart is ₹%d of ₹%d. Say 'place the order' to check out, or tell me what to add or remove.", summary["total"], summary["budget"])})
		return
	}

	// pick strategy: dish search first, else restaurant search
	var chosen data.Restaurant
	var items []data.MenuItem
	if dishQuery != "" {
		args := map[string]any{"query": dishQuery}
		if vegOnly {
			args["veg_only"] = true
		}
		res := run("search_dishes", args)
		if m := res; m != nil {
			if ds, ok := m["dishes"].([]interface{}); ok && len(ds) > 0 {
				// group by restaurant, prefer best-rated dish's restaurant
				counts := map[string]int{}
				best := map[string]float64{}
				first := map[string]data.MenuItem{}
				for _, d := range ds {
					dm, _ := d.(map[string]any)
					it, ok := data.Get().ItemByID(dm["item_id"].(string))
					if !ok {
						continue
					}
					counts[it.RestID]++
					if it.Rating > best[it.RestID] {
						best[it.RestID] = it.Rating
					}
					if _, seen := first[it.RestID]; !seen {
						first[it.RestID] = it
					}
				}
				type cand struct {
					id string
					n  int
					r  float64
				}
				var cs []cand
				for id, n := range counts {
					r, _ := data.Get().RestByID(id)
					if area != "" && r.Area != area {
						continue
					}
					cs = append(cs, cand{id, n, r.Rating})
				}
				if len(cs) == 0 { // ignore area constraint if too strict
					for id, n := range counts {
						cs = append(cs, cand{id, n, 0})
					}
				}
				sort.Slice(cs, func(i, j int) bool {
					if cs[i].n != cs[j].n {
						return cs[i].n > cs[j].n
					}
					return cs[i].r > cs[j].r
				})
				chosen, _ = data.Get().RestByID(cs[0].id)
				for _, it := range data.Get().Menu(chosen.ID) {
					if strings.Contains(strings.ToLower(it.Name+" "+it.Description+" "+it.Category), dishQuery) {
						if vegOnly && !it.Veg {
							continue
						}
						items = append(items, it)
					}
				}
				if len(items) == 0 {
					items = append(items, first[chosen.ID])
				}
			}
		}
	}
	if chosen.ID == "" {
		args := map[string]any{}
		if cuisine != "" {
			args["cuisine"] = cuisine
		}
		if area != "" {
			args["area"] = area
		}
		if vegOnly {
			args["veg_only"] = true
		}
		res := run("search_restaurants", args)
		if m := res; m != nil {
			if rs, ok := m["restaurants"].([]interface{}); ok && len(rs) > 0 {
				rm, _ := rs[0].(map[string]any)
				chosen, _ = data.Get().RestByID(rm["id"].(string))
			}
		}
		if chosen.ID == "" {
			emit(Event{Type: EvSay, Mode: "fallback",
				Text: "I couldn't find a match (offline mode). Try naming a cuisine like biryani, dosa, pizza or momos, plus an area and budget."})
			return
		}
		// pick signature dishes from its menu
		menu := data.Get().Menu(chosen.ID)
		var best []data.MenuItem
		for _, it := range menu {
			if it.Bestseller {
				best = append(best, it)
			}
		}
		if len(best) < 2 {
			best = menu
		}
		sort.Slice(best, func(i, j int) bool { return best[i].Rating > best[j].Rating })
		items = best
	}

	// 6) add up to 3 items within budget
	added := []string{}
	spend := s.Cart.Total()
	for _, it := range items {
		if len(added) >= 3 {
			break
		}
		if spend+it.Price > budget {
			continue
		}
		if vegOnly && !it.Veg {
			continue
		}
		res := run("add_to_cart", map[string]any{"item_id": it.ID, "qty": 1})
		if m := res; m != nil {
			if _, bad := m["error"]; bad {
				continue
			}
		}
		spend += it.Price
		added = append(added, fmt.Sprintf("%s (₹%d)", it.Name, it.Price))
	}
	if len(added) == 0 {
		emit(Event{Type: EvSay, Mode: "fallback",
			Text: fmt.Sprintf("Found %s in %s but nothing fits the ₹%d budget. Raise it and I'll retry.", chosen.Name, chosen.Area, budget)})
		return
	}
	s.Mu.Lock()
	total := s.Cart.Total()
	s.Mu.Unlock()
	emit(Event{Type: EvSay, Mode: "fallback",
		Text: fmt.Sprintf("Offline planner picked %s (%s, ⭐%.1f): added %s. Cart ₹%d of ₹%d — say 'place the order' to check out.",
			chosen.Name, chosen.Area, chosen.Rating, strings.Join(added, ", "), total, budget)})
}

func hasAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}
