// engine.go — топологія команди (Homework п.2):
//
//	Start → fan-out(risk_desk, compliance_desk, integration_desk)
//	      → JoinNode → merge → approval_gate → send
//
// Еталон fan-out + JoinNode: examples/workflow/complex/main.go.
package main

import (
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/workflow"

	"github.com/dimetron/ai-eng-course/labs/week5/Day10_Collaboration_Agents_HITL/labs10/internal/approvals"
)

// Назви fan-out вузлів — JoinNode ключує свою вихідну мапу саме ними, і
// merge (нижче) читає мапу за цими самими іменами.
const (
	nodeRiskDesk        = "risk_desk"
	nodeComplianceDesk  = "compliance_desk"
	nodeIntegrationDesk = "integration_desk"
)

// specialistResult — те, що один fan-out вузол повертає: розділ плюс облік
// токенів цього виклику. FunctionNode сам ніколи не повертає error — збій
// спеціаліста вже згорнутий у Section.Status (document.go, runSpecialist).
type specialistResult struct {
	Section Section
	Usage   []Usage
}

// NewEngine збирає граф команди. provider resolves кожному спеціалісту
// модель окремо (ModelFor, specialists.go) — з реальним провайдером це та
// сама модель тричі, офлайн — три різні скриптовані фейки, кожен під СВІЙ
// інструмент.
func NewEngine(provider ModelFor, store *approvals.Store) (agent.Agent, error) {
	risk, err := newRiskDesk(provider)
	if err != nil {
		return nil, fmt.Errorf("risk_desk: %w", err)
	}
	compliance, err := newComplianceDesk(provider)
	if err != nil {
		return nil, fmt.Errorf("compliance_desk: %w", err)
	}
	integration, err := newIntegrationDesk(provider)
	if err != nil {
		return nil, fmt.Errorf("integration_desk: %w", err)
	}

	specialist := func(name string, ag agent.Agent) *workflow.FunctionNode {
		return workflow.NewFunctionNode(name, func(ctx agent.Context, topic string) (specialistResult, error) {
			sec, usage := runSpecialist(ctx, name, ag, topic)
			return specialistResult{Section: sec, Usage: usage}, nil
		}, workflow.NodeConfig{})
	}
	riskNode := specialist(nodeRiskDesk, risk)
	complianceNode := specialist(nodeComplianceDesk, compliance)
	integrationNode := specialist(nodeIntegrationDesk, integration)

	join := workflow.NewJoinNode("join")

	mergeNode := workflow.NewFunctionNode("merge", func(_ agent.Context, in map[string]any) (Document, error) {
		sections := map[string]Section{}
		usage := map[string][]Usage{}
		for name, v := range in {
			r, ok := v.(specialistResult)
			if !ok {
				return Document{}, fmt.Errorf("merge: вузол %q дав %T, очікували specialistResult", name, v)
			}
			sections[name] = r.Section
			usage[name] = r.Usage
		}
		return Merge(sections, usage), nil
	}, workflow.NodeConfig{})

	gateNode := newApprovalGateNode(store)
	sendNode := newSendNode(store)

	eb := workflow.NewEdgeBuilder()
	eb.AddFanOut(workflow.Start, riskNode, complianceNode, integrationNode)
	eb.AddFanIn(join, riskNode, complianceNode, integrationNode)
	eb.Add(join, mergeNode)
	eb.Add(mergeNode, gateNode)
	eb.Add(gateNode, sendNode)

	return workflowagent.New(workflowagent.Config{
		Name:        "ledgerworks_migration_board",
		Description: "Координатор + три спеціалісти + затвор схвалення для плану міграції.",
		Edges:       eb.Build(),
	})
}
