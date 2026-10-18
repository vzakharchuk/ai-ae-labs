// eval_test.go — Homework п.5 (15 балів): 20 запитів (10 single-hop + 10
// multi-hop) з ЦЬОГО корпусу, baseline vs агентний pipeline, latency_p95 і
// tokens_used — вимірювані, top1_relevance і groundedness — правилом, не
// вгадуванням і не LLM-суддею (нема ключа API в цьому графі взагалі — весь
// pipeline не робить жодного LLM-виклику, це retrieval, не генерація):
//
//   - top1_relevance / recall: для кожного запиту заздалегідь відомо, які
//     chunk_id МАЮТЬ з'явитися в цитатах відповіді (golden, нижче). Для
//     single-hop — одна група (мають потрапити); для multi-hop — ДВІ
//     незалежні групи (кожен з двох фактів); relevance = частка груп, які
//     задоволені. Це перевіряється рядковим пошуком "(chunk_id, " у
//     відповіді, не семантичним судженням — прозоро й відтворювано.
//   - groundedness: частка цитат "(chunk_id, document_id)" у відповіді, чий
//     document_id справді збігається з тим, що є в chunksByID для цього
//     chunk_id. І baseline, і агентний pipeline завжди цитують реальні
//     chunk_id з корпусу (жодної генерації тексту немає) — очікується 1.0
//     для обох; вимірюємо, а не постулюємо.
//   - tokens_used: len(strings.Fields(response)) — проксі "скільки довелося
//     б прочитати LLM, якби він був далі в конвеєрі". Немає LLM-викликів
//     у цьому графі, тож це не справжні токени моделі — чесно назвати проксі.
//
// Стаб-ембединги (детерміновано, без мережі — так самий баланс, що і в
// решті тестів): числа відтворювані будь-ким без Ollama. README документує
// ці самі номери як офіційні; окремо (не в CI) README згадує, що реальний
// embeddinggemma на цих самих запитах дає якісно той самий результат.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dimetron/ai-eng-course/labs/internal/labrun"
	"github.com/dimetron/ai-eng-course/labs/week3/Day6_Reranking_Semantic_Cache_Maturity_Ladder/labs6/internal/corpus"
	"github.com/dimetron/ai-eng-course/labs/week3/Day6_Reranking_Semantic_Cache_Maturity_Ladder/labs6/internal/embed"
)

// evalCase — один запит еталонного набору. required — групи взаємозамінних
// chunk_id: для single-hop одна група (будь-який з них задовольняє факт),
// для multi-hop дві групи (обидва факти мають бути знайдені, кожен —
// будь-яким зі своїх альтернативних chunk_id). forbidden — chunk_id, що не
// повинні з'явитися (перевірка на забруднення, як CT-2025-007/T-1 у
// канонічному запиті).
type evalCase struct {
	name      string
	query     string
	multiHop  bool
	required  [][]string
	forbidden []string
}

