package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/ledger"
)

// TestClassifyError — таблиця з 6 рядків (підказка Homework.md, "Тестуйте
// classifyError окремо від усього іншого"): чиста функція, жодної інфри не
// потребує. Кожен рядок перевіряє клас через errors.Is/As контракт, не
// текст повідомлення.
func TestClassifyError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want Class
	}{
		{"canceled is fatal, not retried", context.Canceled, ClassFatal},
		{"our own deadline is retried", context.DeadlineExceeded, ClassRetryTransient},
		{"ledger transient fault is retried", ledger.Timeout.Err("open_refund_case"), ClassRetryTransient},
		{"semantic error surfaces to the model", &ledger.SemanticError{Field: "merchant_id", Reason: "bad"}, ClassSurfaceToModel},
		{"transient wrapped through %w is still retried", fmt.Errorf("desk: %w", ledger.Unavailable.Err("op")), ClassRetryTransient},
		{"unknown error is fatal, not retried blindly", errors.New("disk on fire"), ClassFatal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got, _ := classifyError(tt.err); got != tt.want {
				t.Errorf("classifyError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestBudgetObserve_SemanticBecomesObservation — семантична помилка не йде
// назовні як error: вона стає Observation, яку модель читає.
func TestBudgetObserve_SemanticBecomesObservation(t *testing.T) {
	t.Parallel()
	b := &budget{}
	obs, err := b.observe(&ledger.SemanticError{Field: "merchant_id", Reason: `"undefined" was not extracted from the request`})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if obs.Status != "invalid_argument" || obs.Field != "merchant_id" || obs.Attempt != 1 {
		t.Fatalf("obs = %+v", obs)
	}
	if want := `ERROR: invalid merchant_id: "undefined" was not extracted from the request — please correct your arguments and try again`; obs.Observation != want {
		t.Errorf("observation = %q, want %q", obs.Observation, want)
	}
}

// TestBudgetObserve_ExhaustsAfterMaxSelfCorrections — maxSelfCorrections
// семантичних помилок стають Observation; наступна (четверта) — явна помилка
// з ErrSelfCorrectionExhausted, не тиха зупинка.
func TestBudgetObserve_ExhaustsAfterMaxSelfCorrections(t *testing.T) {
	t.Parallel()
	b := &budget{}
	semErr := &ledger.SemanticError{Field: "merchant_id", Reason: "bad"}
	for i := 1; i <= maxSelfCorrections; i++ {
		if _, err := b.observe(semErr); err != nil {
			t.Fatalf("attempt %d: observe returned error, want Observation: %v", i, err)
		}
	}
	_, err := b.observe(semErr)
	if !errors.Is(err, ErrSelfCorrectionExhausted) {
		t.Fatalf("err = %v; want errors.Is(err, ErrSelfCorrectionExhausted)", err)
	}
}

// TestBudgetObserve_ExhaustedTransientIsUnavailable — транзієнтна помилка,
// що дійшла до observe (тобто retry() уже вичерпав свої спроби), стає
// Observation{Status: "unavailable"}, а не ще однією спробою.
func TestBudgetObserve_ExhaustedTransientIsUnavailable(t *testing.T) {
	t.Parallel()
	b := &budget{}
	obs, err := b.observe(ledger.Unavailable.Err("open_refund_case"))
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if obs.Status != "unavailable" {
		t.Errorf("status = %q, want %q", obs.Status, "unavailable")
	}
}

// TestBudgetObserve_FatalPropagates — фатальна помилка йде назовні як error,
// не ховається в Observation.
func TestBudgetObserve_FatalPropagates(t *testing.T) {
	t.Parallel()
	b := &budget{}
	want := errors.New("disk on fire")
	_, err := b.observe(want)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}
