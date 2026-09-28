// Package session holds per-conversation state: cart, orders, LLM history
// and the user's simulated delivery location. A janitor goroutine evicts
// idle sessions so memory stays bounded.
package session

import (
	"log/slog"
	"sync"
	"time"

	"github.com/gorredinesh21/orderpilot/internal/cart"
	"github.com/gorredinesh21/orderpilot/internal/data"
	"github.com/gorredinesh21/orderpilot/internal/order"
)

// Msg is one conversation turn sent to the LLM.
type Msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Session is one user conversation. Mu guards Cart and History; tools acquire
// it as needed so parallel tool calls stay safe.
type Session struct {
	ID      string
	Mu      sync.Mutex
	Cart    *cart.Cart
	Orders  *order.Tracker
	History []Msg
	UserLat float64
	UserLng float64
	Area    string

	lastSeen time.Time
}

// Touch updates the idle timestamp.
func (s *Session) Touch() { s.lastSeen = time.Now() }

// Store is the in-memory session map.
type Store struct {
	mu       sync.Mutex
	sessions map[string]*Session
	log      *slog.Logger
}

// NewStore builds a session store and starts the janitor.
func NewStore(log *slog.Logger, ttl time.Duration) *Store {
	if log == nil {
		log = slog.Default()
	}
	st := &Store{sessions: map[string]*Session{}, log: log}
	go st.janitor(ttl)
	return st
}

// Get returns an existing session or creates one anchored at the given area.
func (st *Store) Get(id, area string) *Session {
	if area == "" {
		area = "Koramangala"
	}
	if a, ok := data.Areas[area]; ok {
		area = a.Name
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if s, ok := st.sessions[id]; ok {
		s.Touch()
		return s
	}
	loc := data.Areas[area]
	s := &Session{
		ID:      id,
		Cart:    cart.New(0),
		Orders:  order.NewTracker(st.log),
		UserLat: loc.Lat,
		UserLng: loc.Lng,
		Area:    loc.Name,
	}
	s.Touch()
	st.sessions[id] = s
	st.log.Info("session created", "session_id", id, "area", area)
	return s
}

// Count reports live sessions (for /healthz).
func (st *Store) Count() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.sessions)
}

func (st *Store) janitor(ttl time.Duration) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		st.mu.Lock()
		for id, s := range st.sessions {
			if time.Since(s.lastSeen) > ttl {
				s.Orders.Shutdown()
				delete(st.sessions, id)
				st.log.Info("session evicted", "session_id", id)
			}
		}
		st.mu.Unlock()
	}
}

// Shutdown cancels all order simulations (graceful server shutdown).
func (st *Store) Shutdown() {
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, s := range st.sessions {
		s.Orders.Shutdown()
		st.log.Info("session shutdown", "session_id", id)
	}
}
