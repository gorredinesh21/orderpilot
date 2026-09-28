// Package order implements the order lifecycle: a time-driven state machine
// with a simulated delivery partner (DE), an ETA engine, and per-order
// background goroutines that advance status so /track reflects a live order.
package order

import (
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/gorredinesh21/orderpilot/internal/data"
)

// States an order moves through.
const (
	StatePlaced         = "PLACED"
	StateConfirmed      = "CONFIRMED"
	StatePreparing      = "PREPARING"
	StatePickedUp       = "PICKED_UP"
	StateOutForDelivery = "OUT_FOR_DELIVERY"
	StateDelivered      = "DELIVERED"
	StateCancelled      = "CANCELLED"
)

// AllStates is the happy-path order of states (used by the UI stepper).
var AllStates = []string{StatePlaced, StateConfirmed, StatePreparing, StatePickedUp, StateOutForDelivery, StateDelivered}

// DEs is the delivery-partner name pool for the simulation.
var DEs = []string{"Ravi K.", "Suresh N.", "Manju P.", "Imran S.", "Lakshmi D.", "Vinay G.", "Prakash M.", "Farhan A."}

// Line is one item in a placed order (immutable snapshot).
type Line struct {
	ItemID string `json:"item_id"`
	Name   string `json:"name"`
	Price  int    `json:"price"`
	Qty    int    `json:"qty"`
}

// Order is the public, JSON-facing view of a placed order. Pure data — the
// runtime state machine lives in the unexported live wrapper.
type Order struct {
	ID         string    `json:"id"`
	RestID     string    `json:"restaurant_id"`
	RestName   string    `json:"restaurant_name"`
	Area       string    `json:"area"`
	Lines      []Line    `json:"lines"`
	Subtotal   int       `json:"subtotal"`
	DEName     string    `json:"de_name"`
	State      string    `json:"state"`
	ETAMin     int       `json:"eta_min"` // original promise, realistic minutes
	DistanceKm float64   `json:"distance_km"`
	PlacedAt   time.Time `json:"placed_at"`
	DeliverBy  time.Time `json:"deliver_by"` // wall-clock promise (compressed clock)
}

// PlaceLine is one immutable line of a placed order.
type PlaceLine struct {
	ItemID string
	Name   string
	Price  int
	Qty    int
}

// PlaceInput carries everything needed to place one order per restaurant.
type PlaceInput struct {
	RestID   string
	RestName string
	Subtotal int
	Lines    []PlaceLine
}

// live holds the mutable simulation for one order.
type live struct {
	mu       sync.Mutex
	o        Order
	timeline []transition
	next     int
	cancelCh chan struct{}
	doneCh   chan struct{}
}

type transition struct {
	at    time.Time
	state string
}

// Tracker owns all live orders and runs their simulation goroutines.
type Tracker struct {
	mu     sync.Mutex
	lives  map[string]*live
	seq    int
	deSeq  int
	log    *slog.Logger
}

// NewTracker builds a tracker.
func NewTracker(log *slog.Logger) *Tracker {
	if log == nil {
		log = slog.Default()
	}
	return &Tracker{lives: map[string]*live{}, log: log}
}

// ETA computes kitchen prep + travel minutes for a restaurant→user hop.
func ETA(rest data.Restaurant, userLat, userLng float64) (int, float64) {
	km := data.HaversineKm(rest.Lat, rest.Lng, userLat, userLng)
	// effective door-to-door speed in city traffic ~20 km/h plus a light
	// evening-traffic factor; minimum hop of 1.2 km for pickup slack
	if km < 1.2 {
		km = 1.2
	}
	travelMin := (km / 20.0) * 60 * 1.15
	total := float64(rest.PrepMin) + travelMin
	return int(math.Max(6, math.Round(total))), math.Round(km*10) / 10
}

