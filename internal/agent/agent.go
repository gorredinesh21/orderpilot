package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/gorredinesh21/orderpilot/internal/session"
	"github.com/gorredinesh21/orderpilot/internal/tools"
)

// Event types streamed to the UI over SSE.
const (
	EvMode       = "mode"        // planner mode: llm | fallback
	EvSay        = "say"         // agent message shown in chat
	EvToolStart  = "tool_start"  // tool invocation begins
	EvToolResult = "tool_result" // tool finished (result or error)
	EvDone       = "done"        // turn complete
	EvError      = "error"       // fatal turn error
)

// Event is one streamed agent step.
type Event struct {
	Type   string `json:"type"`
	Mode   string `json:"mode,omitempty"`
	Text   string `json:"text,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Args   any    `json:"args,omitempty"`
	Result any    `json:"result,omitempty"`
}

// Agent runs the plan-execute loop for one user turn.
type Agent struct {
	LLM      Client
	Registry *tools.Registry
	MaxSteps int
}

// New builds an agent over a tool registry. Step budget is generous: small
// instruct models routinely burn turns on empty-search recovery.
func New(llm Client, reg *tools.Registry) *Agent {
	return &Agent{LLM: llm, Registry: reg, MaxSteps: 16}
}

// llmTurn is the protocol the model must follow. Kept deliberately small so
// an 8B model can comply reliably.
type llmTurn struct {
	Say   string `json:"say,omitempty"`
	Reply string `json:"reply,omitempty"`
	Tools []struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"tools,omitempty"`
}

// Run plans and executes one user turn, emitting events as it goes.
func (a *Agent) Run(ctx context.Context, s *session.Session, userMsg string, emit func(Event)) {
	s.Mu.Lock()
	s.History = append(s.History, session.Msg{Role: "user", Content: userMsg})
	if len(s.History) > 20 {
		s.History = s.History[len(s.History)-20:]
	}
	s.Mu.Unlock()

	mode := "llm"
	for step := 0; step < a.MaxSteps; step++ {
		if ctx.Err() != nil {
			emit(Event{Type: EvError, Text: "request cancelled"})
			return
		}
		raw, err := a.LLM.Chat(ctx, a.systemPrompt(s), toAgentMsgs(s))
		if err != nil {
			// LLM unavailable / misbehaving: degrade to the deterministic
			// planner so the product keeps working.
			mode = "fallback"
			emit(Event{Type: EvMode, Mode: mode, Text: "LLM unavailable (" + err.Error() + ") — switching to deterministic planner"})
			RunFallback(s, userMsg, emit)
			emit(Event{Type: EvDone, Mode: mode})
			return
		}
		turn, ok := parseTurn(raw)
		if !ok {
			if step == 0 {
				// one corrective retry before giving up on the model
				s.Mu.Lock()
				s.History = append(s.History, session.Msg{Role: "assistant", Content: raw},
					session.Msg{Role: "user", Content: "INVALID: your response was not a single JSON object following the protocol. Respond only with {\"say\":...,\"tools\":[...]} or {\"reply\":...}. No markdown fences."})
				s.Mu.Unlock()
				continue
			}
			mode = "fallback"
			emit(Event{Type: EvMode, Mode: mode, Text: "model broke the JSON protocol — switching to deterministic planner"})
			RunFallback(s, userMsg, emit)
			emit(Event{Type: EvDone, Mode: mode})
			return
		}

		if turn.Say != "" {
			emit(Event{Type: EvSay, Mode: mode, Text: turn.Say})
		}
		if len(turn.Tools) == 0 && turn.Reply != "" {
			s.Mu.Lock()
			s.History = append(s.History, session.Msg{Role: "assistant", Content: raw})
			s.Mu.Unlock()
			emit(Event{Type: EvDone, Mode: mode})
			return
		}
		if len(turn.Tools) == 0 {
			// model said nothing useful; nudge once
			s.Mu.Lock()
			s.History = append(s.History, session.Msg{Role: "assistant", Content: raw},
				session.Msg{Role: "user", Content: "Continue: call a tool or reply with {\"reply\": ...}."})
			s.Mu.Unlock()
			continue
		}

		results := a.execTools(ctx, s, turn.Tools, emit)
		var sb strings.Builder
		for name, res := range results {
			b, _ := json.Marshal(res)
			trimmed := string(b)
			if len(trimmed) > 1400 {
				trimmed = trimmed[:1400] + "...(truncated)"
			}
			fmt.Fprintf(&sb, "TOOL RESULT %s: %s\n", name, trimmed)
		}
		s.Mu.Lock()
		s.History = append(s.History, session.Msg{Role: "assistant", Content: raw},
			session.Msg{Role: "user", Content: strings.TrimSpace(sb.String())})
		s.Mu.Unlock()
	}

	emit(Event{Type: EvSay, Mode: mode, Text: "I hit my step limit for this turn — ask me to continue or simplify the request."})
	emit(Event{Type: EvDone, Mode: mode})
}

