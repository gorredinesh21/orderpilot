# OrderPilot — Agentic Food-Ordering Copilot in Go

**Live:** https://orderpilot-yzzxrxetcq-uc.a.run.app

OrderPilot is an AI agent that orders food end-to-end: it understands a craving in plain
English, searches restaurants and dishes, builds a **multi-restaurant cart under a hard
budget guardrail**, places one order per restaurant, and tracks live simulated deliveries
(delivery partner, ETA engine, status timeline). Every step streams to the UI so you can
*watch the agent think*.

Built as a production-style Go service: framework-free tool-calling loop, typed tools,
lenient argument coercion for small models, deterministic fallback planner, SSE streaming,
graceful shutdown, embedded frontend, distroless container.

```
User: "spicy Andhra food for 2 under 500 in Koramangala, and something sweet after"
  └─ agent loop (Go) ──► LLM replies strict JSON tool-calls
       ├─ search_restaurants(cuisine=andhra, area=Koramangala)   ┐ run concurrently
       ├─ search_dishes(query=gulab jamun)                       ┘ (errgroup)
       ├─ get_menu(r01) → add_to_cart(×2) … budget guardrail enforces ₹500
       ├─ place_order → ORD1 (one per restaurant), DE assigned, ETA promised
       └─ SSE: say / tool_start / tool_result events stream to the browser live
```

## Why this exists

Food-delivery platforms are moving to **agentic ordering** (MCP-style tool surfaces where
an AI assistant searches, builds carts and places orders). OrderPilot is a from-scratch
Go implementation of exactly that pattern — the domain, the concurrency and the failure
modes a backend engineer actually faces.

## Architecture

```
main.go                  flag/env config, signal-driven graceful shutdown
internal/agent/          the loop: LLM ⇄ strict-JSON tools, parallel execution,
                         protocol-repair retry, deterministic fallback planner
internal/tools/          typed tool registry (9 tools) + lenient arg coercion
internal/cart/           multi-restaurant cart + budget guardrail (ErrOverBudget)
internal/order/          order state machine, delivery-partner sim, ETA engine
                         (haversine + traffic factor), time-compressed timelines
internal/data/           synthetic Bangalore corpus: 36 restaurants, ~800 dishes
internal/session/        per-conversation state + idle-session janitor
internal/server/         HTTP: SSE chat stream, snapshots, healthz
web/                     vanilla-JS UI (embedded via embed.FS — one binary)
```

### The agent loop (no framework)

The system prompt declares the tools; the model must answer with a single JSON object —
`{"say": "...", "tools": [{"name": "...", "args": {...}}]}` to act, or `{"reply": "..."}`
to finish. Go:

1. parses leniently (markdown fences, stray prose tolerated), repairs once on bad JSON;
2. executes the requested tools — **multiple independent calls run concurrently** via
   `errgroup`; cart-mutating tools serialize on the session lock;
3. feeds results back as bounded JSON turns until the model replies or the step limit hits;
4. degrades to a **deterministic regex-intent planner** if the LLM is unreachable or keeps
   breaking protocol — the product keeps working with zero LLM dependency.

Small-model hardening learned the hard way: Llama-3.1-8B emits `"200"` for numbers and
`"false"` for booleans, so every numeric/bool tool argument decodes through coercion types
(`internal/tools/tools.go`: `flexInt`, `flexBool`) instead of strict unmarshalling.

### The budget guardrail

Enforced in the **cart, not the prompt** — `cart.Add` rejects any add that would cross the
session budget and returns the reason as tool data. The model sees the error and adapts
(remove an item / ask to raise the budget). An agent physically cannot overspend.

### Order simulation

Placement precomputes a state timeline (`PLACED → CONFIRMED → PREPARING → PICKED_UP →
OUT_FOR_DELIVERY → DELIVERED`) from realistic ETA math (kitchen prep + haversine distance
at ~20 km/h with a traffic factor), then plays it back time-compressed (1 s ≈ 30 s) so a
full order lives out in about a minute. Each order runs on its own goroutine; snapshots
are lock-copy safe.

## Run

```bash
# LLM live (HuggingFace router, OpenAI-compatible):
export HF_TOKEN=hf_...
go run .                       # http://localhost:8080

# no token → every turn uses the deterministic fallback planner:
go run .
```

```bash
make test    # unit tests: agent loop (mock LLM), guardrail, ETA, tools, corpus
make vet
make docker  # distroless image (~20 MB)
```

## API

| Route | What |
|---|---|
| `POST /api/chat` | `{session_id, message}` → SSE stream of agent events + final snapshot |
| `GET /api/session/{id}` | cart, orders, budget snapshot |
| `POST /api/budget` | `{session_id, amount}` set the guardrail |
| `GET /api/restaurants` | browse top restaurants |
| `GET /healthz` | liveness + LLM mode |

## Interview talking points

- **Why no agent framework?** The loop is 150 lines; owning it means owning the failure
  modes — protocol repair, bounded tool output, step limits, and a deterministic fallback
  that frameworks hide.
- **Where's the concurrency?** errgroup fan-out for batched tool calls, one goroutine per
  live order, per-session turn serialization (one agent turn per session, HTTP 409 otherwise),
  mutex-guarded SSE writes (parallel emitters, one ResponseWriter).
- **What breaks at scale?** In-memory sessions/orders → Redis/Postgres; corpus → Postgres +
  OpenSearch; the LLM call → provider pool with circuit breaking. The design keeps those
  seams (Client interface, Tracker, Store) swappable.

## Stack

Go 1.27 · net/http + embed.FS (no web framework) · golang.org/x/sync/errgroup ·
Llama-3.1-8B-Instruct via OpenAI-compatible endpoint · vanilla JS frontend ·
Google Cloud Run (distroless).

---
Built by [Dinesh Gorre](https://github.com/gorredinesh21) · see also
[MenuMind](https://github.com/gorredinesh21/menumind) — Go + GenAI semantic menu search & RAG.