// Place creates an order and starts its simulation. Time compression:
// 1 wall second = 30 simulated seconds, so a ~25-minute order plays out
// live in under a minute — while ETAs stay realistic numbers.
func (t *Tracker) Place(group PlaceInput, userLat, userLng float64) Order {
	rest, _ := data.Get().RestByID(group.RestID)
	etaMin, km := ETA(rest, userLat, userLng)

	t.mu.Lock()
	t.seq++
	id := fmt.Sprintf("ORD%d", t.seq)
	deName := DEs[t.deSeq%len(DEs)]
	t.deSeq++
	now := time.Now()
	o := Order{
		ID:         id,
		RestID:     rest.ID,
		RestName:   rest.Name,
		Area:       rest.Area,
		Subtotal:   group.Subtotal,
		DEName:     deName,
		State:      StatePlaced,
		ETAMin:     etaMin,
		DistanceKm: km,
		PlacedAt:   now,
	}
	for _, l := range group.Lines {
		o.Lines = append(o.Lines, Line{ItemID: l.ItemID, Name: l.Name, Price: l.Price, Qty: l.Qty})
	}
	sim := time.Duration(etaMin) * time.Minute / 30 // compressed
	if sim < 20*time.Second {
		sim = 20 * time.Second
	}
	// DeliverBy rides the compressed clock so the UI countdown hits zero
	// exactly when the simulation delivers.
	o.DeliverBy = now.Add(sim)
	l := &live{
		o:    o,
		timeline: []transition{
			{now.Add(2 * time.Second), StateConfirmed},
			{now.Add(sim / 5), StatePreparing},
			{now.Add(2 * sim / 5), StatePickedUp},
			{now.Add(2*sim/5 + time.Second), StateOutForDelivery},
			{now.Add(sim), StateDelivered},
		},
		cancelCh: make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	t.lives[id] = l
	t.mu.Unlock()

	go l.run()
	t.log.Info("order placed", "order_id", id, "restaurant", rest.Name, "eta_min", etaMin, "subtotal", group.Subtotal)
	return o
}

// Get returns a snapshot of an order.
func (t *Tracker) Get(id string) (Order, bool) {
	t.mu.Lock()
	l, ok := t.lives[id]
	t.mu.Unlock()
	if !ok {
		return Order{}, false
	}
	return l.snapshot(), true
}

// All returns snapshots of every order.
func (t *Tracker) All() []Order {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Order, 0, len(t.lives))
	for _, l := range t.lives {
		out = append(out, l.snapshot())
	}
	return out
}

// Cancel stops a not-yet-delivered order.
func (t *Tracker) Cancel(id string) bool {
	t.mu.Lock()
	l, ok := t.lives[id]
	t.mu.Unlock()
	if !ok {
		return false
	}
	return l.cancel()
}

// Shutdown cancels all running simulations (graceful server shutdown).
func (t *Tracker) Shutdown() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, l := range t.lives {
		l.cancel()
	}
}

// snapshot copies the order under its lock.
func (l *live) snapshot() Order {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.o
}

// ETASecondsRemaining reports seconds until the promised delivery, clamped at 0.
func (o Order) ETASecondsRemaining() int {
	r := int(time.Until(o.DeliverBy).Seconds())
	if r < 0 {
		return 0
	}
	return r
}

// cancel is the core cancel; only touches the live order's own mutex so it
// is safe to call from inside the tracker lock.
func (l *live) cancel() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.o.State == StateDelivered || l.o.State == StateCancelled {
		return false
	}
	l.o.State = StateCancelled
	close(l.cancelCh)
	return true
}

// run advances the order through its timeline until delivered or cancelled.
func (l *live) run() {
	defer close(l.doneCh)
	timer := time.NewTimer(time.Until(l.timeline[0].at))
	defer timer.Stop()
	for l.next < len(l.timeline) {
		select {
		case <-l.cancelCh:
			return
		case <-timer.C:
			l.mu.Lock()
			l.o.State = l.timeline[l.next].state
			l.next++
			l.mu.Unlock()
			if l.next < len(l.timeline) {
				timer.Reset(time.Until(l.timeline[l.next].at))
			}
		}
	}
}
