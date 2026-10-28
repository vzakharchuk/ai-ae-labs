// Каркас тестів ДЗ 10. Він компілюється й проходить одразу, тому перший
// `go test` у цій лабі показує «ok … [skipped]», а не «[no test files]».
//
// Різниця не косметична. «Немає тестових файлів» виглядає як успіх — і
// рівно тому нікого не змушує подивитись уважніше. Це та сама помилка,
// яку ми в Блоці 4 називаємо про затвор: результат без названої причини
// гірший за помилку.
//
// Усі три тести нижче — скелети під конкретні пункти ДЗ. Зніміть t.Skip,
// коли писатимете тіло.
//
//	go test ./...                    # зелено, скіпи видно
//	go test -run Approval -v ./...
//
// Жоден тест цієї лаби не має вимагати GOOGLE_API_KEY: те, що варто
// перевіряти — топологія графа, політика затвора, стримування збою —
// перевіряється без моделі. Якщо ваш тест не запускається без ключа,
// ви тестуєте Gemini, а не свій код.
package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/workflow"

	"github.com/dimetron/ai-eng-course/labs/internal/labrun"
	"github.com/dimetron/ai-eng-course/labs/week5/Day10_Collaboration_Agents_HITL/labs10/internal/approvals"
)

// TestJoinNodeWaitsForAllBranches — ДЗ 10, п.2 (fan-out / fan-in, 20 балів).
//
// Що довести: JoinNode — це БАР'ЄР. Він активується лише тоді, коли
// відзвітували ВСІ попередники, перелічені в AddFanIn. Найчастіша
// помилка — перелічити не всіх, і тоді граф тихо йде далі з неповним
// входом.
//
// Каркас: мінімальний граф із двох гілок (не весь engine.go — щоб помилка
// в НАШІЙ топології не змінювала сенс цього тесту), перевіряє, що merge
// бачить ОБИДВА ключі.
func TestJoinNodeWaitsForAllBranches(t *testing.T) {
	a := workflow.NewFunctionNode("a", func(_ agent.Context, _ string) (string, error) { return "A", nil }, workflow.NodeConfig{})
	b := workflow.NewFunctionNode("b", func(_ agent.Context, _ string) (string, error) { return "B", nil }, workflow.NodeConfig{})
	join := workflow.NewJoinNode("join")
	var got map[string]any
	collect := workflow.NewFunctionNode("collect", func(_ agent.Context, in map[string]any) (string, error) {
		got = in
		return "ok", nil
	}, workflow.NodeConfig{})

	eb := workflow.NewEdgeBuilder()
	eb.AddFanOut(workflow.Start, a, b)
	eb.AddFanIn(join, a, b)
	eb.Add(join, collect)

	ag, err := workflowagent.New(workflowagent.Config{Name: "join_test", Edges: eb.Build()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := labrun.Run(context.Background(), ag, "go"); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["a"]; !ok {
		t.Error("merge не бачив гілку \"a\" — JoinNode активувався до того, як вона відзвітувала")
	}
	if _, ok := got["b"]; !ok {
		t.Error("merge не бачив гілку \"b\" — JoinNode активувався до того, як вона відзвітувала")
	}
	if len(got) != 2 {
		t.Errorf("merge отримав %d ключів, want 2: %v", len(got), got)
	}
}

// TestSpecialistFailureDoesNotCorruptTheAnswer — ДЗ 10, п.6 (containment).
//
// Сильна форма твердження: навіть якщо людина СХВАЛИЛА, документ з
// неперевіреним розділом не відправляється. decideSend (gate.go) — чиста
// функція, яку перевіряємо напряму, без збирання графа.
func TestSpecialistFailureDoesNotCorruptTheAnswer(t *testing.T) {
	store, err := approvals.Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	doc := Document{Sections: []Section{
		{Node: "risk_desk", Status: "verified", Findings: []Finding{{Claim: "ок", Source: "x"}}},
		{Node: "integration_desk", Status: "unverified", Error: ErrIntegrationDeskDown.Error()},
	}}
	doc.Hash = hashDocument(doc)

	in := GateDecision{Doc: doc, ID: "a", Status: approvals.Approved, By: "oksana"}
	out, err := decideSend(context.Background(), store, in)
	if err != nil {
		t.Fatalf("decideSend: %v", err)
	}
	if !strings.Contains(out, "ВІДМОВЛЕНО") || !strings.Contains(out, "integration_desk") {
		t.Fatalf("approved-але-unverified мало бути блоковано з названою причиною, отримали: %q", out)
	}

	// Контроль: той самий механізм, усі розділи verified — має пройти.
	// decideSend(Approved) викликає store.Spend, а Spend вимагає реального
	// запису в журналі (Ask + Decide), не лише значення Status у GateDecision
	// — та сама перевірка, що в продакшн-коді: approve "з повітря" не існує.
	clean := Document{Sections: []Section{
		{Node: "risk_desk", Status: "verified", Findings: []Finding{{Claim: "ок", Source: "x"}}},
		{Node: "integration_desk", Status: "verified", Findings: []Finding{{Claim: "ок2", Source: "y"}}},
	}}
	clean.Hash = hashDocument(clean)
	if _, err := store.Ask(context.Background(), approvals.Request{
		InterruptID: "b", SessionID: "s1", Action: "send_plan_to_board", ArtifactURI: "memo://x",
		Hash: clean.Hash, Recipients: []string{"oksana@ledgerworks.ua", "board@ledgerworks.ua"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Decide(context.Background(), "b", approvals.Decision{Status: approvals.Approved, By: "oksana"}); err != nil {
		t.Fatal(err)
	}
	out, err = decideSend(context.Background(), store, GateDecision{Doc: clean, ID: "b", Status: approvals.Approved, By: "oksana"})
	if err != nil {
		t.Fatalf("decideSend(clean): %v", err)
	}
	if !strings.Contains(out, "ВІДПРАВЛЕНО") {
		t.Errorf("повністю verified документ мав пройти, отримали: %q", out)
	}
}

// TestApprovalTimeoutMeansRefusal — ДЗ 10, п.4 (вікно рішення, 5 балів).
//
// Чотири шляхи ведуть у відмову, кожен зі своєю причиною: вікно
// вичерпалось, контекст скасовано, канал закрито без відповіді,
// нерозпізнане слово. П'ятий і шостий — явні "approve"/"reject" — єдині,
// що дають не-відмову чи явну відмову людини.
func TestApprovalTimeoutMeansRefusal(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		ch := make(chan string)
		d := awaitReply(context.Background(), ch, 20*time.Millisecond)
		if d.Status != approvals.TimedOut || d.Reason == "" {
			t.Errorf("d = %+v, want TimedOut з причиною", d)
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ch := make(chan string)
		d := awaitReply(ctx, ch, time.Minute)
		if d.Status != approvals.TimedOut || d.Reason == "" {
			t.Errorf("d = %+v, want TimedOut з причиною (скасований контекст)", d)
		}
	})
	t.Run("closed channel", func(t *testing.T) {
		ch := make(chan string)
		close(ch)
		d := awaitReply(context.Background(), ch, time.Minute)
		if d.Status != approvals.TimedOut || d.Reason == "" {
			t.Errorf("d = %+v, want TimedOut з причиною (закритий канал)", d)
		}
	})
	t.Run("unrecognized word", func(t *testing.T) {
		ch := make(chan string, 1)
		ch <- "maybe"
		d := awaitReply(context.Background(), ch, time.Minute)
		if d.Status != approvals.Invalid || d.Reason == "" {
			t.Errorf("d = %+v, want Invalid з причиною", d)
		}
	})
	t.Run("explicit reject", func(t *testing.T) {
		ch := make(chan string, 1)
		ch <- "reject"
		d := awaitReply(context.Background(), ch, time.Minute)
		if d.Status != approvals.Denied {
			t.Errorf("d = %+v, want Denied", d)
		}
	})
	t.Run("explicit approve", func(t *testing.T) {
		ch := make(chan string, 1)
		ch <- "approve"
		d := awaitReply(context.Background(), ch, time.Minute)
		if d.Status != approvals.Approved {
			t.Errorf("d = %+v, want Approved", d)
		}
	})
}

// TestRiskToolReturnsOnlySourcedFindings — ранній win, без моделі: кожен факт
// інструмента має джерело. Спеціаліст, який переказує факт без джерела, —
// початок вигаданого розділу.
func TestRiskToolReturnsOnlySourcedFindings(t *testing.T) {
	out, err := lookupRisk(nil, riskInput{Topic: "міграція"})
	if err != nil || len(out.Findings) == 0 {
		t.Fatalf("lookupRisk: %v %+v", err, out)
	}
	for _, f := range out.Findings {
		if f.Source == "" {
			t.Errorf("факт без джерела: %+v", f)
		}
	}
}
