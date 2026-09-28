// Package server wires HTTP handlers: static site, SSE chat stream,
// session snapshots and health.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorredinesh21/orderpilot/internal/agent"
	"github.com/gorredinesh21/orderpilot/internal/data"
	"github.com/gorredinesh21/orderpilot/internal/session"
	"github.com/gorredinesh21/orderpilot/web"
)

// Server holds shared state for all handlers.
type Server struct {
	Store    *session.Store
	Agent    *agent.Agent
	Log      *slog.Logger
	Live     bool // whether an LLM key is configured
	busy     map[string]chan struct{} // one agent turn per session at a time
	busyMu   sync.Mutex
}

// New builds the server.
func New(st *session.Store, ag *agent.Agent, live bool, log *slog.Logger) *Server {
	return &Server{Store: st, Agent: ag, Log: log, Live: live, busy: map[string]chan struct{}{}}
}

// Handler returns the full route mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(web.Assets, "files")
	mux.Handle("/", http.FileServer(http.FS(static)))

	mux.HandleFunc("GET /healthz", s.health) // internal/startup probes
	mux.HandleFunc("GET /live", s.health)    // public: GFE intercepts /healthz on ingress
	mux.HandleFunc("GET /api/restaurants", s.restaurants)
	mux.HandleFunc("POST /api/chat", s.chat)
	mux.HandleFunc("GET /api/session/{id}", s.snapshot)
	mux.HandleFunc("POST /api/budget", s.budget)

	return s.requestLog(mux)
}

func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/healthz" {
			s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "ms", time.Since(start).Milliseconds())
		}
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"sessions": s.Store.Count(),
		"llm":      map[string]bool{"live": s.Live},
		"time":     time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) restaurants(w http.ResponseWriter, r *http.Request) {
	rs := data.Get().SearchRestaurants("", "", 0, 0, false)
	if len(rs) > 12 {
		rs = rs[:12]
	}
	writeJSON(w, http.StatusOK, map[string]any{"restaurants": rs, "total": len(data.Get().Restaurants)})
}

type chatRequest struct {
	SessionID string `json:"session_id"`
	Area      string `json:"area"`
	Message   string `json:"message"`
}

// chat runs one agent turn and streams events over SSE.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if len(req.Message) > 2000 {
		http.Error(w, "message too long", http.StatusRequestEntityTooLarge)
		return
	}
	if req.Message == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}
	sess := s.Store.Get(req.SessionID, req.Area)

	// one turn at a time per session
	if !s.lockSession(sess.ID) {
		http.Error(w, "a previous turn is still running for this session", http.StatusConflict)
		return
	}
	defer s.unlockSession(sess.ID)

	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// SSE writes must be serialized: parallel tool goroutines emit events
	// concurrently, but http.ResponseWriter is not safe for concurrent use.
	var wmu sync.Mutex
	s.Agent.Run(ctx, sess, req.Message, func(ev agent.Event) {
		b, _ := json.Marshal(ev)
		wmu.Lock()
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
		wmu.Unlock()
	})
	// final snapshot so the UI can sync cart/orders without polling
	snap, _ := json.Marshal(s.snapshotPayload(sess))
	wmu.Lock()
	fmt.Fprintf(w, "data: %s\n\n", snap)
	fl.Flush()
	wmu.Unlock()
}

func (s *Server) lockSession(id string) bool {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	if _, busy := s.busy[id]; busy {
		return false
	}
	s.busy[id] = make(chan struct{})
	return true
}

func (s *Server) unlockSession(id string) {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	if ch, ok := s.busy[id]; ok {
		close(ch)
		delete(s.busy, id)
	}
}

type snapshotPayload struct {
	Type    string `json:"type"`
	Cart    any    `json:"cart"`
	Orders  any    `json:"orders"`
	Budget  int    `json:"budget"`
	Area    string `json:"area"`
}

func (s *Server) snapshotPayload(sess *session.Session) snapshotPayload {
	sess.Mu.Lock()
	defer sess.Mu.Unlock()
	return snapshotPayload{
		Type:   "snapshot",
		Cart:   sess.Cart.Snapshot(),
		Orders: orderSnapshots(sess),
		Budget: sess.Cart.Budget,
		Area:   sess.Area,
	}
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.Store.Get(id, "")
	sess.Mu.Lock()
	defer sess.Mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"cart":    sess.Cart.Snapshot(),
		"orders":  orderSnapshots(sess),
		"budget":  sess.Cart.Budget,
		"area":    sess.Area,
		"user_id": sess.ID,
	})
}

func orderSnapshots(sess *session.Session) []map[string]any {
	var out []map[string]any
	for _, o := range sess.Orders.All() {
		out = append(out, map[string]any{
			"order_id": o.ID, "restaurant": o.RestName, "state": o.State,
			"de_name": o.DEName, "eta_min": o.ETAMin, "subtotal": o.Subtotal,
			"eta_seconds_remaining": o.ETASecondsRemaining(),
			"lines":                 o.Lines,
			"placed_at":             o.PlacedAt.Format(time.RFC3339),
		})
	}
	return out
}

type budgetRequest struct {
	SessionID string `json:"session_id"`
	Amount    int    `json:"amount"`
}

func (s *Server) budget(w http.ResponseWriter, r *http.Request) {
	var req budgetRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.Amount < 50 || req.Amount > 20000 {
		http.Error(w, "amount must be 50-20000", http.StatusBadRequest)
		return
	}
	sess := s.Store.Get(req.SessionID, "")
	sess.Mu.Lock()
	defer sess.Mu.Unlock()
	if err := sess.Cart.SetBudget(req.Amount); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"budget": sess.Cart.Budget, "total": sess.Cart.Total()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
