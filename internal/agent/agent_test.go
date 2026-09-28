package agent

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/gorredinesh21/orderpilot/internal/cart"
	"github.com/gorredinesh21/orderpilot/internal/order"
	"github.com/gorredinesh21/orderpilot/internal/session"
	"github.com/gorredinesh21/orderpilot/internal/tools"
)

func testSession() *session.Session {
	s := &session.Session{ID: "at1", Area: "Koramangala", UserLat: 12.9345, UserLng: 77.6266}
	s.Cart = cart.New(0)
	s.Orders = order.NewTracker(nil)
	return s
}

var itemIDRe = regexp.MustCompile(`"item_id":"([^"]+)"`)

// scriptedClient drives the full protocol: search, then add what was found,
// then reply — using only what tool results put in the conversation.
type scriptedClient struct {
	mu   sync.Mutex
	step int
}

func (c *scriptedClient) Chat(_ context.Context, system string, msgs []Msg) (string, error) {
	c.mu.Lock()
	c.step++
	defer c.mu.Unlock()
	last := ""
	if len(msgs) > 0 {
		last = msgs[len(msgs)-1].Content
	}
	switch {
	case strings.Contains(last, "TOOL RESULT search_dishes") && c.step >= 2:
		m := itemIDRe.FindStringSubmatch(last)
		if m == nil {
			return `{"reply":"no dish found"}`, nil
		}
		return `{"tools":[{"name":"add_to_cart","args":{"item_id":"` + m[1] + `","qty":2}}]}`, nil
	case strings.Contains(last, "TOOL RESULT add_to_cart"):
		return `{"reply":"Added two of the top dosa for you."}`, nil
	default:
		return `{"say":"Searching fresh dosas…","tools":[{"name":"search_dishes","args":{"query":"dosa","veg_only":true}}]}`, nil
	}
}

func TestAgentLoopPlansToolsAndFinishes(t *testing.T) {
	s := testSession()
	ag := New(&scriptedClient{}, tools.New())

	var mu sync.Mutex
	var events []Event
	emit := func(ev Event) { mu.Lock(); events = append(events, ev); mu.Unlock() }

	ag.Run(context.Background(), s, "two dosas please", emit)

	var sawTool, sawSay, sawDone bool
	var firstTool string
	for _, ev := range events {
		switch ev.Type {
		case EvToolStart:
			sawTool = true
			if firstTool == "" {
				firstTool = ev.Tool
			}
		case EvSay:
			sawSay = true
		case EvDone:
			sawDone = true
		}
	}
	if !sawTool || !sawSay || !sawDone {
		t.Fatalf("missing events: tool=%v say=%v done=%v", sawTool, sawSay, sawDone)
	}
	if firstTool != "search_dishes" {
		t.Errorf("first tool = %s, want search_dishes", firstTool)
	}
	s.Mu.Lock()
	count := s.Cart.Count()
	s.Mu.Unlock()
	if count != 2 {
		t.Fatalf("cart count = %d, want 2 (agent must actually execute tools)", count)
	}
}

func TestAgentParallelToolCalls(t *testing.T) {
	// model batches two independent searches in one turn
	client := &MockClient{Fn: func(system string, msgs []Msg) (string, error) {
		last := msgs[len(msgs)-1].Content
		if strings.Contains(last, "TOOL RESULT") {
			return `{"reply":"done"}`, nil
		}
		return `{"tools":[{"name":"search_dishes","args":{"query":"dosa"}},{"name":"search_restaurants","args":{"cuisine":"biryani"}}]}`, nil
	}}
	s := testSession()
	ag := New(client, tools.New())
	var mu sync.Mutex
	var starts []string
	ag.Run(context.Background(), s, "compare dosa and biryani", func(ev Event) {
		if ev.Type == EvToolStart {
			mu.Lock()
			starts = append(starts, ev.Tool)
			mu.Unlock()
		}
	})
	if len(starts) < 2 {
		t.Fatalf("expected 2 tool starts, got %v", starts)
	}
}

func TestAgentFallsBackWhenLLMFails(t *testing.T) {
	client := &MockClient{Fn: func(system string, msgs []Msg) (string, error) {
		return "", context.DeadlineExceeded
	}}
	s := testSession()
	ag := New(client, tools.New())
	var mu sync.Mutex
	var modes []string
	var says []string
	ag.Run(context.Background(), s, "biryani under 400 in Koramangala", func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		if ev.Type == EvMode {
			modes = append(modes, ev.Mode)
		}
		if ev.Type == EvSay {
			says = append(says, ev.Text)
		}
	})
	if len(modes) == 0 || modes[0] != "fallback" {
		t.Fatalf("expected fallback mode, got %v", modes)
	}
	s.Mu.Lock()
	count := s.Cart.Count()
	s.Mu.Unlock()
	if count == 0 {
		t.Fatal("fallback planner should have added dishes to the cart")
	}
}

