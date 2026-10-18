package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"

	"github.com/dimetron/ai-eng-course/labs/week3/Day6_Reranking_Semantic_Cache_Maturity_Ladder/labs6/internal/corpus"
	"github.com/dimetron/ai-eng-course/labs/week3/Day6_Reranking_Semantic_Cache_Maturity_Ladder/labs6/internal/embed"
	"github.com/dimetron/ai-eng-course/labs/week3/Day6_Reranking_Semantic_Cache_Maturity_Ladder/labs6/internal/kgraph"
	"github.com/dimetron/ai-eng-course/labs/week3/Day6_Reranking_Semantic_Cache_Maturity_Ladder/labs6/internal/systemone"
)

// Candidate — кандидат retrieval з оцінкою і provenance.
type Candidate struct {
	ChunkID     string  `json:"chunk_id"`
	DocumentID  string  `json:"document_id"`
	SectionPath string  `json:"section_path,omitempty"`
	Text        string  `json:"text"`
	Score       float64 `json:"score"`
}

// SearchResult — запит + кандидати, тече між вузлами графа.
type SearchResult struct {
	Query      string      `json:"query"`
	Candidates []Candidate `json:"candidates"`
	CacheHit   bool        `json:"cache_hit"`
	// MultiHop — класифікація із classify, пронесена через fan-out і join:
	// answer() бере більше кандидатів у фінальну відповідь для multi-hop
	// запиту (Homework п.2 — обидва факти, не тільки топ-1).
	MultiHop bool `json:"multi_hop"`
}

// ClassifiedQuery — вихід classify, вхід усіх трьох fan-out гілок
// (vector_search / graph_filter / graph_traversal): той самий запит плюс
// позначка типу. Усі три гілки мають приймати ОДИН тип входу (їх живить той
// самий AddFanOut-вихід), тому MultiHop їде поруч із Query, а не окремим
// вузлом-розгалуженням — Homework-спрощення з graph_traversal (він і так
// проходить УВЕСЬ граф Contract→Signer незалежно від хопів) означає, що
// розгалуження тут нічого б не фільтрувало.
type ClassifiedQuery struct {
	Query    string `json:"query"`
	MultiHop bool   `json:"multi_hop"`
}

// topK — скільки кандидатів search віддає в rerank.
const topK = 5

// codeRE знаходить короткі ідентифікатори цього корпусу: "T-2", "A-114",
// "CT-2025-014", "F-101" — буква(и) + дефіс + цифри. Вони — єдиний надійний
// сигнал, що розрізняє "тариф T-2" і "тариф T-1": ембединги (і стаб, і
// реальний embeddinggemma — див. internal/embed і internal/systemone) дають
// на цій парі майже однаковий score, бо речення різняться рівно одним
// токеном в інакше ідентичному шаблоні. Код — дешевий точний сигнал там, де
// косинус його не бачить.
var codeRE = regexp.MustCompile(`(?i)\b([a-zа-яіїєґ]+)-(\d+(?:-\d+)*)\b`)

// code — один розпізнаний ідентифікатор: префікс (тип, "t", "a", "ct", "f")
// і значення.
type code struct {
	prefix string
	value  string
}

func codesIn(s string) []code {
	var out []code
	for _, m := range codeRE.FindAllStringSubmatch(s, -1) {
		out = append(out, code{prefix: strings.ToLower(m[1]), value: m[2]})
	}
	return out
}

// codesConflict — true, якщо обидва набори НЕПОРОЖНІ й не мають жодного
// спільного коду (той самий префікс І те саме значення).
//
// Було вужче — лише "однаковий префікс, різне значення" (T-2 проти T-1).
// Цього не досить: реально виміряний false positive — "хто підписав договір
// мерчанта A-114?" (код a-114) проти канонічного запиту про T-2 (код t-2) —
// різні ПРЕФІКСИ, стара перевірка такого не бачить, а embeddinggemma дав
// достатньо високий cosine, щоб кеш віддав відповідь про A-114 на запит про
// T-2. Новий, суворіший варіант: немає жодного спільного коду взагалі — не
// кешуємо. Ціна — трохи більше miss там, де той самий факт згадано різними
// кодами (контракт CT-2025-014 замість мерчанта A-114), але ДЗ прямо каже:
// "кеш — межа безпеки", а не намагання максимізувати hit-rate.
func codesConflict(a, b []code) bool {
	if len(a) == 0 || len(b) == 0 {
		return false // немає сутності для порівняння з жодного боку — нічого стверджувати
	}
	for _, x := range a {
		for _, y := range b {
			if x.prefix == y.prefix && x.value == y.value {
				return false // є спільний код — не конфлікт
			}
		}
	}
	return true // обидва називають сутності, і жодна не збігається
}

