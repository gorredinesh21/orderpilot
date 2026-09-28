// Package cart implements the multi-restaurant cart and the budget
// guardrail. The guardrail is enforced here — in the tool layer, not in the
// prompt — so an agent (or a buggy model) physically cannot overspend.
package cart

import (
	"errors"
	"fmt"
	"sort"

	"github.com/gorredinesh21/orderpilot/internal/data"
)

var (
	// ErrOverBudget is returned when an add would exceed the session budget.
	ErrOverBudget = errors.New("over budget")
	// ErrNotFound is returned for unknown items or empty-cart operations.
	ErrNotFound = errors.New("not found")
)

// Line is one dish and quantity inside a restaurant group.
type Line struct {
	ItemID   string `json:"item_id"`
	Name     string `json:"name"`
	RestID   string `json:"restaurant_id"`
	RestName string `json:"restaurant_name"`
	Price    int    `json:"unit_price"`
	Qty      int    `json:"qty"`
	Veg      bool   `json:"veg"`
}

// Group is the cart slice for one restaurant. Orders are placed per group —
// the marketplace reality: one order per restaurant.
type Group struct {
	RestID   string `json:"restaurant_id"`
	RestName string `json:"restaurant_name"`
	Area     string `json:"area"`
	Lines    []Line `json:"lines"`
	Subtotal int    `json:"subtotal"`
}

// Cart is the session cart. Not safe for concurrent use on its own; callers
// hold the session lock.
type Cart struct {
	Budget int
	lines  map[string]Line // item_id -> line
}

// New returns an empty cart with the given budget (default 600 if <= 0).
func New(budget int) *Cart {
	if budget <= 0 {
		budget = 600
	}
	return &Cart{Budget: budget, lines: map[string]Line{}}
}

// Add inserts or increments an item, enforcing the budget guardrail.
func (c *Cart) Add(it data.MenuItem, qty int) error {
	if qty <= 0 {
		qty = 1
	}
	cur := c.Total()
	existing := c.lines[it.ID]
	if cur+it.Price*qty > c.Budget {
		return fmt.Errorf("%w: adding %d x %s (₹%d) would make the cart ₹%d, budget is ₹%d — remove something or raise the budget with set_budget",
			ErrOverBudget, qty, it.Name, it.Price*qty, cur+it.Price*qty, c.Budget)
	}
	l := existing
	l.ItemID = it.ID
	l.Name = it.Name
	l.RestID = it.RestID
	l.RestName = it.RestName
	l.Price = it.Price
	l.Veg = it.Veg
	l.Qty += qty
	c.lines[it.ID] = l
	return nil
}

// Remove decrements (qty<=1 removes) or errors if absent.
func (c *Cart) Remove(itemID string, qty int) error {
	l, ok := c.lines[itemID]
	if !ok {
		return fmt.Errorf("%w: item %s is not in the cart", ErrNotFound, itemID)
	}
	if qty <= 0 || qty >= l.Qty {
		delete(c.lines, itemID)
		return nil
	}
	l.Qty -= qty
	c.lines[itemID] = l
	return nil
}

// Clear empties the cart.
func (c *Cart) Clear() { c.lines = map[string]Line{} }

// Total is the cart grand total in rupees.
func (c *Cart) Total() int {
	t := 0
	for _, l := range c.lines {
		t += l.Price * l.Qty
	}
	return t
}

// Count is the total number of individual items (qty summed).
func (c *Cart) Count() int {
	n := 0
	for _, l := range c.lines {
		n += l.Qty
	}
	return n
}

// Empty reports whether the cart has no lines.
func (c *Cart) Empty() bool { return len(c.lines) == 0 }

// SetBudget updates the budget. Lowering below the current total is allowed
// but blocks further adds.
func (c *Cart) SetBudget(amount int) error {
	if amount <= 0 {
		return fmt.Errorf("budget must be positive, got %d", amount)
	}
	c.Budget = amount
	return nil
}

// Snapshot returns grouped cart state for APIs and tools.
func (c *Cart) Snapshot() []Group {
	groups := map[string]*Group{}
	for _, l := range c.lines {
		g, ok := groups[l.RestID]
		if !ok {
			g = &Group{RestID: l.RestID, RestName: l.RestName}
			groups[l.RestID] = g
		}
		g.Lines = append(g.Lines, l)
	}
	out := make([]Group, 0, len(groups))
	for _, g := range groups {
		for _, l := range g.Lines {
			g.Subtotal += l.Price * l.Qty
		}
		sort.Slice(g.Lines, func(i, j int) bool { return g.Lines[i].Name < g.Lines[j].Name })
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RestName < out[j].RestName })
	// fill area from corpus
	for i := range out {
		if r, ok := data.Get().RestByID(out[i].RestID); ok {
			out[i].Area = r.Area
		}
	}
	return out
}
