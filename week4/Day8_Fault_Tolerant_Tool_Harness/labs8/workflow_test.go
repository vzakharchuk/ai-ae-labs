// workflow_test.go — обов'язкова умова для п.1-3 (Homework.md): ≥3 сценарії
// збоїв (мережевий таймаут, невалідні аргументи, недоступна залежність), кожен
// — окрема testsuite.WorkflowTestSuite + скриптована модель + ledger.FaultPlan.
// Без API-ключа, без Docker, без сервера Temporal: testsuite дає фейковий
// Temporal-рантайм, googleadk.NewFakeModel — скриптовану модель, усі реальні
// Activity (OpenRefundCase/LoadSession/...) працюють як на справжньому
// воркері, лише DB/Memory — тимчасові (SQLite-файл у t.TempDir(),
// graphmemorytest.Fake замість Dgraph).
package main

import (
	"context"
	"path/filepath"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/contrib/googleadk"
	"go.temporal.io/sdk/testsuite"

	"google.golang.org/adk/v2/model"

	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/agentdb"
	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/graphmemory"
	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/graphmemory/graphmemorytest"
	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/ledger"
)

// testModelName — ім'я, під яким і workflow (googleadk.NewModel), і
// googleadk.Config.Models (воркер-сторона тесту) погоджуються, яку
// скриптовану модель реконструювати — той самий контракт, що app.go
// використовує для демо/живої моделі (chooseModelName).
const testModelName = "test-model"

// testHarness — усе, що один тест-сценарій збирає: TestWorkflowEnvironment
// із зареєстрованими справжніми Activity (не заглушками) і тимчасовими DB/
// Memory, плюс сам Desk, щоб тест міг перевірити побічний ефект
// (CountCases) після прогону.
type testHarness struct {
	env *testsuite.TestWorkflowEnvironment
	db  *agentdb.DB
}