// codeMatchBoost / codeMismatchPenalty — наскільки лексичний збіг/конфлікт
// кодів зсуває bi-encoder score в rerank. Значення підібрані так, щоб
// перекрити вимірювану різницю між T-1 і T-2 на стабі (~0.01 — див. README,
// таблицю "до/після"): боту вистачає й 1.0.
const (
	codeMatchBoost      = 1.0
	codeMismatchPenalty = 1.0
)

func lexicalAdjust(queryCodes []code, text string) float64 {
	var adj float64
	for _, tc := range codesIn(text) {
		for _, qc := range queryCodes {
			if tc.prefix != qc.prefix {
				continue
			}
			if tc.value == qc.value {
				adj += codeMatchBoost
			} else {
				adj -= codeMismatchPenalty
			}
		}
	}
	return adj
}

// semanticCacheThreshold — порог cosine для r.emb.Embed + embed.Cosine.
// Виміряно на обох бекендах (internal/embed), не вгадано:
//
//	                                              stub     embeddinggemma (Ollama)
//	перефразування того самого запиту:            0.47     0.895
//	інше питання, та сама сутність (T-2):         0.33–0.38  0.277
//	зовсім інша тема:                             —        0.302
//	той самий шаблон, інший тариф (T-2 → T-1):    0.88     0.895  ← те саме число, що й перефразування!
//
// 0.45 лежить у розриві між "перефразування" (0.47 / 0.90) і "інше питання"
// (0.33–0.38 / 0.28–0.30) на обох бекендах — отже й тест на стабі, і реальний
// Ollama-поріг узгоджені, без окремого калібрування під кожен.
//
// Останній рядок таблиці — чому поріг сам не вирішує все: "тариф T-2" і
// "тариф T-1" відрізняються одним токеном в однаковому шаблоні, і жоден з
// двох бекендів за cosine їх не розрізняє (обидва дають те саме число, що й
// справжній перефразування). codesConflict нижче — окрема, обов'язкова
// перевірка ПЕРЕД порогом, а не частина його: поріг ловить "про що питання",
// коди ловлять "про яку сутність".
const semanticCacheThreshold = 0.45

// cacheEntry — один запис кешу: запит, його вектор (для cosine) і відповідь.
type cacheEntry struct {
	query    string
	vec      []float32
	codes    []code
	multiHop bool
	answer   string
}

// answerCache — семантичний кеш відповідей. Безпечний для одночасних
// викликів: fan-out і web-режим звертаються до нього паралельно.
//
// Межі (документовано чесно, а не приховано):
//   - один tenant, одна версія корпусу, без TTL — процес живе, доки живе
//     програма; перезапуск очищає кеш. Lab 5-стиль "process restart loses
//     the graph" застосовується і тут.
//   - лінійний скан: O(n) порівнянь на запит. Для корпусу цього розміру
//     (десятки записів) це непомітно; для production — векторний індекс
//     (вустар/HNSW), не цей файл.
type answerCache struct {
	mu      sync.Mutex
	entries []cacheEntry
	cross   *systemone.Client // опційний cross-encoder-дискримінатор; nil — вимкнено
}

