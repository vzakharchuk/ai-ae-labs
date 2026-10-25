// harness.go — серце Fault-Tolerant Tool Harness: класифікація помилок
// (п.1) і бюджет самокорекції (п.2). Чисті функції, нуль I/O — тож їх
// покриває звичайна табличний тест (main_test.go), без Temporal і Docker.
package main

import (
	"context"
	"errors"
	"fmt"

	"go.temporal.io/sdk/temporal"

	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/ledger"
)

// Class — що harness робить із помилкою.
type Class int

const (
	ClassRetryTransient Class = iota + 1 // віддати рушію: повтор допоможе
	ClassSurfaceToModel                  // повернути моделі як Observation: повтор не допоможе
	ClassFatal                           // зупинитись
)

// maxSelfCorrections — скільки семантичних помилок один хід може перетворити
// на Observation, перш ніж harness зупинить ран.
const maxSelfCorrections = 3

// ErrSelfCorrectionExhausted обгортається помилкою, що закінчує такий ран.
var ErrSelfCorrectionExhausted = errors.New("self-correction budget exhausted")

// classifyError — ПУНКТ 1 (15 балів).
//
// Клас визначається через errors.Is/errors.As, не через err.Error(): рядок
// може змінитись за обгорткою (%w) або відрізнятись між провайдерами, а
// errors.Is/As бачить крізь будь-яку кількість обгорток.
//
// context.Canceled перевіряється ПЕРШИМ і окремо від DeadlineExceeded: це
// сигнал зупинитись (той, хто скасував контекст, уже вирішив, що робити
// далі), а не привід повторити виклик — на відміну від DeadlineExceeded
// (наш власний таймаут), де повтор ще має сенс.
func classifyError(err error) (Class, string) {
	if err == nil {
		return 0, ""
	}
	msg := err.Error()

	if errors.Is(err, context.Canceled) {
		return ClassFatal, msg
	}

	var se *ledger.SemanticError
	if errors.As(err, &se) || errors.Is(err, ledger.ErrSemantic) {
		return ClassSurfaceToModel, msg
	}

	if errors.Is(err, ledger.ErrTransient) || errors.Is(err, context.DeadlineExceeded) {
		return ClassRetryTransient, msg
	}

	return ClassFatal, msg // невідому помилку наосліп не повторюють
}

// Observation — те, що модель читає після виклику, який не вдався.
type Observation struct {
	Status      string `json:"status"` // invalid_argument | unavailable | stopped
	Field       string `json:"field,omitempty"`
	Observation string `json:"observation"`
	Attempt     int    `json:"attempt,omitempty"`
}

// budget рахує самокорекції в межах одного ходу. Він живе в стані ходу, а не в
// глобальній змінній: глобальний лічильник ламає паралельні тести.
type budget struct{ used int }

// observe — ПУНКТ 2 (15 балів).
//
// Семантична помилка не повертається як error — вона стає Observation, яку
// модель читає й виправляє свій наступний хід. retry() (tools.go) уже
// вичерпав повтори на все, що classifyError назвав транзієнтним, тож якщо
// сюди дійшла транзієнтна помилка — рушій здався, і це для моделі
// "unavailable", не привід повторити ще раз.
//
// Це шлях без Temporal (крок 0, main.go) — помилка тут завжди справжній
// Go-тип ledger.SemanticError, і errors.As бачить його поле Field прямо.
// observeActivity (нижче) — той самий бюджет для шляху, де ця помилка вже
// перетнула межу Activity й лишила по собі лише рядок типу.
func (b *budget) observe(err error) (Observation, error) {
	class, msg := classifyError(err)
	var se *ledger.SemanticError
	field := ""
	if errors.As(err, &se) {
		field = se.Field
	}
	return b.fromClass(class, msg, field, err)
}

// observeActivity — те саме, що observe, для помилки, що прийшла з
// OpenRefundCase Activity (activities.go) через межу worker → workflow:
// Go-тип не перетнув межу, тож клас і field читаються з
// classifyActivityError (tools.go), не з errors.As на ledger.SemanticError.
func (b *budget) observeActivity(err error) (Observation, error) {
	class, msg, field := classifyActivityError(err)
	return b.fromClass(class, msg, field, err)
}

// fromClass — спільне тіло observe/observeActivity: бюджет самокорекцій і
// форма Observation однакові для обох шляхів, різниться лише те, звідки
// взято class/msg/field.
func (b *budget) fromClass(class Class, msg, field string, err error) (Observation, error) {
	switch class {
	case ClassSurfaceToModel:
		b.used++
		if b.used > maxSelfCorrections {
			return Observation{}, fmt.Errorf("after %d self-corrections: %w", b.used-1, ErrSelfCorrectionExhausted)
		}
		return Observation{
			Status:      "invalid_argument",
			Field:       field,
			Observation: "ERROR: " + msg + " — please correct your arguments and try again",
			Attempt:     b.used,
		}, nil
	case ClassRetryTransient:
		return Observation{Status: "unavailable", Observation: msg}, nil
	default: // ClassFatal
		return Observation{}, err
	}
}

// classifyActivityError — те саме рішення, що classifyError, але за рядком
// ApplicationError.Type(), не за Go-типом: лише рядок типу перетинає межу
// worker → workflow (Homework, п.3 — "через межу... проходить лише рядок
// типу помилки, а не Go-тип"). field приходить через Details (дані, не
// тип) — Activities.OpenRefundCase кладе його туди саме для цього.
//
// *temporal.TimeoutError — окремий випадок, не ApplicationError: коли
// StartToClose/ScheduleToStart вичерпується сам (а не наша класифікована
// помилка всередині), рушій повертає цей тип. Виявлено реальним прогоном
// crash-safe демо: без цієї перевірки такий таймаут падав у default нижче
// (ClassFatal) і модель бачила сирий текст Temporal-помилки замість
// Observation{Status: "unavailable"} — те саме, що exhausted-транзієнтна
// помилка мала би дати.
func classifyActivityError(err error) (class Class, msg, field string) {
	if err == nil {
		return 0, "", ""
	}
	if errors.Is(err, context.Canceled) {
		return ClassFatal, err.Error(), ""
	}
	var timeoutErr *temporal.TimeoutError
	if errors.As(err, &timeoutErr) {
		return ClassRetryTransient, err.Error(), ""
	}
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		msg = appErr.Message()
		switch appErr.Type() {
		case errTypeSemantic:
			var f string
			_ = appErr.Details(&f)
			return ClassSurfaceToModel, msg, f
		case errTypeFatal:
			return ClassFatal, msg, ""
		case errTypeTransient:
			return ClassRetryTransient, msg, ""
		}
	}
	return ClassFatal, err.Error(), "" // невідомий тип — наосліп не повторюємо
}
