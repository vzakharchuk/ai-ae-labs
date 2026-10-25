// tools.go — інструменти агента над ledger.Desk: валідація й побічний ефект
// у ledger, політика повторів і перетворення помилки на Observation — тут.
//
// retry() — стенд-ін для Temporal RetryPolicy (крок 0, один процес, без
// Temporal); workflow.go/activities.go (етап C) замінюють його на Activity
// з RetryPolicy, не змінюючи саму openRefundCase.
package main

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"google.golang.org/adk/v2/agent"

	"go.temporal.io/sdk/contrib/googleadk"

	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/graphmemory"
	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/ledger"
)

// errNotDurable — openRefundCaseDurable/searchNodesDurable/openNodesDurable
// працюють лише всередині workflow.go (AgentWorkflow), ніколи в кроці 0.
var errNotDurable = errors.New("tools: called outside a Temporal workflow context")

// toolActivityOptions — політика повторів для tool-Activity: рушій сам
// повторює лише errTypeTransient (п.3). 4 спроби — та сама межа, що
// retry() (стенд-ін без Temporal) використовував до появи Activity.
//
// StartToCloseTimeout — 30s, не 10s: перша версія мала 10s і помилково
// таймаутила кожну спробу ПЕРШЕ, ніж встигав пройти LEDGER_LATENCY=15s із
// доказу crash-safe (п.4) — виявлено реальним прогоном (лог показував
// "activity StartToClose timeout" замість очікуваного успіху після
// відновлення). 30s — запас над будь-яким реалістичним LEDGER_LATENCY для
// цього ДЗ.
var toolActivityOptions = workflow.ActivityOptions{
	StartToCloseTimeout: 30 * time.Second,
	RetryPolicy: &temporal.RetryPolicy{
		MaximumAttempts:        4,
		NonRetryableErrorTypes: []string{errTypeSemantic, errTypeFatal},
	},
}

// RefundResult — відповідь open_refund_case моделі: кейс або Observation.
type RefundResult struct {
	Status      string `json:"status"`
	CaseID      string `json:"case_id,omitempty"`
	Replayed    bool   `json:"replayed,omitempty"`
	Field       string `json:"field,omitempty"`
	Observation string `json:"observation,omitempty"`
	Attempt     int    `json:"attempt,omitempty"`
}

// retry повторює лише те, що classifyError назвав транзієнтним.
func retry(ctx context.Context, attempts int, fn func() (ledger.RefundCaseOutput, error)) (ledger.RefundCaseOutput, error) {
	var lastErr error
	for i := range attempts {
		out, err := fn()
		if err == nil {
			return out, nil
		}
		if class, _ := classifyError(err); class != ClassRetryTransient {
			return out, err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ledger.RefundCaseOutput{}, ctx.Err()
		case <-time.After(time.Duration(i+1) * 100 * time.Millisecond):
		}
	}
	return ledger.RefundCaseOutput{}, lastErr
}

// openRefundCase будує інструмент: валідація й побічний ефект — у ledger.Desk
// (ключ ідемпотентності = transaction_id + merchant_id), політика — тут.
func openRefundCase(desk *ledger.Desk, b *budget) func(agent.Context, ledger.RefundCaseInput) (RefundResult, error) {
	return func(ctx agent.Context, in ledger.RefundCaseInput) (RefundResult, error) {
		out, err := retry(ctx, 4, func() (ledger.RefundCaseOutput, error) { return desk.OpenRefundCase(ctx, in) })
		if err != nil {
			obs, ferr := b.observe(err)
			if ferr != nil {
				return RefundResult{}, ferr
			}
			return RefundResult{Status: obs.Status, Field: obs.Field, Observation: obs.Observation, Attempt: obs.Attempt}, nil
		}
		return RefundResult{Status: out.Status, CaseID: out.CaseID, Replayed: out.Replayed}, nil
	}
}

const instruction = `You are the LEDGERWORKS refund desk agent. You only open refund cases.
Copy ids exactly as the user wrote them: merchants look like A-114, transactions like txn-2026-07-118845. Never send placeholders such as "undefined".
If a tool returns status "invalid_argument", read the observation, fix the named field from the user's message, and call the tool again.
If a tool returns status "unavailable" or "stopped", do not call it again; tell the user what happened.
You can also look up a merchant's recent payouts, and recall what you were told in earlier sessions with search_nodes (by keyword) or open_nodes (by exact id, e.g. A-114).`

