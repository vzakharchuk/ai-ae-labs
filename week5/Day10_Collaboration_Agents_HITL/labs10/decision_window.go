// decision_window.go — Homework п.4 (5 балів): мовчання протягом вікна
// рішення — це відмова, і відмова з названою причиною, не зависла задача.
//
// Це доповнює, не замінює, operational-шлях (approvals.Store.Expire, main.go
// `sweep`): awaitReply — пряма, синхронна версія того самого принципу, яку
// можна перевірити одним тестом без графа й без SQLite.
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/dimetron/ai-eng-course/labs/week5/Day10_Collaboration_Agents_HITL/labs10/internal/approvals"
)

// awaitReply чекає на одну відповідь із ch, обмежену window. Завжди
// повертає Decision — мовчання саме по собі є рішенням, не особливим
// випадком. Чотири шляхи ведуть у відмову, кожен зі своєю причиною:
// вікно вичерпалось, контекст скасовано, канал закрито без відповіді,
// слово нерозпізнане. П'ятий шлях — явне "approve"/"reject" — єдиний, що
// може дати Approved.
func awaitReply(ctx context.Context, ch <-chan string, window time.Duration) approvals.Decision {
	timer := time.NewTimer(window)
	defer timer.Stop()

	select {
	case reply, ok := <-ch:
		if !ok {
			return approvals.Decision{Status: approvals.TimedOut, By: "system:timeout",
				Reason: "канал відповідей закрито без відповіді"}
		}
		switch reply {
		case "approve":
			return approvals.Decision{Status: approvals.Approved, By: "human"}
		case "reject":
			return approvals.Decision{Status: approvals.Denied, By: "human", Reason: "людина відмовила"}
		default:
			return approvals.Decision{Status: approvals.Invalid, By: "human",
				Reason: fmt.Sprintf("нерозпізняне слово: %q", reply)}
		}
	case <-ctx.Done():
		return approvals.Decision{Status: approvals.TimedOut, By: "system:timeout",
			Reason: "контекст скасовано: " + ctx.Err().Error()}
	case <-timer.C:
		return approvals.Decision{Status: approvals.TimedOut, By: "system:timeout",
			Reason: fmt.Sprintf("ніхто не відповів протягом %s", window)}
	}
}