func newTestHarness(t *testing.T, faults *ledger.FaultPlan, responses ...*model.LLMResponse) *testHarness {
	t.Helper()
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()

	db, err := agentdb.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("agentdb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mem := graphmemory.New(&graphmemorytest.Fake{})

	acts := &Activities{
		Desk:   &ledger.Desk{Cases: db.Cases(), Faults: faults},
		DB:     db,
		Memory: mem,
	}
	env.RegisterActivityWithOptions(acts.OpenRefundCase, activity.RegisterOptions{Name: ActivityOpenRefundCase})
	env.RegisterActivityWithOptions(acts.LoadSession, activity.RegisterOptions{Name: ActivityLoadSession})
	env.RegisterActivityWithOptions(acts.PersistSession, activity.RegisterOptions{Name: ActivityPersistSession})
	env.RegisterActivityWithOptions(acts.RememberSession, activity.RegisterOptions{Name: ActivityRememberSession})
	env.RegisterActivityWithOptions(acts.SearchNodes, activity.RegisterOptions{Name: ActivitySearchNodes})
	env.RegisterActivityWithOptions(acts.OpenNodes, activity.RegisterOptions{Name: ActivityOpenNodes})

	fake := googleadk.NewFakeModel(responses...).WithName(testModelName)
	gaActs, err := googleadk.NewActivities(googleadk.Config{
		Models: map[string]googleadk.ModelFactory{
			testModelName: func(context.Context, string) (model.LLM, error) { return fake, nil },
		},
	})
	if err != nil {
		t.Fatalf("googleadk.NewActivities: %v", err)
	}
	gaActs.Register(env) // *TestWorkflowEnvironment задовольняє worker.Registry

	env.RegisterWorkflow(AgentWorkflow)
	return &testHarness{env: env, db: db}
}

func (h *testHarness) run(t *testing.T, req Request) (string, error) {
	t.Helper()
	h.env.ExecuteWorkflow(AgentWorkflow, req)
	if err := h.env.GetWorkflowError(); err != nil {
		return "", err
	}
	var answer string
	if err := h.env.GetWorkflowResult(&answer); err != nil {
		t.Fatalf("GetWorkflowResult: %v", err)
	}
	return answer, nil
}

// TestWorkflow_NetworkTimeout — сценарій 1: мережевий таймаут. Timeout —
// транзієнтний (ledger.Fault.Err), пушнутий БІЛЬШЕ разів, ніж
// RetryPolicy.MaximumAttempts (4, tools.go) — рушій сам вичерпує повтори, і
// лише ПОТІМ harness класифікує exhausted-транзієнтну помилку як
// Observation{Status: "unavailable"} (не крашить хід, не зависає).
func TestWorkflow_NetworkTimeout(t *testing.T) {
	faults := &ledger.FaultPlan{}
	faults.Push(ledger.Timeout, ledger.Timeout, ledger.Timeout, ledger.Timeout, ledger.Timeout)

	h := newTestHarness(t, faults,
		googleadk.FunctionCallResponse("call-1", "open_refund_case", map[string]any{
			"transaction_id": "txn-2026-07-900001", "merchant_id": "A-114"}),
		googleadk.TextResponse("The acquirer is unavailable right now."),
	)

	answer, err := h.run(t, Request{UserID: "taras", SessionID: "sess-timeout", Message: "open a refund", Model: testModelName})
	if err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if answer != "The acquirer is unavailable right now." {
		t.Errorf("answer = %q", answer)
	}
	if n, err := h.db.CountCases(context.Background()); err != nil || n != 0 {
		t.Errorf("cases = %d, %v; want 0 — a timed-out call must not create a case", n, err)
	}
}

// TestWorkflow_InvalidArguments — сценарій 2: невалідні аргументи.
// Перший виклик — sloppy merchant_id ("undefined", той самий інцидент
// 03:00), другий — скриптована модель читає Observation і виправляється.
func TestWorkflow_InvalidArguments(t *testing.T) {
	h := newTestHarness(t, &ledger.FaultPlan{},
		googleadk.FunctionCallResponse("call-1", "open_refund_case", map[string]any{
			"transaction_id": "txn-2026-07-900002", "merchant_id": "undefined"}),
		googleadk.FunctionCallResponse("call-2", "open_refund_case", map[string]any{
			"transaction_id": "txn-2026-07-900002", "merchant_id": "A-114"}),
		googleadk.TextResponse("Refund case opened."),
	)

	answer, err := h.run(t, Request{UserID: "taras", SessionID: "sess-invalid", Message: "open a refund", Model: testModelName})
	if err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if answer != "Refund case opened." {
		t.Errorf("answer = %q", answer)
	}
	if n, err := h.db.CountCases(context.Background()); err != nil || n != 1 {
		t.Errorf("cases = %d, %v; want 1 — the corrected call must succeed exactly once", n, err)
	}
}

// TestWorkflow_UnavailableDependency — сценарій 3: недоступна залежність
// (503, не таймаут — інший Fault, та сама exhausted-транзієнтна гілка).
func TestWorkflow_UnavailableDependency(t *testing.T) {
	faults := &ledger.FaultPlan{}
	faults.Push(ledger.Unavailable, ledger.Unavailable, ledger.Unavailable, ledger.Unavailable, ledger.Unavailable)

	h := newTestHarness(t, faults,
		googleadk.FunctionCallResponse("call-1", "open_refund_case", map[string]any{
			"transaction_id": "txn-2026-07-900003", "merchant_id": "A-114"}),
		googleadk.TextResponse("The acquirer returned 503; I did not retry further."),
	)

	answer, err := h.run(t, Request{UserID: "taras", SessionID: "sess-unavailable", Message: "open a refund", Model: testModelName})
	if err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if answer != "The acquirer returned 503; I did not retry further." {
		t.Errorf("answer = %q", answer)
	}
	if n, err := h.db.CountCases(context.Background()); err != nil || n != 0 {
		t.Errorf("cases = %d, %v; want 0", n, err)
	}
}

// TestClassifyActivityError — доповнює TestClassifyError (main_test.go):
// той самий контракт, але за рядком ApplicationError.Type()/TimeoutError, не
// за Go-типом (п.3 — "через межу... проходить лише рядок типу помилки").
// TimeoutError-гілку додано після реального прогону crash-safe демо (п.4):
// без неї вичерпаний StartToClose-таймаут падав у ClassFatal, і модель
// бачила сирий текст Temporal-помилки замість Observation{"unavailable"}.
func TestClassifyActivityError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want Class
	}{
		{"canceled is fatal", context.Canceled, ClassFatal},
		{"unknown error is fatal", context.DeadlineExceeded, ClassFatal}, // не ApplicationError/TimeoutError — наосліп не повторюємо
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got, _, _ := classifyActivityError(tt.err); got != tt.want {
				t.Errorf("classifyActivityError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
