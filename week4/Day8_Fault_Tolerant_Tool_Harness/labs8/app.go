// app.go — підкоманди worker/start/counter (п.3-6): усе, що веде до живого
// Temporal/Dgraph прогону. Конфігурація — з env, як описано в Homework.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/googleadk"
	"go.temporal.io/sdk/worker"

	"google.golang.org/adk/v2/model"

	"github.com/dimetron/ai-eng-course/labs/internal/modelcfg"
	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/agentdb"
	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/graphmemory/backend"
	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/ledger"
	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/llm"
)

const (
	appName   = "ledgerworks"
	taskQueue = "ledgerworks-task-queue"
)

// runApp dispatches the durable subcommands. Step-0 (main.go's own main())
// handles everything else, so a bare `go run . "message"` keeps working.
func runApp(ctx context.Context, args []string) error {
	switch args[0] {
	case "worker":
		return runWorker(ctx)
	case "start":
		return runStart(ctx, args[1:])
	case "counter":
		return runCounter(ctx)
	default:
		return fmt.Errorf("unknown subcommand %q (want worker, start, counter)", args[0])
	}
}

// chooseModelName decides the model NAME both the worker (which registers a
// factory under it) and start (which puts it on the workflow Request) must
// agree on. modelcfg.Choose reads MODEL/DEFAULT_MODEL_PROVIDER without
// building a live client — start needs only the name, never credentials.
func chooseModelName() (name string, demo bool, err error) {
	if os.Getenv("LABS8_MODEL") == "demo" {
		return "demo-rules", true, nil
	}
	if err := modelcfg.LoadEnv("."); err != nil {
		fmt.Printf("warning: %v\n", err)
	}
	choice, err := modelcfg.Choose(os.Getenv("MODEL"), os.Getenv("DEFAULT_MODEL_PROVIDER"))
	if err != nil {
		return "", false, fmt.Errorf("немає LLM-провайдера: %w\nНалаштуйте ключ в apps/.env (див. apps/.env-example) або запустіть офлайн: LABS8_MODEL=demo", err)
	}
	return choice.Model, false, nil
}

func agentDBPath() string {
	if p := os.Getenv("AGENT_DB"); p != "" {
		return p
	}
	return agentdb.DefaultPath
}

// faultPlanFromEnv parses LEDGER_FAULTS (e.g. "503,503,timeout") into a
// queue the Desk pops from — a FIFO, not a probability, so a crash test is
// reproducible run to run.
func faultPlanFromEnv(s string) *ledger.FaultPlan {
	p := &ledger.FaultPlan{}
	for _, f := range strings.Split(s, ",") {
		switch strings.TrimSpace(f) {
		case "":
		case "503":
			p.Push(ledger.Unavailable)
		case "timeout":
			p.Push(ledger.Timeout)
		case "429":
			p.Push(ledger.RateLimited)
		}
	}
	return p
}

// latencyFromEnv parses LEDGER_LATENCY (e.g. "15s"); an empty or malformed
// value means no injected latency.
func latencyFromEnv(s string) time.Duration {
	d, _ := time.ParseDuration(s)
	return d
}

