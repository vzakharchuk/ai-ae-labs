// Стартовий шаблон ДЗ 10 — «Collaborative Workflow Engine» команди LEDGERWORKS.
// Станом на ADK Go v2.6.0 (10/2026).
//
// Ранній win (≤15 хв) — один спеціаліст з ОДНИМ інструментом і справжньою моделлю:
//
//	go run .
//
// Повна команда:
//
//	go run . start plan1 "План міграції з legacy-процесора"
//	go run . pending
//	go run . decide approve-<id> approve "Ризики прийнятні"
//	go run . sweep
//
// Модель обирає internal/modelcfg (apps/.env, MODEL, DEFAULT_MODEL_PROVIDER).
// Без провайдера програма працює на ОФЛАЙН-ФЕЙКУ й каже про це першим рядком.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/dimetron/ai-eng-course/labs/internal/fakellm"
	"github.com/dimetron/ai-eng-course/labs/internal/labrun"
	"github.com/dimetron/ai-eng-course/labs/internal/modelcfg"
	"github.com/dimetron/ai-eng-course/labs/week5/Day10_Collaboration_Agents_HITL/labs10/internal/approvals"
)

const (
	appName       = "ledgerworks_board"
	boardUser     = "board"
	defaultDBPath = ".data/board.db"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "помилка:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return runEarlyWin(ctx, out)
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(out)
	dbPath := fs.String("db", defaultDBPath, "SQLite-файл сесій і журналу схвалень")
	window := fs.Duration("window", time.Hour, "вікно рішення (мовчання = відмова)")
	failIntegration := fs.Bool("fail-integration", false, "симулювати збій integration_desk (Homework п.6)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()

	approvalWindow = *window
	integrationFailing = *failIntegration

	store, err := approvals.Open(*dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	switch cmd {
	case "start":
		if len(rest) < 2 {
			return errors.New("start <сесія> <завдання>")
		}
		return cmdStart(ctx, out, store, rest[0], strings.Join(rest[1:], " "))
	case "pending":
		return cmdPending(ctx, out, store)
	case "decide":
		if len(rest) < 2 {
			return errors.New("decide <id> approve|reject [причина]")
		}
		return cmdDecide(ctx, out, store, rest[0], rest[1], strings.Join(rest[2:], " "))
	case "sweep":
		return cmdSweep(ctx, out, store)
	default:
		return fmt.Errorf("невідома команда %q (start|pending|decide|sweep)", cmd)
	}
}

// cmdStart запускає один хід команди: fan-out трьох спеціалістів → join →
// merge → затвор. Якщо затвор ставить ран на паузу, друкує InterruptID, яким
// `decide` (інший процес) пізніше розбудить саме цей хід.
func cmdStart(ctx context.Context, out io.Writer, store *approvals.Store, sid, task string) error {
	provider, label := resolveModel(ctx)
	fmt.Fprintln(out, label)

	eng, err := NewEngine(provider, store)
	if err != nil {
		return err
	}
	r, err := runner.New(runner.Config{AppName: appName, Agent: eng, SessionService: store.Sessions(), AutoCreateSession: true})
	if err != nil {
		return err
	}

	msg := genai.NewContentFromText(task, genai.RoleUser)
	return printRun(out, r.Run(ctx, boardUser, sid, msg, agent.RunConfig{}))
}

// cmdDecide пише рішення в журнал (джерело правди) і лише ПОТІМ надсилає
// resume-повідомлення на ту саму сесію — workflowagent сам виявляє, що це
// resume (FunctionResponse на InterruptID), і реконструює граф із історії
// сесії. Так `start` в одному процесі й `decide` в іншому торкаються того
// самого паузованого ходу без жодного спільного стану в пам'яті.
func cmdDecide(ctx context.Context, out io.Writer, store *approvals.Store, id, verdict, reason string) error {
	var status approvals.Status
	switch verdict {
	case "approve":
		status = approvals.Approved
	case "reject":
		status = approvals.Denied
	default:
		return fmt.Errorf("decide: verdict має бути approve або reject, отримано %q", verdict)
	}
	rec, err := store.Decide(ctx, id, approvals.Decision{Status: status, By: boardUser, Reason: reason})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "[approvals] %s -> %s (by %s)\n", id, rec.Status, rec.DecidedBy)
	return resumeRun(ctx, out, store, id, rec.SessionID)
}

// cmdSweep перетворює кожен простроченийPending-запит на timed_out
// (approvals.Store.Expire — Homework п.4) і будить відповідний хід, щоб він
// дійшов до send і завершився явним «не відправлено», а не лишився
// підвішеним.
func cmdSweep(ctx context.Context, out io.Writer, store *approvals.Store) error {
	expired, err := store.Expire(ctx)
	if err != nil {
		return err
	}
	if len(expired) == 0 {
		fmt.Fprintln(out, "[sweep] нічого простроченого")
		return nil
	}
	for _, rec := range expired {
		fmt.Fprintf(out, "[sweep] %s -> timed_out: %s\n", rec.InterruptID, rec.Reason)
		if err := resumeRun(ctx, out, store, rec.InterruptID, rec.SessionID); err != nil {
			return err
		}
	}
	return nil
}

// resumeRun sends the resume FunctionResponse for id on sid. The payload
// carries no decision — the gate re-reads approvals.Store itself — so {}
// is enough; what matters is Name/ID matching the paused interrupt.
func resumeRun(ctx context.Context, out io.Writer, store *approvals.Store, id, sid string) error {
	provider, _ := resolveModel(ctx)
	eng, err := NewEngine(provider, store)
	if err != nil {
		return err
	}
	r, err := runner.New(runner.Config{AppName: appName, Agent: eng, SessionService: store.Sessions()})
	if err != nil {
		return err
	}
	msg := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: id, Name: workflow.WorkflowInputFunctionCallName, Response: map[string]any{"payload": true}},
	}}}
	return printRun(out, r.Run(ctx, boardUser, sid, msg, agent.RunConfig{}))
}

