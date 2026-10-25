// Стартовий шаблон ДЗ 8 — станом на ADK Go v2.6.0 (жовтень 2026), перевірте актуальність API.
//
// Це крок 0: той самий агент, що в еталоні, але в одному процесі — без Temporal і
// без Dgraph. harness.go/tools.go — серце harness-а (п. 1–2 завдання); кроки 3–6
// (durable цикл, сесія, пам'ять) — у workflow.go/activities.go/app.go (етапи C–D).
//
// Запуск (≤15 хв до першого результату):
//
//	LABS8_MODEL=demo go run . "Open a refund for txn-2026-07-118845 at merchant A-114"   # офлайн
//	go run . -faults 503,timeout "Open a refund for txn-2026-07-118845 at merchant A-114" # справжня модель
//
// Без LABS8_MODEL=demo модель обирає internal/modelcfg: apps/.env підхоплюється сам
// (шаблон — apps/.env-example), MODEL і DEFAULT_MODEL_PROVIDER працюють так само, як
// у лабах Тижня 1. Обрана модель друкується першим рядком. Якщо провайдера немає —
// програма каже про це й підказує LABS8_MODEL=demo.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/dimetron/ai-eng-course/labs/internal/modelcfg"
	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/ledger"
	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/llm"
)

func main() {
	// Підкоманди (worker/start/counter, app.go) розбираються ДО flag.Parse():
	// крок 0 нижче не знає про них, і "worker"/"start"/"counter" як перше
	// слово завдання для кроку 0 — неправдоподібне завдання, тож неоднозначності нема.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "worker", "start", "counter":
			if err := runApp(context.Background(), os.Args[1:]); err != nil {
				log.Fatal(err)
			}
			return
		}
	}

	faultsFlag := flag.String("faults", "", "scripted acquirer faults, e.g. 503,timeout (a queue, not a probability)")
	flag.Parse()
	if flag.NArg() == 0 {
		log.Fatal("usage: go run . [-faults 503,timeout] MESSAGE  |  go run . worker|start|counter")
	}
	ctx := context.Background()

	m, err := pickModel(ctx)
	if err != nil {
		log.Fatal(err)
	}
	faults := &ledger.FaultPlan{}
	for _, f := range strings.Split(*faultsFlag, ",") {
		switch strings.TrimSpace(f) {
		case "":
		case "503":
			faults.Push(ledger.Unavailable)
		case "timeout":
			faults.Push(ledger.Timeout)
		case "429":
			faults.Push(ledger.RateLimited)
		default:
			log.Fatalf("unknown fault %q (want 503, timeout, 429)", f)
		}
	}
	desk := &ledger.Desk{Cases: &ledger.MemCases{}, Faults: faults}

	b := &budget{}
	refund, err := functiontool.New(functiontool.Config{
		Name:        "open_refund_case",
		Description: "Opens a refund case for one transaction of one merchant. Needs transaction_id and merchant_id exactly as the user wrote them. May return status invalid_argument with an observation: read it, fix the named field, call again.",
	}, openRefundCase(desk, b))
	if err != nil {
		log.Fatal(err)
	}
	root, err := llmagent.New(llmagent.Config{
		Name: "refund_agent", Description: "Opens refund cases for LEDGERWORKS.",
		Model: m, Instruction: instruction, Tools: []tool.Tool{refund},
	})
	if err != nil {
		log.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "ledgerworks", Agent: root, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		log.Fatal(err)
	}

	msg := genai.NewContentFromText(strings.Join(flag.Args(), " "), genai.RoleUser)
	for ev, err := range r.Run(ctx, "taras", "s1", msg, agent.RunConfig{}) {
		if err != nil {
			log.Fatalf("[ev:failed] %v", err)
		}
		printEvent(ev)
	}
	fmt.Printf("[ev:success] self_corrections=%d\n", b.used)
}

// pickModel: LABS8_MODEL=demo — офлайн-модель на правилах (Sloppy відтворює
// інцидент 03:00); інакше — провайдер курсу через internal/modelcfg.
func pickModel(ctx context.Context) (model.LLM, error) {
	if os.Getenv("LABS8_MODEL") == "demo" {
		fmt.Println("model: demo-rules (offline, LABS8_MODEL=demo)")
		return llm.Demo{Sloppy: true}, nil
	}
	if err := modelcfg.LoadEnv("."); err != nil {
		fmt.Printf("warning: %v\n", err)
	}
	m, choice, err := modelcfg.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("немає LLM-провайдера: %w\nНалаштуйте ключ в apps/.env (див. apps/.env-example) або запустіть офлайн: LABS8_MODEL=demo go run . \"...\"", err)
	}
	fmt.Printf("model: %s\n", choice.Reason)
	return m, nil
}

func printEvent(ev *session.Event) {
	if ev == nil || ev.Content == nil {
		return
	}
	for _, p := range ev.Content.Parts {
		switch {
		case p == nil:
		case p.FunctionCall != nil:
			fmt.Printf("[ev:tool_call] %s %v\n", p.FunctionCall.Name, p.FunctionCall.Args)
		case p.FunctionResponse != nil:
			fmt.Printf("[ev:tool_observation] %s %v\n", p.FunctionResponse.Name, p.FunctionResponse.Response)
		case strings.TrimSpace(p.Text) != "":
			fmt.Printf("[ev:answer] %s\n", strings.TrimSpace(p.Text))
		}
	}
}
