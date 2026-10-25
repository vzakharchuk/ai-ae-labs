// activities.go — усе, що робить I/O: диск, мережа, LLM. Workflow (workflow.go)
// сам цього не робить ("Workflow не виконує I/O") — він лише диспетчерить ці
// методи через workflow.ExecuteActivity за іменем.
package main

import (
	"context"
	"errors"
	"fmt"

	"go.temporal.io/sdk/contrib/googleadk"
	"go.temporal.io/sdk/temporal"

	"google.golang.org/adk/v2/session"

	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/agentdb"
	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/graphmemory"
	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/ledger"
)

// Назви Activity — стабільний контракт між диспетчем у workflow.go/tools.go
// і реєстрацією у app.go. Рядок, не Go-значення: саме так Temporal матчить
// виклик на worker-і.
const (
	ActivityOpenRefundCase  = "OpenRefundCase"
	ActivityLoadSession     = "LoadSession"
	ActivityPersistSession  = "PersistSession"
	ActivityRememberSession = "RememberSession"
	ActivitySearchNodes     = "SearchNodes"
	ActivityOpenNodes       = "OpenNodes"
)

// Рядкові типи ApplicationError.Type — те єдине, що проходить межу
// worker → workflow (не Go-тип). workflow.go/tools.go класифікують помилку
// саме за цим рядком.
const (
	errTypeSemantic  = "SemanticError"
	errTypeTransient = "TransientError"
	errTypeFatal     = "FatalError"
)

// Activities тримає все, що потребує диска, мережі чи LLM.
type Activities struct {
	Desk   *ledger.Desk
	DB     *agentdb.DB
	Memory *graphmemory.Service
}

// OpenRefundCase — durable версія ledger.Desk.OpenRefundCase (п.3, 20 балів).
// RetryPolicy{NonRetryableErrorTypes: ["SemanticError","FatalError"]}
// (app.go, де будується ActivityOptions тулзи) означає: Temporal сам
// повторює лише те, що тут позначено errTypeTransient; semantic/fatal до
// повтору не доходять — вони одразу повертаються у workflow, де
// classifyActivityError (tools.go) перетворює їх назад у Class.
func (a *Activities) OpenRefundCase(ctx context.Context, in ledger.RefundCaseInput) (ledger.RefundCaseOutput, error) {
	out, err := a.Desk.OpenRefundCase(ctx, in)
	if err == nil {
		return out, nil
	}
	class, msg := classifyError(err)
	switch class {
	case ClassSurfaceToModel:
		var se *ledger.SemanticError
		field := ""
		if errors.As(err, &se) {
			field = se.Field
		}
		// field їде як Details — дані, не тип: сам Type() лишається голим
		// рядком "SemanticError", як і вимагає Homework ("лише рядок типу
		// помилки, а не Go-тип").
		return ledger.RefundCaseOutput{}, temporal.NewApplicationErrorWithOptions(msg, errTypeSemantic,
			temporal.ApplicationErrorOptions{NonRetryable: true, Details: []any{field}})
	case ClassRetryTransient:
		return ledger.RefundCaseOutput{}, temporal.NewApplicationErrorWithOptions(msg, errTypeTransient,
			temporal.ApplicationErrorOptions{NonRetryable: false})
	default:
		return ledger.RefundCaseOutput{}, temporal.NewApplicationErrorWithOptions(msg, errTypeFatal,
			temporal.ApplicationErrorOptions{NonRetryable: true})
	}
}

// LoadSession — Activity п.5: знімок сесії з SQLite, або nil, якщо сесії ще
// немає (workflow тоді створює нову). Перший `start` для нового -session —
// це НЕ помилка, а очікуваний стан, тож session.ErrNotFound (database-бекенд
// повертає саме помилку на відсутній запис, не порожню відповідь) явно
// проганяється через errors.Is і стає (nil, nil), а не пропагується: інакше
// Activity-рушій безкінечно повторював би "not found" як транзієнтний збій
// (виявлено прогоном — лог воркера показував зростаючий backoff без кінця).
func (a *Activities) LoadSession(ctx context.Context, userID, sessionID string) (*googleadk.SessionSnapshot, error) {
	resp, err := a.DB.Sessions().Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: sessionID})
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("activities: load session: %w", err)
	}
	if resp == nil || resp.Session == nil {
		return nil, nil // немає попередньої сесії — хід починається з порожньої історії
	}
	return googleadk.ExportSession(ctx, a.DB.Sessions(), appName, userID, sessionID)
}

// PersistSession — Activity п.5: записує знімок сесії в SQLite. Delete перед
// Create (усередині ImportSession), бо повторний `start` з тим самим
// -session повинен ПЕРЕЗАПИСАТИ знімок (у нього більше подій), не впасти на
// дублікаті первинного ключа; Delete нешкідливий, якщо сесії ще не було.
func (a *Activities) PersistSession(ctx context.Context, snap *googleadk.SessionSnapshot) error {
	if snap == nil {
		return nil
	}
	_ = a.DB.Sessions().Delete(ctx, &session.DeleteRequest{AppName: snap.AppName, UserID: snap.UserID, SessionID: snap.SessionID})
	_, err := googleadk.ImportSession(ctx, a.DB.Sessions(), snap)
	return err
}

// RememberSession — Activity п.6: єдиний шлях запису в пам'ять
// (graphmemory.AddSessionToMemory хоче session.Session, не знімок — тож
// знімок відтворюється в тимчасовому in-memory сервісі лише для того, щоб
// отримати це значення; сам запис іде в довготривале сховище, Dgraph).
// Ingestion у graphmemory ідемпотентний за event ID, тож повторний виклик
// при retry Activity нічого не дублює.
func (a *Activities) RememberSession(ctx context.Context, snap *googleadk.SessionSnapshot) error {
	if snap == nil {
		return nil
	}
	s, err := googleadk.ImportSession(ctx, session.InMemoryService(), snap)
	if err != nil {
		return fmt.Errorf("activities: remember session: rehydrate: %w", err)
	}
	return a.Memory.AddSessionToMemory(ctx, s)
}

// SearchNodesInput/OpenNodesInput — аргументи інструментів пам'яті. UserID не
// з моделі (вона не знає, хто питає) — tools.go підставляє його з
// agent.Context перед диспетчем Activity.
type SearchNodesInput struct {
	UserID string
	Query  string
}

type OpenNodesInput struct {
	UserID string
	Names  []string
}

// SearchNodes — Activity-обгортка над graphmemory.Service.SearchNodes.
func (a *Activities) SearchNodes(ctx context.Context, in SearchNodesInput) (graphmemory.KnowledgeGraph, error) {
	return a.Memory.SearchNodes(ctx, appName, in.UserID, in.Query, 0)
}

// OpenNodes — Activity-обгортка над graphmemory.Service.OpenNodes.
func (a *Activities) OpenNodes(ctx context.Context, in OpenNodesInput) (graphmemory.KnowledgeGraph, error) {
	return a.Memory.OpenNodes(ctx, appName, in.UserID, in.Names, 0)
}