// execTools runs the model's tool calls. Multiple calls execute concurrently
// (bounded by the number of calls) via errgroup; cart-mutating tools
// serialize on the session lock.
func (a *Agent) execTools(ctx context.Context, s *session.Session, calls []struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}, emit func(Event)) map[string]any {

	type outcome struct {
		idx  int
		res  any
		err  error
	}
	outcomes := make([]outcome, len(calls))
	g, gctx := errgroup.WithContext(ctx)
	for i, c := range calls {
		i, c := i, c
		g.Go(func() error {
			emit(Event{Type: EvToolStart, Tool: c.Name, Args: compactArgs(c.Args)})
			tool, ok := a.Registry.Get(c.Name)
			var res any
			var err error
			if !ok {
				err = fmt.Errorf("no such tool")
			} else {
				res, err = tool.Run(gctx, s, c.Args)
			}
			if err != nil {
				res = map[string]any{"error": err.Error()}
			}
			emit(Event{Type: EvToolResult, Tool: c.Name, Result: res})
			outcomes[i] = outcome{idx: i, res: res, err: err}
			return nil // tool errors are data for the model, not loop errors
		})
	}
	_ = g.Wait()

	named := map[string]any{}
	for i, c := range calls {
		key := c.Name
		if len(calls) > 1 {
			key = fmt.Sprintf("%s#%d", c.Name, i+1)
		}
		named[key] = outcomes[i].res
	}
	return named
}

func toAgentMsgs(s *session.Session) []Msg {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	out := make([]Msg, 0, len(s.History))
	for _, m := range s.History {
		out = append(out, Msg{Role: m.Role, Content: m.Content})
	}
	return out
}

// parseTurn extracts the JSON object from a model response, tolerating
// markdown fences and stray prose around it.
func parseTurn(raw string) (llmTurn, bool) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return llmTurn{}, false
	}
	var t llmTurn
	if err := json.Unmarshal([]byte(raw[start:end+1]), &t); err != nil {
		return llmTurn{}, false
	}
	if t.Reply == "" && t.Say == "" && len(t.Tools) == 0 {
		return llmTurn{}, false
	}
	// normalize unknown tool names early so the model sees the error
	return t, true
}

func compactArgs(raw json.RawMessage) any {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return map[string]any{"_raw": string(raw)}
	}
	return v
}

// systemPrompt builds the standing instruction, including live session
// context so the model always knows cart/budget/orders state.
func (a *Agent) systemPrompt(s *session.Session) string {
	s.Mu.Lock()
	cartTotal, budget, itemCount := s.Cart.Total(), s.Cart.Budget, s.Cart.Count()
	var orderBits []string
	for _, o := range s.Orders.All() {
		orderBits = append(orderBits, fmt.Sprintf("%s@%s:%s", o.ID, o.RestName, o.State))
	}
	s.Mu.Unlock()

	var b strings.Builder
	b.WriteString(`You are OrderPilot, an agent that orders food for the user in Bangalore.

PROTOCOL — CRITICAL:
You respond with ONE JSON object and nothing else. No markdown fences, no prose outside JSON. Two forms:
1. To act: {"say": "short update for the user (optional)", "tools": [{"name": "<tool>", "args": { ... }}]}
2. When done: {"reply": "final message for the user"}
You may batch MULTIPLE independent tool calls in one response (e.g. two searches) — they run concurrently. Never batch dependent calls (search then get_menu must be separate turns since you need the id).

TOOLS:
`)
	b.WriteString(a.Registry.Spec())
	fmt.Fprintf(&b, `SESSION CONTEXT:
- User location: %s (distance-based ETAs apply)
- Budget: ₹%d, cart total: ₹%d (%d items)
- Recent orders: %s

RULES:
- Prices are rupees (₹). One order per restaurant; a multi-restaurant cart becomes multiple orders — mention this.
- max_price_for_two on search_restaurants is the RESTAURANT's cost-for-two, not the user's budget. Only set it when the user explicitly wants cheap/premium places. For dish budgets ("under 300"), use set_budget and search_dishes with max_price instead.
- Never invent item_id/restaurant_id/order_id values; only use ones from tool results.
- If a search returns 0 matches, broaden: drop filters one at a time (area first, then price), or switch to search_dishes.
- If a tool result contains "error", adapt: fix args, remove an item, or suggest raising the budget. Do not repeat a failed call unchanged.
- Before place_order, confirm the cart with the user UNLESS they already clearly told you to order/checkout.
- Only say an order was "placed" AFTER place_order returns orders. Until then, say "ready to place".
- Keep "say" under 25 words; don't apologize repeatedly — just fix and continue. In the final "reply", summarize what was ordered/planned with prices.
- Do not mention tools, JSON or protocols to the user.

`, s.Area, budget, cartTotal, itemCount, strings.Join(orderBits, ", "))
	return b.String()
}
