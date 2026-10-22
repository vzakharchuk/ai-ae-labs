// ВИ: той самий цикл у workflow-графі ADK. Крок — вузол (NewFunctionNode),
// цикл — динамічний вузол (NewDynamicNode), що викликає крок через RunNode.
// Еталон API: sources/github/adk-go/examples/workflow/dynamic/basic/main.go.
package main

import (
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

	"github.com/dimetron/ai-eng-course/labs/week4/Day7_ReAct_Loop_Internals/labs7/internal/react"
)

// StepResult — те, що вузол-крок пише в event log.
type StepResult struct {
	Iteration int    `json:"iteration"`
	Thought   string `json:"thought"`
	Calls     int    `json:"calls"`
	Final     bool   `json:"final"`
}

// NewGraphAgent збирає агента Start → react_loop.
//
// plan_step — NewFunctionNode: типізований вхід/вихід, event log, schema-
// валідація — усе від рушія. Але RunNode повертає лише StepResult (те, що
// призначене для event log — Iteration/Thought/кількість Calls/Final), а
// orchestrator-у нижче потрібен ПОВНИЙ react.Step (самі ToolCall з іменами й
// аргументами, Answer). lastStep — closure-змінна, якою крок передає його
// orchestrator-у в тому самому раунді; RunNode блокує до завершення дитини,
// тож до наступного читання lastStep нічого іншого його не перезапише.
//
// Застереження, чесно: history і lastStep — мутабельний стан, зафіксований у
// closure НАВКОЛО одного агента, не в ctx.State() ADK. Для одного CLI-рану
// (go run . graph console) це безпечно: NewGraphAgent викликається раз на
// процес. Якщо той самий *agent.Agent віддати в `web`-режим і він
// обслуговуватиме кілька паралельних розмов — вони поділять ці змінні й
// переплутають історії. Виправлення — те саме ctx.State()/session, що й у
// решти ADK-вузлів, поза обсягом цього ДЗ.
func NewGraphAgent(p react.Planner, reg react.Registry, b react.Budget) (agent.Agent, error) {
	b = b.Normalized()
	var history react.History
	var lastStep react.Step

	step := workflow.NewFunctionNode("plan_step",
		func(ctx agent.Context, turn int) (StepResult, error) {
			s, err := p.NextStep(ctx, history)
			if err != nil {
				return StepResult{}, err
			}
			lastStep = s
			return StepResult{Iteration: turn, Thought: s.Thought, Calls: len(s.Calls), Final: s.Final}, nil
		}, workflow.NodeConfig{})

	loop := workflow.NewDynamicNode[string, string]("react_loop",
		func(ctx agent.Context, task string, _ func(*session.Event) error) (string, error) {
			history = react.Seed(task)
			for i := 1; ; i++ {
				if i > b.MaxIterations {
					return "", fmt.Errorf("вичерпано ліміт %d ітерацій: %w", b.MaxIterations, ErrMaxIterations)
				}
				if _, err := workflow.RunNode[StepResult](ctx, step, i); err != nil {
					return "", fmt.Errorf("ітерація %d: %w", i, err)
				}

				s := lastStep
				if s.Final {
					return s.Answer, nil
				}

				observations := make([]react.Observation, 0, len(s.Calls))
				for _, call := range s.Calls {
					observations = append(observations, reg.Invoke(ctx, call))
				}
				history = history.Append(s, observations)
			}
		}, workflow.NodeConfig{})

	return workflowagent.New(workflowagent.Config{
		Name:        "react_graph_agent",
		Description: "ReAct-цикл як динамічний вузол workflow-графа з лімітом ітерацій.",
		Edges:       workflow.Chain(workflow.Start, loop),
	})
}
