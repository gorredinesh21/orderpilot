// OrderPilot — agentic food-ordering copilot in Go.
//
// A framework-free agent loop: the LLM plans via strict JSON tool-calls and
// Go executes typed tools (search, cart with budget guardrail, orders, live
// tracking). Runs as a single binary serving the embedded web app.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/gorredinesh21/orderpilot/internal/agent"
	"github.com/gorredinesh21/orderpilot/internal/server"
	"github.com/gorredinesh21/orderpilot/internal/session"
	"github.com/gorredinesh21/orderpilot/internal/tools"
)

func main() {
	addr := flag.String("addr", envOr("PORT", "8080"), "listen address (PORT also works, : prefix optional)")
	hfKey := flag.String("hf-token", os.Getenv("HF_TOKEN"), "HuggingFace API token; empty = deterministic fallback mode")
	hfModel := flag.String("hf-model", envOr("HF_LLM_MODEL", "meta-llama/Llama-3.1-8B-Instruct"), "chat model id")
	llmBase := flag.String("llm-base", envOr("LLM_BASE_URL", "https://router.huggingface.co"), "OpenAI-compatible chat base URL")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	var llm agent.Client
	live := *hfKey != ""
	if live {
		llm = &agent.HFClient{BaseURL: *llmBase, APIKey: *hfKey, Model: *hfModel}
	} else {
		// no key configured: every turn degrades to the deterministic planner
		llm = failingClient{}
	}

	store := session.NewStore(log, 30*time.Minute)
	ag := agent.New(llm, tools.New())
	srv := server.New(store, ag, live, log)

	httpSrv := &http.Server{
		Addr:              ":" + trimColon(*addr),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		log.Info("orderpilot listening", "addr", httpSrv.Addr, "llm_live", live, "model", *hfModel)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("server error", "err", err)
			stop()
		}
	}()
	<-ctx.Done()
	log.Info("shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shCtx)
	store.Shutdown()
	log.Info("bye")
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func trimColon(s string) string {
	if len(s) > 0 && s[0] == ':' {
		return s[1:]
	}
	return s
}

// failingClient always errors, which makes Agent.Run use the fallback planner.
type failingClient struct{}

func (failingClient) Chat(ctx context.Context, system string, msgs []agent.Msg) (string, error) {
	return "", context.DeadlineExceeded
}