// embedOne — один текст, один вектор; обгортка над emb.Embed для читабельності
// викликів у get/put.
func embedOne(ctx context.Context, emb embedder, text string) ([]float32, error) {
	vecs, err := emb.Embed(ctx, []string{text})
	if err != nil {
		return nil, fmt.Errorf("cache embed: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("cache embed: отримали %d векторів на 1 текст", len(vecs))
	}
	return vecs[0], nil
}

// get шукає семантично схожий запит у кеші.
//
// Три рівні, у порядку пріоритету:
//  1. точний збіг нормалізованого рядка — швидкий шлях, без ембединг-викликів;
//  2. якщо задано SYSTEMONE_URL — systemone.Same(query, cached.query): питання
//     "чи одна відповідь підходить обом?" безпосередньо, без порогу (документація
//     systemone explicitly каже, що це надійніше за cosine на цій же задачі);
//  3. інакше — embed.Cosine(r.emb.Embed(query), збережений вектор) проти
//     semanticCacheThreshold.
//
// codesConflict — обов'язковий guard, що йде ПЕРЕД будь-яким з трьох рівнів
// (крім #1, де рядки вже ідентичні за визначенням): запит про T-1 ніколи не
// отримає відповідь, закешовану під T-2, хай яким високим вийшов би score.
func (c *answerCache) get(ctx context.Context, emb embedder, query string) (string, bool, error) {
	norm := normalize(query)
	qCodes := codesIn(query)
	qMultiHop := multiHopRE.MatchString(query)

	c.mu.Lock()
	entries := append([]cacheEntry(nil), c.entries...)
	c.mu.Unlock()

	for _, e := range entries {
		if normalize(e.query) == norm {
			return e.answer, true, nil
		}
	}

	var vec []float32
	for _, e := range entries {
		if codesConflict(qCodes, e.codes) {
			continue
		}
		if qMultiHop != e.multiHop {
			// Той самий тариф, різна форма питання: "яка ставка на T-2?"
			// (один факт) і "які мерчанти на T-2 ... і хто підписував?"
			// (два факти) — реально вимірювано embeddinggemma дає їм
			// cosine 0.51, вище semanticCacheThreshold (0.45), бо обидва
			// про T-2 і написані схожими словами. codesConflict тут не
			// спрацює (код той самий, не різний) — потрібен окремий guard
			// саме на форму запиту, інакше multi-hop запит отримає
			// однофактну закешовану відповідь і втратить підписантів.
			continue
		}
		if c.cross != nil {
			p, err := c.cross.Same(ctx, query, e.query)
			if err != nil {
				return "", false, fmt.Errorf("cache: systemone same: %w", err)
			}
			if p >= 0.5 {
				return e.answer, true, nil
			}
			continue
		}
		if vec == nil {
			var err error
			vec, err = embedOne(ctx, emb, query)
			if err != nil {
				return "", false, err
			}
		}
		if sim := embed.Cosine(vec, e.vec); sim >= semanticCacheThreshold {
			return e.answer, true, nil
		}
	}
	return "", false, nil
}

func (c *answerCache) put(ctx context.Context, emb embedder, query, answer string) error {
	vec, err := embedOne(ctx, emb, query)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, cacheEntry{query: query, vec: vec, codes: codesIn(query), multiHop: multiHopRE.MatchString(query), answer: answer})
	return nil
}

func normalize(q string) string {
	return strings.Join(tokens(q), " ")
}

// defaultChunks — корпус стартера: ті самі LEDGERWORKS-документи, що в ДЗ 5,
// уже нарізані у форматі labs5.Chunk. Шлях відносний — запускайте з теки labs6.
const defaultChunks = "testdata/chunks.json"

// chunksPath обирає корпус: змінна CHUNKS (ваш індекс із ДЗ 5) або стартовий.
func chunksPath(env string) string {
	if env != "" {
		return env
	}
	return defaultChunks
}

// embedder перетворює тексти на вектори — по одному на текст, у тому ж порядку.
//
// Реалізація — internal/embed: за замовчуванням чистий Go (детермінований
// стаб, без мережі), Ollama — якщо задано OLLAMA_EMBED_MODEL
// (https://docs.ollama.com/capabilities/embeddings). Там же embed.Cosine і
// embed.Rerank. Стаб — лише для wiring і тестів: пороги калібруйте на Ollama.
type embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// retriever тримає корпус, кеш, embedder і знаннєвий граф; його методи —
// вузли графа.
type retriever struct {
	chunks     []corpus.Chunk
	chunksByID map[string]corpus.Chunk
	cache      answerCache
	emb        embedder
	// cross — опційний cross-encoder (decision-модель tev1:4b через Ollama
	// /v1/systemone). nil, якщо SYSTEMONE_URL не задано: систему тоді веде
	// обов'язковий bi-encoder embed.Rerank + лексичний guard на коди.
	cross *systemone.Client
	// graph — Merchant → Tariff/Contract → Signer (knowledge.go), для
	// graph_filter і graph_traversal.
	graph *kgraph.Graph
	// signerChunk — "<contractID>|<signerID>" → chunk id, для точної
	// цитати в graph_traversal (signerProvenance, knowledge.go).
	signerChunk map[string]string
}

