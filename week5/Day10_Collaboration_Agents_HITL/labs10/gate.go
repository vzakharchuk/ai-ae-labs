// gate.go — затвор схвалення (Homework п.3): пауза переживає рестарт
// процесу, бо все, що потрібно для resume, лежить у approvals.Store
// (SQLite), не в пам'яті цього процесу.
//
// Resume із іншого процесу: workflowagent сам детектує resume, коли вхідне
// повідомлення містить FunctionResponse з ID == interruptID і
// Name == workflow.WorkflowInputFunctionCallName — саме так CLI-команда
// `decide` (main.go) «штурхає» паузу, що лишилась від `start`.
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

	"github.com/dimetron/ai-eng-course/labs/week5/Day10_Collaboration_Agents_HITL/labs10/internal/approvals"
)

// approvalWindow — за замовчуванням, скільки мовчання терпимо, перш ніж
// `sweep` (main.go) перетворить запит на timed_out. Перевизначається через
// -window для демонстрацій, де чекати годину незручно.
var approvalWindow = time.Hour

// GateDecision — те, що йде з затвора на send: сам документ, хто й що
// вирішив (за даними approvals.Store, не з payload відповіді), і InterruptID
// — той самий рядок, що показується людині для `decide <id> approve`.
type GateDecision struct {
	Doc    Document
	ID     string
	Status approvals.Status
	By     string
	Reason string
}

// newApprovalGateNode — п.3 (15 балів). Питання людині — факти (дія,
// отримувачі, хеш документа, факти ризику, джерела), не проза координатора:
// Facts у approvals.Request несе саме Document.Sections, не одне узагальнене
// речення.
func newApprovalGateNode(store *approvals.Store) *workflow.FunctionNode {
	rerun := true
	return workflow.NewEmittingFunctionNode[Document, GateDecision]("approval_gate",
		func(ctx agent.Context, doc Document, emit func(*session.Event) error) (GateDecision, error) {
			id := "approve-" + ctx.InvocationID()
			recipients := []string{"oksana@ledgerworks.ua", "board@ledgerworks.ua"}

			rec, err := store.Ask(ctx, approvals.Request{
				InterruptID: id, SessionID: ctx.SessionID(), Action: "send_plan_to_board",
				ArtifactURI: "memo://migration-plan", Hash: doc.Hash, Recipients: recipients,
				Facts:  factsOf(doc),
				Window: approvalWindow,
			})
			if err != nil {
				return GateDecision{}, fmt.Errorf("approval_gate: %w", err)
			}

			// Статус у журналі — джерело правди, не те, що прийшло з
			// payload відповіді (той ми навіть не читаємо): запис у
			// approvals.Store міг з'явитись між паузою й цим resume
			// через `decide` в ІНШОМУ процесі, і саме туди записано
			// хто/що/коли.
			if rec.Status == approvals.Pending {
				if err := emit(workflow.NewRequestInputEvent(ctx, session.RequestInput{
					InterruptID: id,
					Message:     "Схвалення потрібне: відправка плану міграції " + strings.Join(recipients, ", "),
					Payload:     rec.Facts,
				})); err != nil {
					return GateDecision{}, err
				}
				return GateDecision{}, workflow.ErrNodeInterrupted
			}

			return GateDecision{Doc: doc, ID: id, Status: rec.Status, By: rec.DecidedBy, Reason: rec.Reason}, nil
		},
		workflow.NodeConfig{RerunOnResume: &rerun},
	)
}

// factsOf turns a Document into the plain map approvals.Request.Facts keeps
// verbatim for the audit — exactly what the reviewer sees, not a summary of
// it.
func factsOf(doc Document) map[string]any {
	sections := make([]map[string]any, 0, len(doc.Sections))
	for _, s := range doc.Sections {
		sections = append(sections, map[string]any{
			"node": s.Node, "status": s.Status, "error": s.Error, "findings": s.Findings,
		})
	}
	return map[string]any{"hash": doc.Hash, "sections": sections}
}

// newSendNode — тонка обгортка над decideSend навколо FunctionNode.
func newSendNode(store *approvals.Store) *workflow.FunctionNode {
	return workflow.NewFunctionNode("send", func(ctx agent.Context, in GateDecision) (string, error) {
		return decideSend(ctx, store, in)
	}, workflow.NodeConfig{})
}

// decideSend — п.6 (стримування збою, сильна форма): перед відправкою
// перевіряє, чи є в документі неперевірений розділ, і блокує відправку
// НЕЗАЛЕЖНО від рішення людини. Лише потім дивиться на Status. Винесено з
// newSendNode окремою функцією, щоб TestSpecialistFailureDoesNotCorruptTheAnswer
// міг перевірити це напряму, без збирання графа.
func decideSend(ctx context.Context, store *approvals.Store, in GateDecision) (string, error) {
	if bad, reasons := in.Doc.HasUnverified(); bad {
		return fmt.Sprintf("ВІДМОВЛЕНО: документ містить неперевірений розділ (%s) — відправку заблоковано незалежно від рішення людини",
			strings.Join(reasons, "; ")), nil
	}
	switch in.Status {
	case approvals.Approved:
		if err := store.Spend(ctx, in.ID, in.Doc.Hash, []string{"oksana@ledgerworks.ua", "board@ledgerworks.ua"}); err != nil {
			return "", fmt.Errorf("send: %w", err)
		}
		return notifyBoard(in.Doc), nil
	case approvals.Denied:
		return fmt.Sprintf("ВІДМОВЛЕНО %s: %s", in.By, in.Reason), nil
	case approvals.TimedOut:
		return fmt.Sprintf("НЕ ВІДПРАВЛЕНО (мовчання = відмова): %s", in.Reason), nil
	default:
		return fmt.Sprintf("НЕ ВІДПРАВЛЕНО (%s): %s", in.Status, in.Reason), nil
	}
}

// notifyBoard simulates the actual send. A real system would call a mail or
// board-notification API here; the form (who, what, why it is safe to send)
// is the point of this lab, not the transport.
func notifyBoard(doc Document) string {
	return fmt.Sprintf("ВІДПРАВЛЕНО раді директорів: план міграції (hash %s…, %d розділів)", doc.Hash[:12], len(doc.Sections))
}