// openRefundCaseDurable — durable-версія openRefundCase (п.3): той самий
// контракт і RefundResult, але побічний ефект іде через ActivityOpenRefundCase
// (activities.go), не напряму в ledger.Desk, і помилка читається через
// b.observeActivity (класифікація за рядком типу, не за Go-типом — див.
// harness.go, classifyActivityError).
func openRefundCaseDurable(b *budget) func(agent.Context, ledger.RefundCaseInput) (RefundResult, error) {
	return func(ctx agent.Context, in ledger.RefundCaseInput) (RefundResult, error) {
		wfCtx, ok := googleadk.WorkflowContext(ctx)
		if !ok {
			return RefundResult{}, errNotDurable
		}
		actx := workflow.WithActivityOptions(wfCtx, toolActivityOptions)
		var out ledger.RefundCaseOutput
		err := workflow.ExecuteActivity(actx, ActivityOpenRefundCase, in).Get(wfCtx, &out)
		if err != nil {
			obs, ferr := b.observeActivity(err)
			if ferr != nil {
				return RefundResult{}, ferr
			}
			return RefundResult{Status: obs.Status, Field: obs.Field, Observation: obs.Observation, Attempt: obs.Attempt}, nil
		}
		return RefundResult{Status: out.Status, CaseID: out.CaseID, Replayed: out.Replayed}, nil
	}
}

// getMerchantPayouts — ordinary function tool, НЕ Activity: ledger.GetMerchantPayouts
// не робить I/O (детермінований lookup у захардкодженій мапі), тож
// виконання прямо в workflow не порушує "Workflow не виконує I/O" — там
// йдеться про диск/мережу, не про чисті обчислення.
func getMerchantPayouts(ctx agent.Context, in ledger.PayoutsInput) (ledger.PayoutsOutput, error) {
	return ledger.GetMerchantPayouts(ctx, in)
}

// SearchNodesArgs/OpenNodesArgs — те, що бачить модель: без user_id (модель
// не знає, хто питає — tools.go підставляє його з agent.Context).
type SearchNodesArgs struct {
	Query string `json:"query" jsonschema:"what to look for in earlier conversations"`
}

type OpenNodesArgs struct {
	Names []string `json:"names" jsonschema:"exact ids to recall everything about, e.g. A-114"`
}

// searchNodesDurable — пам'ять за ключовим словом (п.6), через ActivitySearchNodes.
func searchNodesDurable() func(agent.Context, SearchNodesArgs) (graphmemory.KnowledgeGraph, error) {
	return func(ctx agent.Context, in SearchNodesArgs) (graphmemory.KnowledgeGraph, error) {
		wfCtx, ok := googleadk.WorkflowContext(ctx)
		if !ok {
			return graphmemory.KnowledgeGraph{}, errNotDurable
		}
		actx := workflow.WithActivityOptions(wfCtx, toolActivityOptions)
		var out graphmemory.KnowledgeGraph
		err := workflow.ExecuteActivity(actx, ActivitySearchNodes, SearchNodesInput{UserID: ctx.UserID(), Query: in.Query}).Get(wfCtx, &out)
		return out, err
	}
}

// openNodesDurable — пам'ять за точним id (п.6), через ActivityOpenNodes.
func openNodesDurable() func(agent.Context, OpenNodesArgs) (graphmemory.KnowledgeGraph, error) {
	return func(ctx agent.Context, in OpenNodesArgs) (graphmemory.KnowledgeGraph, error) {
		wfCtx, ok := googleadk.WorkflowContext(ctx)
		if !ok {
			return graphmemory.KnowledgeGraph{}, errNotDurable
		}
		actx := workflow.WithActivityOptions(wfCtx, toolActivityOptions)
		var out graphmemory.KnowledgeGraph
		err := workflow.ExecuteActivity(actx, ActivityOpenNodes, OpenNodesInput{UserID: ctx.UserID(), Names: in.Names}).Get(wfCtx, &out)
		return out, err
	}
}