func newRetriever(chunks []corpus.Chunk, emb embedder) (*retriever, error) {
	cross := systemone.New()
	g, err := buildGraph(chunks)
	if err != nil {
		return nil, fmt.Errorf("newRetriever: %w", err)
	}
	byID := make(map[string]corpus.Chunk, len(chunks))
	for _, c := range chunks {
		byID[c.ID] = c
	}
	return &retriever{
		chunks:      chunks,
		chunksByID:  byID,
		emb:         emb,
		cache:       answerCache{cross: cross},
		cross:       cross,
		graph:       g,
		signerChunk: signerProvenance(chunks),
	}, nil
}

// search шукає топ-K кандидатів по чанках із ДЗ 5.
//
// Зараз це BASELINE: частка слів запиту, які є в чанку (keyword overlap).
// Він навмисно наївний — на «яка ставка на тарифі T-2?» чанк про T-1 набирає
// майже стільки ж, скільки чанк про T-2 (TestBaseline_ConfusesTariffs) —
// саме це rerank() нижче виправляє.
func (r *retriever) search(ctx agent.Context, query string) (SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return SearchResult{}, fmt.Errorf("порожній запит")
	}
	if answer, ok, err := r.cache.get(ctx, r.emb, query); err != nil {
		return SearchResult{}, err
	} else if ok {
		return SearchResult{Query: query, CacheHit: true, Candidates: []Candidate{{ChunkID: "cache", Text: answer, Score: 1}}}, nil
	}
	q := tokens(query)
	var out []Candidate
	for _, c := range r.chunks {
		if c.Kind == "heading" {
			continue
		}
		if s := overlap(q, tokens(c.Text)); s > 0 {
			out = append(out, Candidate{ChunkID: c.ID, DocumentID: c.DocumentID, SectionPath: c.SectionPath, Text: c.Text, Score: s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > topK {
		out = out[:topK]
	}
	return SearchResult{Query: query, Candidates: out}, nil
}

// rerank переранжовує кандидатів.
//
// Два шари:
//  1. обов'язковий bi-encoder embed.Rerank (cosine між ембедингом запиту й
//     кожного чанка) + лексичний guard на коди (lexicalAdjust) — коди
//     потрібні, бо вимірювано (README, таблиця "до/після"): і стаб, і
//     реальний embeddinggemma дають "тариф T-2" і "тариф T-1" майже
//     однаковий score, бо речення різняться рівно одним токеном в інакше
//     ідентичному шаблоні;
//  2. опційний cross-encoder — systemone (tev1:4b через Ollama,
//     SYSTEMONE_URL), якщо задано: він бачить пару (запит, уривок) цілком і
//     зазвичай точніший за bi-encoder. Коли SYSTEMONE_URL не задано, cross
//     == nil і цей шар просто не виконується (systemone.New() документовано
//     повертає nil — "decision model is opt-in").
func (r *retriever) rerank(ctx agent.Context, in SearchResult) (SearchResult, error) {
	if len(in.Candidates) < 2 {
		return in, nil
	}
	texts := make([]string, len(in.Candidates))
	for i, c := range in.Candidates {
		texts[i] = c.Text
	}
	scored, err := embed.Rerank(ctx, r.emb, in.Query, texts)
	if err != nil {
		return SearchResult{}, fmt.Errorf("rerank: %w", err)
	}
	queryCodes := codesIn(in.Query)
	out := make([]Candidate, len(scored))
	for i, s := range scored {
		c := in.Candidates[s.Index]
		c.Score = s.Score + lexicalAdjust(queryCodes, c.Text)
		out[i] = c
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })

	if r.cross != nil {
		reordered := make([]string, len(out))
		for i, c := range out {
			reordered[i] = c.Text
		}
		crossScored, err := r.cross.Rerank(ctx, in.Query, reordered)
		if err != nil {
			return SearchResult{}, fmt.Errorf("rerank: systemone: %w", err)
		}
		final := make([]Candidate, len(crossScored))
		for i, s := range crossScored {
			c := out[s.Index]
			c.Score = s.Score
			final[i] = c
		}
		out = final
	}

	return SearchResult{Query: in.Query, Candidates: out, CacheHit: in.CacheHit, MultiHop: in.MultiHop}, nil
}

// answer формує фінальну відповідь із кандидатів і пише в кеш.
//
// Multi-hop (Homework п.2): якщо серед кандидатів є і факт про тариф/мерчанта,
// і факт про підписанта (різні chunk_id, різні document_id чи секції), answer
// об'єднує ОБИ ДВА з окремою цитатою кожного — а не тільки топ-1. "Відповідь
// без посилання на джерело в цьому курсі не зараховується" (TestPipeline_EarlyWin) —
// це так само і про кожен з об'єднаних фактів, не тільки про перший.
func (r *retriever) answer(ctx agent.Context, in SearchResult) (string, error) {
	if len(in.Candidates) == 0 {
		return "Нічого не знайдено: у корпусі немає доказів для цього запиту.", nil
	}
	if in.CacheHit {
		return in.Candidates[0].Text, nil
	}
	n := budgetAnswerCandidates(in.MultiHop)
	if n > len(in.Candidates) {
		n = len(in.Candidates)
	}
	var parts []string
	for _, c := range in.Candidates[:n] {
		parts = append(parts, fmt.Sprintf("(%s, %s) %s", c.ChunkID, c.DocumentID, c.Text))
	}
	result := "[cache_hit=false] " + strings.Join(parts, " | ")
	if err := r.cache.put(ctx, r.emb, in.Query, strings.Replace(result, "[cache_hit=false]", "[cache_hit=true]", 1)); err != nil {
		return "", fmt.Errorf("answer: %w", err)
	}
	return result, nil
}

// budgetAnswerCandidates — скільки топ-кандидатів після rerank потрапляють у
// фінальну відповідь.
//
// Для single-hop — 3, не 1: той самий лексичний code-guard, що рятує запит
// про T-2 від чанка про T-1 (rerank(), lexicalAdjust), однаково підсилює й
// vectorSearchBranch-кандидата, й синтезовані факти
// graphFilterBranch/graphTraversalBranch, що згадують той самий код — на
// запит "Що таке тариф T-2?" вони опиняються в одному кластері з різницею
// score ~0.01 (виміряно: 1.453 / 1.449 / 1.446) — шум стабу, не сигнал.
// Обрізати на топ-1 означало б вгадувати переможця цього кластера замість
// покрити його весь.
//
// Для multi-hop — 6: на канонічному запиті Homework-у (README,
// "До/після") правильні факти — обидва мерчанти на T-2 (graph_filter) і
// обидва їхні підписанти (graph_traversal) — опиняються на позиціях 3–6
// після generic-контексту (означення тарифу, текст вимоги НБУ); менший
// бюджет означав би мовчки відкинути вже знайдений, правильно процитований
// доказ тільки тому, що rerank поставив контекст перед фактом.
func budgetAnswerCandidates(multiHop bool) int {
	if multiHop {
		return 6
	}
	return 3
}

// --- Вузли агентного графа (Homework п.1) -----------------------------------
//
// Start → cacheCheck ─ hit → answer
//                     ─ miss → classify ─┬─ vectorSearchBranch ─┐
//                                        ├─ graphFilterBranch   ├─ join → mergeBranches → budgetGate ─ insufficient → insufficientEvidence
//                                        └─ graphTraversalBranch┘                                     ─ ok → rerank → answer
//
// Явний бюджет: граф — фіксований DAG без циклів, тож глибина фіксована і
// невелика за побудовою, не за рантайм-лічильником: cacheCheck(1) →
// classify(2) → fan-out(3, паралельно — один "крок", не три) →
// join/merge/budgetGate — це вже постобробка зібраних доказів, не нові
// спроби retrieval. budgetGate — не лічильник ітерацій (граф не зациклюється
// і йому нема що рахувати), а контрольна точка: зупинитись і сказати "доказів
// недостатньо", а не повернути порожню відповідь мовчки.

// Routes — рядкові значення для workflow.StringRoute.
const (
	routeCacheHit     = "hit"
	routeCacheMiss    = "miss"
	routeInsufficient = "insufficient"
	routeOK           = "ok"
)

// Назви fan-out вузлів — JoinNode ключує свою вихідну мапу саме ними.
const (
	nodeVectorSearch   = "vector_search"
	nodeGraphFilter    = "graph_filter"
	nodeGraphTraversal = "graph_traversal"
)

// cacheCheck — перший вузол після Start: єдина кеш-перевірка на весь запуск.
// На hit граф одразу йде в answer (answer бачить CacheHit і повертає
// закешований текст без жодного retrieval); на miss — у classify. Вузол сам
// повертає *session.Event, щоб явно виставити Routes (FunctionNode.Run
// пропускає такий вихід напряму, не обгортаючи його в свій власний event).
func (r *retriever) cacheCheck(ctx agent.Context, query string) (*session.Event, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("порожній запит")
	}
	ev := session.NewEvent(ctx, ctx.InvocationID())
	answer, hit, err := r.cache.get(ctx, r.emb, query)
	if err != nil {
		return nil, fmt.Errorf("cacheCheck: %w", err)
	}
	if hit {
		ev.Output = SearchResult{Query: query, CacheHit: true, Candidates: []Candidate{{ChunkID: "cache", Text: answer, Score: 1}}}
		ev.Routes = []string{routeCacheHit}
		return ev, nil
	}
	ev.Output = query
	ev.Routes = []string{routeCacheMiss}
	return ev, nil
}

// multiHopRE — грубий, але достатній для цього корпусу сигнал, що запит
// просить ДРУГИЙ, пов'язаний факт (не просто "хто/що таке X"), як канонічний
// запит з Homework п.2: "...і хто підписував їхні договори?".
//
// Корінь "підпис", не "підписа": "підписав" (доконаний, підписа+в) і
// "підписував" (недоконаний, підписув+ав — інше дієслово, "підписувати")
// розходяться вже на шостій літері, не на сьомій — та сама помилка, що в
// knowledge.go з "підписав"/"підписала", тільки тут вона ламала не
// knowledge.go (signerRE лишається на "підписа": у корпусі обидва підписанти
// фігурують саме в доконаному виді), а цей regex — бо канонічне питання з
// Homework вжито в недоконаному.
var multiHopRE = regexp.MustCompile(`(?i)і\s+хто\b|хто\s+підпис`)

// classify позначає запит single-hop/multi-hop. Самого розгалуження графа
// це НЕ змінює (всі три гілки fan-out виконуються завжди — Homework-
// спрощення, задокументоване вище над routeInsufficient): позначка їде
// разом із запитом до answer і вирішує, скільки фактів увійде у відповідь
// (budgetAnswerCandidates).
func (r *retriever) classify(_ agent.Context, query string) (ClassifiedQuery, error) {
	return ClassifiedQuery{Query: query, MultiHop: multiHopRE.MatchString(query)}, nil
}

// vectorSearchBranch — адаптер над search(): усі три fan-out гілки мають
// приймати той самий тип входу (ClassifiedQuery), а search() лишається
// незмінним і прямо юніт-тестованим (TestSearch, TestBaseline_ConfusesTariffs).
// Повторна перевірка кешу всередині search() тут нешкідлива: на cacheCheck
// miss вона знову дасть miss (той самий запит, той самий кеш).
func (r *retriever) vectorSearchBranch(ctx agent.Context, in ClassifiedQuery) (SearchResult, error) {
	out, err := r.search(ctx, in.Query)
	if err != nil {
		return SearchResult{}, err
	}
	out.MultiHop = in.MultiHop
	return out, nil
}

// graphFilterBranch шукає в знаннєвому графі мерчантів і контракти для
// кожного тарифного коду в запиті (Merchant -has_tariff-> Tariff,
// Merchant -has_contract-> Contract). Це структурний, а не текстовий, пошук:
// на відміну від vectorSearchBranch, він не може сплутати T-1 і T-2 —
// Neighbors повертає лише те, що справді з'єднане ребром.
//
// Provenance — чанк таблиці тарифів (t-tariffs у стартовому корпусі): саме
// звідти buildGraph узяв ці вузли (merchant.Sources/tariff.Sources).
func (r *retriever) graphFilterBranch(_ agent.Context, in ClassifiedQuery) (SearchResult, error) {
	var out []Candidate
	seen := map[string]bool{}
	for _, qc := range codesIn(in.Query) {
		if qc.prefix != "t" {
			continue // цю гілку веде саме тарифний код; інші типи кодів їй не підказка
		}
		tariffID := entityID(entityTariff, strings.ToUpper(qc.prefix)+"-"+qc.value)
		tariff, ok := r.graph.Node(tariffID)
		if !ok {
			continue
		}
		for _, m := range r.graph.ByType(entityMerchant) {
			if !hasEdge(r.graph, m.ID, "has_tariff", tariffID) {
				continue
			}
			chunkID := firstOr(m.Sources, "")
			text := fmt.Sprintf("Мерчант %s підпадає під тариф %s.", m.Name, tariff.Name)
			for _, contract := range r.graph.Neighbors(m.ID, "has_contract") {
				text = fmt.Sprintf("Мерчант %s підпадає під тариф %s, договір %s.", m.Name, tariff.Name, contract.Name)
			}
			if seen[chunkID+m.ID] {
				continue
			}
			seen[chunkID+m.ID] = true
			out = append(out, r.graphCandidate(chunkID, text))
		}
	}
	return SearchResult{Query: in.Query, Candidates: out, MultiHop: in.MultiHop}, nil
}

// graphTraversalBranch обходить ВЕСЬ граф Contract -signed_by-> Signer, без
// фільтру по тарифу.
//
// Чому без фільтру — задокументована спрощення ДЗ: ця гілка виконується в
// тому самому fan-out, паралельно з graphFilterBranch, а не ПІСЛЯ нього —
// у неї немає результату graphFilterBranch, щоб звузити обхід до "лише
// контракти мерчантів на тарифі T-2" (справжній послідовний 2-hop: спершу
// відфільтрувати тариф, потім обійти лише ЦІ контракти). Замість цього вона
// віддає ВСІ пари контракт→підписант, а rerank() (лексичний guard на коди +
// bi-encoder) підіймає нагору саме той підписант, чий контракт згаданий у
// запиті чи знайдений graphFilterBranch-ом. Чесний шлях до справжнього
// 2-hop — зробити graph_traversal наступником graph_filter у графі, а не
// його сусідом по fan-out; README.md, "Decision Sheet" називає це явно як
// межу поточної реалізації.
func (r *retriever) graphTraversalBranch(_ agent.Context, in ClassifiedQuery) (SearchResult, error) {
	var out []Candidate
	for _, contract := range r.graph.ByType(entityContract) {
		// Один додатковий хоп — Contract ← (reverse has_contract) Merchant →
		// (has_tariff) Tariff — на тому ж вузлі, без залежності від
		// graphFilterBranch: збагачення, не фільтр. Без нього текст кандидата
		// однаковий для всіх трьох контрактів ("Договір X підписав Y"), і
		// лексичний guard у rerank() не має за що зачепитись — усі три
		// отримують однаковий (нульовий) code-бонус і лишаються нерозрізненими
		// на cosine, який на цьому корпусі теж їх майже не розрізняє (виміряно:
		// 0.389–0.403, у межах шуму). Тариф у тексті — те саме, за що вже
		// чіпляється lexicalAdjust для T-2 у запиті, тож він підніме саме
		// підписанта контракту на T-2, не вигадуючи нового механізму.
		tariffName, _ := tariffForContract(r.graph, contract.ID)
		for _, signer := range r.graph.Neighbors(contract.ID, "signed_by") {
			chunkID := r.signerChunk[contract.ID+"|"+signer.ID]
			if chunkID == "" {
				chunkID = firstOr(signer.Sources, "")
			}
			text := fmt.Sprintf("Договір %s підписав(ла) %s.", contract.Name, signer.Name)
			if tariffName != "" {
				text = fmt.Sprintf("Договір %s (тариф %s) підписав(ла) %s.", contract.Name, tariffName, signer.Name)
			}
			out = append(out, r.graphCandidate(chunkID, text))
		}
	}
	return SearchResult{Query: in.Query, Candidates: out, MultiHop: in.MultiHop}, nil
}

// tariffForContract — зворотний хоп Contract → Merchant → Tariff: Graph.Neighbors
// іде лише вперед (From→To), а ребро "has_contract" веде Merchant→Contract, тож
// тариф контракту шукаємо, перебираючи мерчантів (граф малий — десятки вузлів,
// лінійний скан непомітний).
func tariffForContract(g *kgraph.Graph, contractID string) (string, bool) {
	for _, m := range g.ByType(entityMerchant) {
		if !hasEdge(g, m.ID, "has_contract", contractID) {
			continue
		}
		if tariffs := g.Neighbors(m.ID, "has_tariff"); len(tariffs) > 0 {
			return tariffs[0].Name, true
		}
	}
	return "", false
}

// graphCandidate пакує синтезований з графа факт у Candidate з provenance:
// DocumentID — через chunksByID, щоб і графові кандидати, як і текстові,
// завжди несли "(chunk_id, document_id)" (TestPipeline_EarlyWin вимагає
// цього від будь-якого кандидата, що потрапляє у відповідь).
func (r *retriever) graphCandidate(chunkID, text string) Candidate {
	c := Candidate{ChunkID: chunkID, Text: text, Score: 1}
	if chunk, ok := r.chunksByID[chunkID]; ok {
		c.DocumentID = chunk.DocumentID
		c.SectionPath = chunk.SectionPath
	}
	return c
}

func hasEdge(g *kgraph.Graph, from, rel, to string) bool {
	for _, n := range g.Neighbors(from, rel) {
		if n.ID == to {
			return true
		}
	}
	return false
}

func firstOr(xs []string, def string) string {
	if len(xs) == 0 {
		return def
	}
	return xs[0]
}

// mergeBranches — fan-in вузол ПІСЛЯ JoinNode: об'єднує кандидатів трьох
// гілок в один SearchResult, прибираючи лише СПРАВЖНІ дублі — однаковий
// chunk_id І однаковий текст.
//
// Дедуп за самим лише chunk_id був реальним багом: graphFilterBranch
// синтезує кілька різних фактів ("Мерчант A-114 підпадає під тариф T-2,
// договір...", "Мерчант A-207 ...") з ОДНІЄЇ таблиці (t-tariffs) —
// vectorSearchBranch тим часом віддає ту саму таблицю як один кандидат
// із тим самим chunk_id. Дедуп лише за chunk_id бачив їх як один і той
// самий кандидат і викидав обидва синтезовані факти graphFilterBranch,
// лишаючи тільки сирий текст таблиці — саме той факт, що graph_filter мав
// додати явно, зникав.
func mergeBranches(_ agent.Context, gathered map[string]any) (SearchResult, error) {
	var out SearchResult
	seen := map[string]bool{}
	for _, name := range []string{nodeVectorSearch, nodeGraphFilter, nodeGraphTraversal} {
		v, ok := gathered[name]
		if !ok {
			continue
		}
		sr, ok := v.(SearchResult)
		if !ok {
			return SearchResult{}, fmt.Errorf("mergeBranches: %s дав %T, очікували SearchResult", name, v)
		}
		if out.Query == "" {
			out.Query, out.MultiHop = sr.Query, sr.MultiHop
		}
		for _, c := range sr.Candidates {
			key := c.ChunkID + "\x00" + c.Text
			if seen[key] {
				continue
			}
			seen[key] = true
			out.Candidates = append(out.Candidates, c)
		}
	}
	return out, nil
}

// budgetGate — контрольна точка перед rerank: немає жодного кандидата —
// явний терміновий вузол insufficientEvidence замість мовчазної порожньої
// відповіді.
func budgetGate(ctx agent.Context, in SearchResult) (*session.Event, error) {
	ev := session.NewEvent(ctx, ctx.InvocationID())
	ev.Output = in
	if len(in.Candidates) == 0 {
		ev.Routes = []string{routeInsufficient}
	} else {
		ev.Routes = []string{routeOK}
	}
	return ev, nil
}

// insufficientEvidence — терміновий вузол: явна відмова, без запису в кеш
// (нема що запам'ятовувати).
func insufficientEvidence(_ agent.Context, in SearchResult) (string, error) {
	return fmt.Sprintf("Недостатньо доказів у корпусі для відповіді на %q.", in.Query), nil
}

// tokens — нижній регістр, слова з літер/цифр; дефіс лишається всередині
// слова, щоб "T-2" не злився з "T-1".
func tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-'
	})
}

// overlap — частка слів запиту, що зустрічаються в тексті чанка.
func overlap(query, text []string) float64 {
	if len(query) == 0 {
		return 0
	}
	in := make(map[string]bool, len(text))
	for _, t := range text {
		in[t] = true
	}
	hits := 0
	for _, q := range query {
		if in[q] {
			hits++
		}
	}
	return float64(hits) / float64(len(query))
}
