// ВИ: мануальний ReAct-цикл. Тут немає імпортів ADK — це `for` і `if` над
// інтерфейсом Planner та реєстром інструментів.
//
// Словник (ToolCall, Step, Planner, Budget, Trace, Registry, Observation) уже
// є в internal/react — прочитайте його перед тим, як писати цикл.
package main

import (
	"context"
	"fmt"

	"github.com/dimetron/ai-eng-course/labs/week4/Day7_ReAct_Loop_Internals/labs7/internal/react"
)

// Run виконує цикл Thought → Action → Observation, доки Step.Final не стане
// true або не вичерпано Budget.
//
// Trace повертається і разом із помилкою: о 3-й ночі саме він показує, де
// застрягло — тому накопичуємо його на кожній ітерації, а не лише на успіху.
func Run(ctx context.Context, p react.Planner, reg react.Registry, task string, b react.Budget) (string, react.Trace, error) {
	b = b.Normalized()
	history := react.Seed(task)
	var trace react.Trace

	for i := 1; ; i++ {
		if i > b.MaxIterations {
			return "", trace, fmt.Errorf("вичерпано ліміт %d ітерацій: %w", b.MaxIterations, ErrMaxIterations)
		}

		step, err := p.NextStep(ctx, history)
		if err != nil {
			return "", trace, err
		}
		trace.Steps = append(trace.Steps, step)
		trace.Iterations = i
		trace.Usage = trace.Usage.Add(step.Usage)

		if step.Final {
			return step.Answer, trace, nil
		}

		// Послідовно: core-вимога дозволяє це (паралельні read-only виклики —
		// ++ Advanced). Порядок Observation відповідає порядку step.Calls, а
		// CallID на кожному — те, за чим history.Append зводить їх разом,
		// не позиція в слайсі.
		observations := make([]react.Observation, 0, len(step.Calls))
		for _, call := range step.Calls {
			observations = append(observations, reg.Invoke(ctx, call))
		}
		trace.Observations = append(trace.Observations, observations)

		history = history.Append(step, observations)
	}
}