// evalSet — 10 single-hop + 10 multi-hop запитів із testdata/chunks.json.
// Кожен required — факт, що РЕАЛЬНО є в корпусі (перевірено knowledge_test.go
// і прямим читанням chunks.json), не вигаданий для зручності.
var evalSet = []evalCase{
	// --- single-hop (10) --------------------------------------------------
	{name: "tariff-t2-def", query: "Що таке тариф T-2?", required: [][]string{{"c-tariff-T2"}}},
	{name: "tariff-t1-def", query: "Що таке тариф T-1?", required: [][]string{{"c-tariff-T1"}}},
	{name: "rate-t2", query: "яка ставка комісії на тарифі T-2?", required: [][]string{{"c-tariff-T2", "c-A114-rate", "c-A207-rate"}}},
	{name: "rate-t1", query: "яка ставка комісії на тарифі T-1?", required: [][]string{{"c-tariff-T1", "c-A331-rate"}}},
	{name: "signer-014", query: "хто підписав договір CT-2025-014?", required: [][]string{{"c-sign-014"}}},
	{name: "signer-031", query: "хто підписала договір CT-2025-031?", required: [][]string{{"c-sign-031"}}},
	{name: "signer-007", query: "хто підписав договір CT-2025-007?", required: [][]string{{"c-sign-007"}}},
	{name: "finding-acme", query: "яка знахідка стосується привілейованих облікових записів Acme Bank JSC?", required: [][]string{{"c-F101"}}},
	{name: "availability-northwind", query: "скільки інцидентів недоступності зафіксував Northwind Cloud?", required: [][]string{{"c-availability"}}},
	{name: "nbu-deadline", query: "коли подається звіт про відповідність вимозі НБУ 2026?", required: [][]string{{"c-nbu-deadline"}}},

	// --- multi-hop (10) — дві незалежні групи фактів ----------------------
	{
		name: "canonical-t2-signers", multiHop: true,
		query: "які мерчанти на тарифі T-2 підпадають під вимогу НБУ 2026 і хто підписував їхні договори?",
		required: [][]string{
			{"t-tariffs", "c-A114-rate", "c-A207-rate"},
			{"c-sign-014", "c-sign-031"},
		},
		forbidden: []string{"CT-2025-007"},
	},
	{
		name: "a114-rate-and-signer", multiHop: true,
		query:    "хто підписав договір мерчанта A-114 і яка в нього ставка комісії?",
		required: [][]string{{"c-sign-014"}, {"c-A114-rate", "t-tariffs"}},
	},
	{
		name: "a207-tariff-and-signer", multiHop: true,
		query:    "на якому тарифі мерчант A-207 і хто підписав його договір?",
		required: [][]string{{"c-A207-rate", "t-tariffs"}, {"c-sign-031"}},
	},
	{
		name: "a331-signer-and-nbu", multiHop: true,
		query:    "хто підписав договір мерчанта A-331 і підпадає цей мерчант під вимогу НБУ 2026?",
		required: [][]string{{"c-sign-007"}, {"c-A331-rate", "t-tariffs"}},
	},
	{
		name: "northwind-service-and-incidents", multiHop: true,
		query:    "яку послугу надає Northwind Cloud і скільки в нього інцидентів недоступності?",
		required: [][]string{{"t-vendors"}, {"c-availability"}},
	},
	{
		name: "f101-control-and-auditor", multiHop: true,
		query:    "який контроль порушила знахідка F-101 і хто аудитор Acme Bank JSC?",
		required: [][]string{{"t-findings"}, {"t-vendors"}},
	},
	{
		name: "nbu-requirement-and-deadline", multiHop: true,
		query:    "яка вимога НБУ 2026 для мерчантів на T-2 і коли подається звіт про відповідність?",
		required: [][]string{{"c-nbu-req"}, {"c-nbu-deadline"}},
	},
	{
		name: "globex-auditor-and-finding", multiHop: true,
		query:    "хто аудитор Globex Payments і яка в нього знахідка?",
		required: [][]string{{"t-vendors"}, {"t-findings"}},
	},
	{
		name: "t1-rate-and-signer", multiHop: true,
		query:    "яка ставка на тарифі T-1 і хто підписав договір мерчанта на цьому тарифі?",
		required: [][]string{{"c-tariff-T1", "c-A331-rate"}, {"c-sign-007"}},
	},
	{
		name: "acme-findings-and-severity", multiHop: true,
		query:    "яка знахідка F-104 у Acme Bank JSC і яка в неї серйозність?",
		required: [][]string{{"c-F104"}, {"t-findings"}},
	},
}

