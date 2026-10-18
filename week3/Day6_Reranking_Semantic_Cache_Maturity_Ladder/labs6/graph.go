package main

import (
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/workflow"
)

// newGraph збирає агентний retrieval-граф (Homework п.1):
//
//	Start → cacheCheck ─ hit  → answer
//	                   ─ miss → classify ─┬─ vectorSearchBranch   ─┐
//	                                      ├─ graphFilterBranch     ├─ join → mergeBranches → budgetGate ─ insufficient → insufficientEvidence
//	                                      └─ graphTraversalBranch ─┘                                    ─ ok          → rerank → answer
//
// Коментар над cacheCheck у retrieval.go пояснює бюджет і чому
// graphTraversalBranch не фільтрується тарифом заздалегідь (fan-out, не
// послідовний ланцюжок). Еталон fan-out + JoinNode:
// https://github.com/google/adk-go/blob/v2.5.0/examples/workflow/complex/main.go
func newGraph(r *retriever) (agent.Agent, error) {
	// Без retry: вузли локальні (CPU + опційно Ollama на localhost), повтор
	// однакового входу не змінить результат.
	cfg := workflow.NodeConfig{}

	cacheCheckNode := workflow.NewFunctionNode("cache_check", r.cacheCheck, cfg)
	classifyNode := workflow.NewFunctionNode("classify", r.classify, cfg)
	vectorSearchNode := workflow.NewFunctionNode(nodeVectorSearch, r.vectorSearchBranch, cfg)
	graphFilterNode := workflow.NewFunctionNode(nodeGraphFilter, r.graphFilterBranch, cfg)
	graphTraversalNode := workflow.NewFunctionNode(nodeGraphTraversal, r.graphTraversalBranch, cfg)
	joinNode := workflow.NewJoinNode("join")
	mergeNode := workflow.NewFunctionNode("merge", mergeBranches, cfg)
	budgetGateNode := workflow.NewFunctionNode("budget_gate", budgetGate, cfg)
	insufficientNode := workflow.NewFunctionNode("insufficient_evidence", insufficientEvidence, cfg)
	rerankNode := workflow.NewFunctionNode("rerank", r.rerank, cfg)
	answerNode := workflow.NewFunctionNode("answer", r.answer, cfg)

	eb := workflow.NewEdgeBuilder()
	eb.Add(workflow.Start, cacheCheckNode)
	eb.AddRoute(cacheCheckNode, answerNode, workflow.StringRoute(routeCacheHit))
	eb.AddRoute(cacheCheckNode, classifyNode, workflow.StringRoute(routeCacheMiss))
	eb.AddFanOut(classifyNode, vectorSearchNode, graphFilterNode, graphTraversalNode)
	eb.AddFanIn(joinNode, vectorSearchNode, graphFilterNode, graphTraversalNode)
	eb.Add(joinNode, mergeNode)
	eb.Add(mergeNode, budgetGateNode)
	eb.AddRoute(budgetGateNode, insufficientNode, workflow.StringRoute(routeInsufficient))
	eb.AddRoute(budgetGateNode, rerankNode, workflow.StringRoute(routeOK))
	eb.Add(rerankNode, answerNode)

	return workflowagent.New(workflowagent.Config{
		Name:        "agentic_graphrag_engine",
		Description: "Агентний retrieval-граф: кеш → класифікація → fan-out(vector/graph_filter/graph_traversal) → join → бюджет-гейт → re-rank → відповідь із provenance.",
		Edges:       eb.Build(),
	})
}