func TestAgentRecoversFromMalformedJSONOnce(t *testing.T) {
	bad := true
	client := &MockClient{Fn: func(system string, msgs []Msg) (string, error) {
		if bad {
			bad = false
			return "Sure! Let me help with that. (no json)", nil // protocol violation
		}
		if strings.Contains(msgs[len(msgs)-1].Content, "INVALID") {
			return "```json\n{\"reply\":\"recovered\"}\n```", nil // fenced but valid
		}
		return `{"reply":"recovered"}`, nil
	}}
	s := testSession()
	ag := New(client, tools.New())
	var done bool
	ag.Run(context.Background(), s, "hello", func(ev Event) { if ev.Type == EvDone { done = true } })
	if !done {
		t.Fatal("turn must complete after corrective retry")
	}
}

func TestParseTurn(t *testing.T) {
	tests := []struct {
		name, raw string
		wantOK    bool
		wantTools int
		wantReply string
	}{
		{"clean tools", `{"tools":[{"name":"x","args":{}}]}`, true, 1, ""},
		{"fenced reply", "```json\n{\"reply\":\"hi\"}\n```", true, 0, "hi"},
		{"prose around json", `Here you go: {"reply":"hi"} hope that helps`, true, 0, "hi"},
		{"say and tools", `{"say":"looking","tools":[{"name":"a","args":{}},{"name":"b","args":{}}]}`, true, 2, ""},
		{"garbage", "I would recommend paneer.", false, 0, ""},
		{"empty object", `{}`, false, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseTurn(tt.raw)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok {
				if len(got.Tools) != tt.wantTools {
					t.Errorf("tools = %d, want %d", len(got.Tools), tt.wantTools)
				}
				if got.Reply != tt.wantReply {
					t.Errorf("reply = %q, want %q", got.Reply, tt.wantReply)
				}
			}
		})
	}
}

func TestFallbackIntentParsing(t *testing.T) {
	tests := []struct {
		msg       string
		wantAdds  bool
		wantPlace bool
	}{
		{"spicy biryani under 400", true, false},
		{"2 dosa and filter coffee in Jayanagar", true, false},
		{"something sweet, veg, under 250", true, false},
		{"place the order", false, true},
		// combined intent: food + placement in one message must add FIRST,
		// then place (regression: used to place an empty cart)
		{"one ghee roast dosa and a filter coffee please, then place the order", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			s := testSession()
			if tt.wantPlace && !tt.wantAdds {
				seedCart(t, s)
			}
			var says []string
			RunFallback(s, tt.msg, func(ev Event) {
				if ev.Type == EvSay {
					says = append(says, ev.Text)
				}
			})
			if len(says) == 0 {
				t.Fatal("fallback must say something")
			}
			s.Mu.Lock()
			count, empty := s.Cart.Count(), s.Cart.Empty()
			orders := len(s.Orders.All())
			s.Mu.Unlock()
			if tt.wantAdds && !tt.wantPlace && count == 0 {
				t.Errorf("fallback should have added items for %q (said: %v)", tt.msg, says)
			}
			if tt.wantPlace && (orders == 0 || !empty) {
				t.Errorf("fallback should have placed order for %q: orders=%d empty=%v said=%v", tt.msg, orders, empty, says)
			}
		})
	}
}

func seedCart(t *testing.T, s *session.Session) {
	t.Helper()
	reg := tools.New()
	res, err := exec(reg, s, "search_dishes", `{"query":"dosa"}`)
	if err != nil {
		t.Fatal(err)
	}
	m := itemIDRe.FindStringSubmatch(mustJSON(res))
	if m == nil {
		t.Fatal("no dish to seed")
	}
	if _, err := exec(reg, s, "add_to_cart", `{"item_id":"`+m[1]+`","qty":1}`); err != nil {
		t.Fatal(err)
	}
}

func exec(reg *tools.Registry, s *session.Session, name, args string) (any, error) {
	tool, _ := reg.Get(name)
	return tool.Run(context.Background(), s, json.RawMessage(args))
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