func TestEval(t *testing.T) {
	chunks := testChunks(t)
	emb := &embed.Client{} // стаб: детерміновано, без мережі — див. коментар над evalSet

	type row struct {
		name      string
		query     string
		multiHop  bool
		relevance float64
		grounded  float64
		latencyMS float64
		tokens    int
	}

	// Корпус без heading-чанків — вектор-пошук по заголовку ("1 Scope") не
	// має сенсу; той самий фільтр, що й search() у retrieval.go.
	var searchable []corpus.Chunk
	for _, c := range chunks {
		if c.Kind != "heading" {
			searchable = append(searchable, c)
		}
	}
	texts := make([]string, len(searchable))
	for i, c := range searchable {
		texts[i] = c.Text
	}

	// runBaseline — "перед усім агентним": один bi-encoder прохід
	// (embed.Rerank, той самий механізм, що й у rerank() — щоб порівняння
	// було про топологію графа, не про різні ембединги) по ВСЬОМУ корпусу
	// без кешу, без графа, без лексичного guard-у на коди, без budget_gate,
	// топ-1. Це baseline, із яким Homework просить порівняти агентний
	// pipeline — не vectorSearchBranch (та гілка вже ВСЕРЕДИНІ pipeline,
	// навмисно наївний keyword-overlap прототип, не baseline для порівняння).
	runBaseline := func(q string) (string, time.Duration, error) {
		start := time.Now()
		scored, err := embed.Rerank(context.Background(), emb, q, texts)
		if err != nil {
			return "", time.Since(start), err
		}
		if len(scored) == 0 {
			return "Нічого не знайдено.", time.Since(start), nil
		}
		top := searchable[scored[0].Index]
		return fmt.Sprintf("(%s, %s) %s", top.ID, top.DocumentID, top.Text), time.Since(start), nil
	}

	runAgentic := func(q string) (string, time.Duration, error) {
		r, err := newRetriever(chunks, emb)
		if err != nil {
			return "", 0, err
		}
		a, err := newGraph(r)
		if err != nil {
			return "", 0, err
		}
		start := time.Now()
		res, err := labrun.Run(context.Background(), a, q)
		if err != nil {
			return "", time.Since(start), err
		}
		return res.Final, time.Since(start), nil
	}

	byID := make(map[string]string, len(chunks)) // chunk_id -> document_id
	for _, c := range chunks {
		byID[c.ID] = c.DocumentID
	}

	score := func(resp string, c evalCase) (relevance, grounded float64) {
		satisfied := 0
		for _, group := range c.required {
			for _, id := range group {
				if strings.Contains(resp, "("+id+",") {
					satisfied++
					break
				}
			}
		}
		if len(c.required) > 0 {
			relevance = float64(satisfied) / float64(len(c.required))
		}
		for _, bad := range c.forbidden {
			if strings.Contains(resp, bad) {
				relevance -= 0.5 // забруднення карається, не тільки відсутність факту
			}
		}
		cites, valid := 0, 0
		for _, c := range chunks {
			tag := "(" + c.ID + ", " + c.DocumentID + ")"
			if strings.Contains(resp, "("+c.ID+",") {
				cites++
				if strings.Contains(resp, tag) {
					valid++
				}
			}
		}
		if cites == 0 {
			return relevance, 0
		}
		return relevance, float64(valid) / float64(cites)
	}

	var baselineRows, agenticRows []row
	for _, c := range evalSet {
		bResp, bDur, err := runBaseline(c.query)
		if err != nil {
			t.Fatalf("baseline(%s): %v", c.name, err)
		}
		bRel, bGround := score(bResp, c)
		baselineRows = append(baselineRows, row{c.name, c.query, c.multiHop, bRel, bGround, bDur.Seconds() * 1000, len(strings.Fields(bResp))})

		aResp, aDur, err := runAgentic(c.query)
		if err != nil {
			t.Fatalf("agentic(%s): %v", c.name, err)
		}
		aRel, aGround := score(aResp, c)
		agenticRows = append(agenticRows, row{c.name, c.query, c.multiHop, aRel, aGround, aDur.Seconds() * 1000, len(strings.Fields(aResp))})
	}

	summarize := func(rows []row) (meanRel, meanGround, p95Latency, meanTokens float64) {
		var lat []float64
		for _, r := range rows {
			meanRel += r.relevance
			meanGround += r.grounded
			meanTokens += float64(r.tokens)
			lat = append(lat, r.latencyMS)
		}
		n := float64(len(rows))
		meanRel, meanGround, meanTokens = meanRel/n, meanGround/n, meanTokens/n
		sort.Float64s(lat)
		idx := int(0.95 * float64(len(lat)))
		if idx >= len(lat) {
			idx = len(lat) - 1
		}
		return meanRel, meanGround, lat[idx], meanTokens
	}

	bRel, bGround, bP95, bTok := summarize(baselineRows)
	aRel, aGround, aP95, aTok := summarize(agenticRows)

	var sb strings.Builder
	fmt.Fprintf(&sb, "| Метрика | Baseline (vector-RAG, топ-1) | Агентний pipeline |\n")
	fmt.Fprintf(&sb, "|---|---|---|\n")
	fmt.Fprintf(&sb, "| top1_relevance / recall (mean, 20 запитів) | %.2f | %.2f |\n", bRel, aRel)
	fmt.Fprintf(&sb, "| groundedness (mean) | %.2f | %.2f |\n", bGround, aGround)
	fmt.Fprintf(&sb, "| latency_p95, ms | %.2f | %.2f |\n", bP95, aP95)
	fmt.Fprintf(&sb, "| tokens_used (проксі, mean слів у відповіді) | %.1f | %.1f |\n", bTok, aTok)
	sb.WriteString("\n| # | Запит | Тип | rel (base) | rel (agentic) |\n|---|---|---|---|---|\n")
	for i, c := range evalSet {
		kind := "single-hop"
		if c.multiHop {
			kind = "multi-hop"
		}
		fmt.Fprintf(&sb, "| %d | %s | %s | %.2f | %.2f |\n", i+1, c.name, kind, baselineRows[i].relevance, agenticRows[i].relevance)
	}

	t.Log("\n" + sb.String())
	if err := os.WriteFile("eval_results.md", []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("writing eval_results.md: %v", err)
	}
}