// cmdPending lists every approval still waiting for a human.
func cmdPending(ctx context.Context, out io.Writer, store *approvals.Store) error {
	pending, err := store.PendingList(ctx)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Fprintln(out, "[pending] порожньо")
		return nil
	}
	for _, rec := range pending {
		fmt.Fprintf(out, "[pending] %s  дія=%s  отримувачі=%v  до=%s\n",
			rec.InterruptID, rec.Action, rec.RecipientList(), rec.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

// printRun drains a run's events, printing tool calls, a pause (with its
// InterruptID — what the operator feeds to `decide`) and the final answer.
func printRun(out io.Writer, events func(func(*session.Event, error) bool)) error {
	for ev, err := range events {
		if err != nil {
			if errors.Is(err, workflow.ErrNothingToResume) {
				fmt.Fprintln(out, "[resume] відповідь не знайшла паузи, на яку чекала (вже оброблена?)")
				return nil
			}
			return fmt.Errorf("ран: %w", err)
		}
		if ev == nil {
			continue
		}
		if ev.RequestedInput != nil {
			fmt.Fprintf(out, "[pause] InterruptID=%s  %s\n", ev.RequestedInput.InterruptID, ev.RequestedInput.Message)
			continue
		}
		if t := labrun.EventText(ev); t != "" {
			fmt.Fprintf(out, "[%s] %s\n", ev.Author, t)
		}
	}
	return nil
}

// runEarlyWin — один спеціаліст, один інструмент, без fan-out/затвора. Той
// самий ран, що й у Homework, «Ранній win».
func runEarlyWin(ctx context.Context, out io.Writer) error {
	provider, label := resolveModel(ctx)
	fmt.Fprintln(out, label)

	riskDesk, err := newRiskDesk(provider)
	if err != nil {
		return err
	}
	res, err := labrun.Run(ctx, riskDesk, "Ризики міграції з legacy-процесора")
	if err != nil {
		return fmt.Errorf("ран: %w", err)
	}
	fmt.Fprintln(out, "інструменти:", res.ToolCalls)
	fmt.Fprintln(out, "відповідь:  ", res.Final)
	return nil
}

// resolveModel — справжній провайдер (та сама модель для всіх трьох
// спеціалістів) або названий офлайн-фейк (окремий скриптований екземпляр на
// кожного — ModelFor, specialists.go).
func resolveModel(ctx context.Context) (ModelFor, string) {
	if err := modelcfg.LoadEnv("."); err != nil {
		fmt.Fprintf(os.Stderr, "попередження: %v\n", err)
	}
	m, choice, err := modelcfg.Load(ctx)
	if err != nil {
		cause := err.Error()
		provider := func(toolName string, args map[string]any, text string) model.LLM {
			return fakellm.New("offline-"+toolName, fakellm.CallTurn(toolName, args), fakellm.TextTurn(text))
		}
		return provider, "ОФЛАЙН-ФЕЙК (справжньої LLM немає): " + cause + ". Додайте ключ в apps/.env — див. Homework.md."
	}
	provider := func(string, map[string]any, string) model.LLM { return m }
	return provider, "модель: " + choice.Reason
}