// runWorker starts the Temporal worker: registers AgentWorkflow and every
// Activity, and wires the chosen model (demo or live) into googleadk so
// InvokeModel can rebuild it worker-side. Credentials never cross into the
// workflow — only the model name (Request.Model) does.
func runWorker(ctx context.Context) error {
	modelName, demo, err := chooseModelName()
	if err != nil {
		return err
	}

	db, err := agentdb.Open(agentDBPath())
	if err != nil {
		return fmt.Errorf("open agentdb: %w", err)
	}
	defer db.Close()

	mem, closer, err := backend.Open(ctx, backend.ConfigFromEnv(os.Getenv))
	if err != nil {
		return err
	}
	defer closer.Close()

	var factory googleadk.ModelFactory
	if demo {
		fmt.Println("worker: model demo-rules (offline, LABS8_MODEL=demo)")
		factory = func(context.Context, string) (model.LLM, error) {
			return llm.Counting{Inner: llm.Demo{Sloppy: true}, Recorder: db}, nil
		}
	} else {
		m, choice, err := modelcfg.Load(ctx)
		if err != nil {
			return fmt.Errorf("немає LLM-провайдера: %w\nНалаштуйте ключ в apps/.env або запустіть офлайн: LABS8_MODEL=demo go run . worker", err)
		}
		fmt.Printf("worker: model live (%s) %s\n", modelName, choice.Reason)
		factory = func(context.Context, string) (model.LLM, error) {
			return llm.Counting{Inner: m, Recorder: db}, nil
		}
	}

	adkPlugin, err := googleadk.NewPlugin(googleadk.Config{Models: map[string]googleadk.ModelFactory{modelName: factory}})
	if err != nil {
		return err
	}

	c, err := client.Dial(client.Options{})
	if err != nil {
		return fmt.Errorf("temporal at localhost:7233 (is `temporal server start-dev` running?): %w", err)
	}
	defer c.Close()

	w := worker.New(c, taskQueue, worker.Options{Plugins: []worker.Plugin{adkPlugin}})

	acts := &Activities{
		Desk: &ledger.Desk{
			Cases:   db.Cases(),
			Faults:  faultPlanFromEnv(os.Getenv("LEDGER_FAULTS")),
			Latency: latencyFromEnv(os.Getenv("LEDGER_LATENCY")),
		},
		DB:     db,
		Memory: mem,
	}
	w.RegisterWorkflow(AgentWorkflow)
	w.RegisterActivityWithOptions(acts.OpenRefundCase, activity.RegisterOptions{Name: ActivityOpenRefundCase})
	w.RegisterActivityWithOptions(acts.LoadSession, activity.RegisterOptions{Name: ActivityLoadSession})
	w.RegisterActivityWithOptions(acts.PersistSession, activity.RegisterOptions{Name: ActivityPersistSession})
	w.RegisterActivityWithOptions(acts.RememberSession, activity.RegisterOptions{Name: ActivityRememberSession})
	w.RegisterActivityWithOptions(acts.SearchNodes, activity.RegisterOptions{Name: ActivitySearchNodes})
	w.RegisterActivityWithOptions(acts.OpenNodes, activity.RegisterOptions{Name: ActivityOpenNodes})

	fmt.Printf("worker: task queue %q, app %q\n", taskQueue, appName)
	return w.Run(worker.InterruptCh())
}

// runStart dispatches one turn (AgentWorkflow) and waits for its answer —
// the client side of the durable loop. It needs no credentials: only the
// model NAME crosses into the Request, same as the worker's factory key.
func runStart(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	sessionID := fs.String("session", "s1", "session id to continue or start")
	userID := fs.String("user", "taras", "user id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: go run . start [-session ID] [-user ID] MESSAGE")
	}
	message := strings.Join(fs.Args(), " ")

	modelName, _, err := chooseModelName()
	if err != nil {
		return err
	}

	c, err := client.Dial(client.Options{})
	if err != nil {
		return fmt.Errorf("temporal at localhost:7233 (is `temporal server start-dev` running?): %w", err)
	}
	defer c.Close()

	wfID := fmt.Sprintf("refund-%s-%d", *sessionID, time.Now().UnixNano())
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: wfID, TaskQueue: taskQueue}, AgentWorkflow,
		Request{UserID: *userID, SessionID: *sessionID, Message: message, Model: modelName})
	if err != nil {
		return fmt.Errorf("start workflow: %w", err)
	}
	fmt.Printf("[wf:started] %s (session %s)\n", run.GetID(), *sessionID)

	var answer string
	if err := run.Get(ctx, &answer); err != nil {
		return fmt.Errorf("workflow failed: %w", err)
	}
	fmt.Printf("[ev:answer] %s\n", answer)
	return nil
}

// runCounter prints the durable counters the crash-safe proof (п.4) reads:
// how many real model calls and how many refund cases exist, ever — the
// evidence that a resumed run did not repeat work a crash already paid for.
func runCounter(ctx context.Context) error {
	db, err := agentdb.Open(agentDBPath())
	if err != nil {
		return err
	}
	defer db.Close()

	calls, err := db.CountLLMCalls(ctx)
	if err != nil {
		return err
	}
	cases, err := db.CountCases(ctx)
	if err != nil {
		return err
	}
	promptTok, outTok, err := db.SumLLMTokens(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("llm_calls=%d refund_cases=%d tokens_in=%d tokens_out=%d\n", calls, cases, promptTok, outTok)
	return nil
}
