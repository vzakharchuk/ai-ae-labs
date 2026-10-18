# Лабораторна 6 — retrieval: пошук → re-rank → відповідь із provenance + кеш

**Станом на 09/2026:** Go 1.27.1, ADK Go v2.5.0. Жодного LLM, ключа або `.env` для стартера.
Завдання — у [Homework.md](Homework.md).

## Запуск

```bash
cd week3/Day6_Reranking_Semantic_Cache_Maturity_Ladder/labs6
go run . console          # запит: Що таке тариф T-2?  (повторіть — буде cache hit)
go test -v -race ./...    # 20 passed у labs6 (0 skipped) + internal/kgraph, internal/embed, internal/systemone
go test -run TestEval -v . # перегенерувати eval_results.md (Homework п.5)
```

Свій індекс із ДЗ 5 замість стартового корпусу:

```bash
CHUNKS=../../Day5_Lossless_PDF_Parsing_Chunking/labs5/testdata/chunks.json go run . console
```

Фінальна топологія (`graph.go`) — повна agentic-схема з Homework п.1, деталі й "чому саме так" —
у розділі [«Що нижче — вже не стартер»](#що-нижче--вже-не-стартер) далі:

```text
Start → cache_check ─ hit  → answer
                     ─ miss → classify ─┬─ vector_search    ─┐
                                        ├─ graph_filter      ├─ join → merge → budget_gate ─ insufficient → insufficient_evidence
                                        └─ graph_traversal  ─┘                               ─ ok          → rerank → answer
```

| Файл | Що в ньому |
|---|---|
| `retrieval.go` | `retriever`: усі вузли графа — `cacheCheck`, `classify`, три fan-out гілки, `rerank`, `answer`; семантичний кеш із трьома guard'ами |
| `knowledge.go` | знаннєвий граф `Merchant → Tariff/Contract → Signer` із `testdata/chunks.json` (Markdown-таблиці + regex на речення-підписи) |
| `graph.go` | повна топологія: `AddFanOut`/`AddFanIn`/`AddRoute` |
| `eval_test.go` | eval harness, 20 запитів, `TestEval` генерує `eval_results.md` |
| `main.go` | wiring: корпус + launcher |
| `main_test.go` | контрактні тести (рерank, кеш, multi-hop) + табличні |
| `internal/corpus` | читання `chunks.json` із перевіркою provenance (незмінено) |
| `internal/kgraph` | **новий пакет**: in-memory граф, типізовані вузли/ребра з provenance (копія дизайну `labs5/internal/kgraph` — Go забороняє імпорт `internal/` між лабами) |
| `internal/systemone` | re-rank decision-моделлю `tev1:4b` через `/v1/systemone` (опційно, `SYSTEMONE_URL`); незмінено |
| `internal/embed` | ембединги + `Cosine`/`Rerank`; незмінено |
| `testdata/chunks.json` | корпус LEDGERWORKS + вимога НБУ 2026, 21 чанк |

## Корпус

`testdata/chunks.json` — 21 чанк із двох документів: `ledgerworks_soc2` (той самий, що в ДЗ 5:
вендори, знахідки, тарифи, договори, підписанти) і `nbu_2026_requirements`. Цього достатньо для
запиту Оксани: *«які мерчанти на тарифі T-2 підпадають під вимогу НБУ 2026 і хто підписував їхні
договори?»* — відповідь потребує трьох hop-ів: тариф → мерчант → договір → підписант.

## Чому baseline навмисно слабкий

`search` рахує частку слів запиту в чанку. На *«яка ставка комісії на тарифі T-2?»* чанк про T-1
(`c-A331-rate`) потрапляє в топ-5 поруч із чанками про T-2 — `TestBaseline_ConfusesTariffs`
показує це. Це вихідна точка для таблиці «до/після» re-ranker-а в README.

## Ембединги

За замовчуванням `internal/embed` працює на чистому Go: детермінований стаб (хешовані триграми символів),
без мережі й ключа — на ньому йдуть `go test` і `go run`. Справжня модель — [Ollama](https://docs.ollama.com/capabilities/embeddings),
вмикається явно:

```bash
ollama pull embeddinggemma
OLLAMA_EMBED_MODEL=embeddinggemma go run . console   # OLLAMA_BASE_URL — якщо не localhost:11434
task week3:day6:web:ollama                           # Web UI з Ollama: перевіряє сервер і тягне модель
```

Рядок `embeddings: …` на старті показує, що саме працює. **Пороги не калібруйте на стабі:** він дає
0.47 для перефразування *«Що таке тариф T-2?» ≈ «Розкажи про тарифний план T-2»* і 0.88 для
*«…T-2?» vs «…T-1?»*, а `embed.Rerank` на стабі ставить чанк про мерчанта на T-1 першим на запит про T-2.
Стаб схожий на рядки, а не на зміст.

Але й модель сама не рятує. `embeddinggemma` (Ollama 0.40, 10/2026) на тих самих парах:

| Пара | стаб | embeddinggemma |
|---|---:|---:|
| «Що таке тариф T-2?» ≈ «Розкажи про тарифний план T-2» (має влучити) | 0.47 | 0.79 |
| «Що таке тариф T-2?» vs «Що таке тариф T-1?» (не має влучити) | 0.88 | 0.79 |
| «Скільки коштує тариф T-2?» ≈ «Яка ціна тарифу T-2?» | 0.42 | 0.95 |

Re-rank на моделі вже правильний (чанк про T-2 — перший), а от для кешу перефразування й інший тариф
мають однаковий cosine: жоден поріг їх не розділить. Потрібен детермінований доказ поверх схожості —
наприклад, однаковий набір кодів (`T-2`, `CT-2025-031`) у запиті й у кешованому запиті.

## Re-rank decision-моделлю (tev1:4b)

[Decision-модель](https://docs.ollama.com/capabilities/decision) — не чат-модель: відповідає на типізовані
питання за один прохід, без генерації тексту. `internal/systemone` питає для кожного кандидата «чи містить
уривок відповідь на запит?» і бере P(yes) як оцінку — це cross-encoder, на відміну від `embed.Rerank`.
Модель за замовчуванням — [`tev1:4b`](https://ollama.com/library/tev1) (Together AI).

```bash
task week3:day6:web:ollama        # тягне embeddinggemma і tev1:4b, вмикає обидва
```

Один запит руками — так виглядає re-rank одного кандидата:

```bash
curl -s http://localhost:11434/v1/systemone -H 'Content-Type: application/json' -d '{
  "model": "tev1:4b",
  "state": {"query": "Яка ставка комісії на тарифі T-2?",
            "passage": "Тариф T-2 — тарифний план зі ставкою комісії 2.9 % для мерчантів, які підпадають під вимогу НБУ 2026."},
  "questions": {"relevant": {"type": "noul", "instructions": "Does the passage contain the answer to the query?"}}
}'
# {"model":"tev1:4b","answers":{"relevant":{"type":"noul","noul":0.915}},"usage":{"input_tokens":167,"output_tokens":0}}
```

`tev1:4b` на корпусі стартера (Ollama 0.40.1, 10/2026):

| Перевірка | tev1:4b | embeddinggemma (cosine) |
|---|---:|---:|
| re-rank «ставка T-2»: чанк про T-2 | **0.92** | 0.73 |
| re-rank «ставка T-2»: чанки про T-1 | 0.004, 0.009 | 0.66, 0.57 |
| кеш: «Що таке тариф T-2?» ≈ «Розкажи про тарифний план T-2» | **0.89** | 0.79 |
| кеш: «Що таке тариф T-2?» vs «Що таке тариф T-1?» | **0.085** | 0.79 |

Для кешу питання інше: state `{"question_a", "question_b"}`, noul *«Do both questions ask for the same
information, so one answer serves both?»* — там, де cosine дає однакові 0.79, decision-модель розділяє.
Ціна — один виклик моделі на кандидата (або на пару запитів), тож рахуйте latency в eval harness.

Чи модель справді працює з українською, перевіряє live eval на Go — `internal/systemone/eval_test.go`:
rerank із hard negatives (інший тариф, договір, мерчант), порядок кандидатів, out-of-domain, кеш і
детермінізм. Без `SYSTEMONE_URL` тести пропускаються.

```bash
task week3:day6:eval:systemone                      # tev1:4b
task week3:day6:eval:systemone RERANK_MODEL=nimble  # те саме для іншої моделі
```

`tev1:4b`, 08.10.2026: rerank TP=10 FP=0 TN=11 FN=0, кеш TP=5 FP=0 TN=5 FN=0, out-of-domain ≤ 0.014.
Одна пастка: модель читає state як JSON-текст. Кирилиця має йти UTF-8, а не `\uXXXX` — Go так і робить,
а Python `json.dumps` без `ensure_ascii=False` ні (той самий запит: 0.867 замість 0.963).

Інші decision-моделі в Ollama: `tev1:0.8b`, `nimble`, `laya`, `clef-flash`, `clef`
([список](https://ollama.com/search?c=decision)); підставляйте через `SYSTEMONE_MODEL`.
LiquidAI d1-3B в Ollama 0.40.1 ще не працює (`unsupported decision encoding "lfm2-d1"`).

## Що нижче — вже не стартер

Усе над цим рядком — опис стартового шаблону (як його лишив курс). Нижче — звіт про те, що я
реально зробив по всіх 6 пунктах базового завдання (80 балів), з переміряними числами, не
оцінками. `go test -race ./...` — 20 passed у `labs6` (0 skipped), плюс тести `internal/kgraph`
(новий пакет) і вже наявні `internal/embed`/`internal/systemone`.

## 1. Agentic-граф

```text
Start → cache_check ─ hit  → answer
                     ─ miss → classify ─┬─ vector_search    ─┐
                                        ├─ graph_filter      ├─ join → merge → budget_gate ─ insufficient → insufficient_evidence
                                        └─ graph_traversal  ─┘                               ─ ok          → rerank → answer
```

`cache_check` і `budget_gate` повертають `*session.Event` із `Routes`, щоб `workflow.StringRoute`
обрав гілку (`workflow.NewEdgeBuilder`, `AddRoute`/`AddFanOut`/`AddFanIn`; еталон fan-out+Join —
[`examples/workflow/complex`](https://github.com/google/adk-go/blob/v2.5.0/examples/workflow/complex/main.go)).
`classify` — regex-правило (`multiHopRE`), не LLM: Homework explicitly дозволяє це як production-опцію
("Якщо щось не працює", п. «немає API-ключа для LLM-декомпозитора»).

**Бюджет 3–5 кроків — чому структурний, а не лічильник.** Граф — фіксований DAG без циклів:
`cache_check(1) → classify(2) → fan-out(3, паралельно — один крок, не три) → join/merge/budget_gate
(постобробка вже зібраних доказів, не нові спроби retrieval) → rerank → answer`. Лічильник ітерацій
нема чого рахувати — немає циклу, в якому граф міг би застрягнути. `budget_gate` — контрольна точка:
0 кандидатів після merge → явний вузол `insufficient_evidence` ("Недостатньо доказів у корпусі для
відповіді на ...") замість мовчазної порожньої відповіді.

**Чесне обмеження.** `graph_traversal` виконується в тому самому fan-out, що й `graph_filter`, не
після нього — тож не бачить його результату і не може звузити обхід до "лише контракти мерчантів на
T-2". Натомість він обходить ВЕСЬ граф `Contract → Signer` і для кожного контракту додає ще один
зворотний хоп `Contract ← Merchant → Tariff`, щоб позначити текст тарифом (не відфільтрувати —
просто дати `rerank` лексичний гачок, за який можна вхопитись). Справжній послідовний 2-hop —
зробити `graph_traversal` наступником `graph_filter`, не його сусідом по fan-out; це і є межа, яку
Decision Sheet (п. 6) явно називає наступним щаблем.

## 2. Multi-hop запит Оксани

```text
Q: які мерчанти на тарифі T-2 підпадають під вимогу НБУ 2026 і хто підписував їхні договори?

(c-tariff-T2, ledgerworks_soc2) Тариф T-2 — ставка комісії 2.9 % для мерчантів під вимогою НБУ 2026.
(c-nbu-req, nbu_2026_requirements) Вимога НБУ 2026 поширюється на мерчантів з тарифом T-2: посилена
  перевірка договору до 01.04.2026, підпис уповноваженою особою еквайра.
(t-tariffs, ledgerworks_soc2) Мерчант A-207 підпадає під тариф T-2, договір CT-2025-031.
(c-sign-014, ledgerworks_soc2) Договір CT-2025-014 (тариф T-2) підписав(ла) Олег Мельник.
(t-tariffs, ledgerworks_soc2) Мерчант A-114 підпадає під тариф T-2, договір CT-2025-014.
(c-sign-031, ledgerworks_soc2) Договір CT-2025-031 (тариф T-2) підписав(ла) Ірина Бойко.
```

Обидва мерчанти (A-114, A-207), обидва підписанти (Олег Мельник, Ірина Бойко), кожен зі своєю
цитатою `(chunk_id, document_id)` — і жодного згадування `CT-2025-007` (тариф T-1, не відповідає
запиту). `TestPipeline_MultiHop` фіксує це автоматично, включно з негативною перевіркою на
забруднення T-1. `budgetAnswerCandidates` для multi-hop = 6, не 2: на цьому корпусі правильні факти
опиняються на позиціях 3–6 після generic-контексту (означення тарифу, текст вимоги НБУ) — менший
бюджет мовчки відкинув би вже знайдений, правильно процитований доказ.

## 3. Re-ranking: 5 запитів «до/після»

Перший рядок — `TestRerank_ChangesOrder` (обов'язковий приклад з Homework.md), решта — на
testdata/chunks.json, `embed.Rerank` (bi-encoder, стаб; реальний embeddinggemma — нижче):

| # | Запит | До rerank (топ) | Після rerank (топ) | Що показує |
|---|---|---|---|---|
| 1 | «яка ставка комісії на тарифі T-2?» (2 кандидати, ізольована пара) | `c-A331-rate` (T-1), score 0.88 | `c-A114-rate` (T-2), score 0.86 → топ | **Виправлено**: чистий baseline ставив T-1 вище T-2 — текстово майже ідентичні речення |
| 2 | «яка ставка комісії на тарифі T-1?» (повний пул, topK=5) | `c-tariff-T1` 0.50, у топ-3 — `c-A114-rate` (T-2!) 0.333 | `c-A331-rate` 1.571, `c-tariff-T1` 1.503, `c-A207-rate` (T-2) **-0.502** | **Забруднення прибрано**: T-2-кандидат не просто впав — пішов у негативний score |
| 3 | канонічний multi-hop (п. 2) | vector-only пул: жодного мерчанта/підписанта поіменно | після merge+rerank: A-114/A-207 + Олег/Ірина в топ-6, CT-2025-007 (T-1) не входить | **Факти знайдено**: ефект не переупорядкування, а поява фактів, яких у vector-only пулі не було взагалі |
| 4 | «яка знахідка… Acme Bank JSC?» | `c-F101` 0.444 = `c-F104` 0.444 (нічийна) | `c-F101` 0.634, `c-F104` 0.421 | **Нічию розведено**: той самий топ-1, але впевнено, не навмання — стійкіше до дрейфу корпусу |
| 5 | «хто підписав договір мерчанта A-114?» | `c-A114-rate` (ставка, НЕ підписант) топ | те саме, `c-A114-rate` лишається топ-1 | **Відома межа**: лексичний guard на код `A-114` однаково підсилює і чанк про ставку, і чанки про підписанта — обидва згадують код, тож не розрізняє "про ставку" від "про підпис". `c-sign-014` (правильна відповідь) є в пулі, але нижче топ-1 |

Рядок 5 — не прихований, а задокументований провал: лексичний guard розв'язує конфлікт
"який тариф" (код тарифу), не конфлікт "яка сторона факту про тариф" (ставка чи підпис). Для цього
потрібен або LLM-суддя, або явний інтент-слот у classify ("просить підписанта" / "просить ставку") —
поза обсягом базового завдання.

### Той самий guard — чому embeddinggemma сам не рятує

`internal/embed`'s власний коментар попереджає: стаб "схожий на рядки, а не на зміст". Виміряно й
на реальній моделі (`OLLAMA_EMBED_MODEL=embeddinggemma`, той самий корпус):

| Пара | cosine | Що мало статись |
|---|---:|---|
| «яка ставка на T-2?» ≈ «скажи яка комісія для тарифу T-2» (парафраз) | 0.895 | мало дати hit |
| «яка ставка на T-2?» vs «яка ставка на T-1?» (той самий шаблон, інший тариф) | **0.895** | не мало дати hit — те саме число, що й парафраз |
| «яка ставка на T-2?» vs multi-hop запит про T-2 (той самий тариф, інша форма питання) | 0.513 | не мало дати hit |

Рядок 2 — не гіпотеза, а те, через що `lexicalAdjust`/`codesConflict` у коді — обов'язкові, не
"nice to have": жоден порог cosine не розділить парафраз і підміну коду, бо embeddinggemma дає їм
ідентичний score.

## 4. Семантичний кеш

`cache_check` — перший вузол графа (не просто поле всередині `search`, як у стартері). Ключ —
`cosine(r.emb.Embed(query), збережений вектор) ≥ 0.45` (`systemone.Same`, якщо `SYSTEMONE_URL`
задано — точніше за cosine, але опційно), І обов'язкові guard'и, що йдуть ПЕРЕД порогом:

- **`codesConflict`** — немає спільного коду (`T-2`/`A-114`/`CT-2025-014`…) між запитами → не
  кешуємо, хай яким високим не був би score.
- **guard на форму запиту** — той самий `multiHopRE`, що й `classify`: single-hop запит ніколи не
  віддасть відповідь multi-hop-кешу про той самий тариф, і навпаки.

Три продемонстровані сценарії (`TestCache_ExactHit`, `TestCache_ParaphraseHits`,
`TestCache_RejectsDifferentTariff` + `RejectsDifferentHopShape` + `RejectsDifferentEntity`):

```text
1. "Що таке тариф T-2?"                                    → miss (кеш порожній)
2. "що таке тариф t-2"  (та сама, інший регістр/пунктуація) → hit (точний збіг після normalize)
3. "Розкажи про тарифний план T-2"  (справжній парафраз)    → hit (cosine ≥ 0.45)
4. "яка ставка комісії на тарифі T-1?"  (інший тариф)       → miss (codesConflict: T-2 ≠ T-1)
5. canonical multi-hop про T-2  (той самий тариф, 2 факти)  → miss (guard на форму запиту)
6. "хто підписав договір мерчанта A-114?" vs запит про T-2  → miss (codesConflict: a-114 ≠ t-2)
```

Рядки 4–6 — це НЕ один тест-кейс, а три РІЗНІ виміряні false positive, кожен з окремим механізмом
захисту (знайдено й закрито послідовно, не спроєктовано заздалегідь — `retrieval.go`, коментарі над
кожним guard-ом показують реальні вимірені cosine, на яких guard без нього пропустив би хибний hit).

**Що кешується.** Фінальна відповідь (`answer()`), не проміжний `SearchResult` — кеш на межі
retrieval і presentation, щоб `cache_check` міг повернути текст одразу, без запуску графа. **Межі,
чесно:** один tenant, одна версія корпусу, без TTL — перезапуск процесу очищає кеш (`sync.Mutex`,
слайс у пам'яті, той самий компроміс, що й `internal/kgraph`: "process restart loses the graph").
Інвалідація після оновлення корпусу — не реалізована: production-версія or потребує версіонованого
ключа (`corpus_hash` поруч із запитом), або TTL, що коротший за цикл переіндексації.

## 5. Eval harness — 20 запитів

10 single-hop + 10 multi-hop, усі — реальні факти з `testdata/chunks.json` (не вигадані для
зручності; перевірено `knowledge_test.go` і прямим читанням корпусу). `go test -run TestEval -v .`
генерує `eval_results.md` поруч із кодом — числа нижче саме звідти, стаб-ембединги (детерміновано,
відтворювано без Ollama):

| Метрика | Baseline (vector-RAG, bi-encoder топ-1, без кешу/графа/budget) | Агентний pipeline |
|---|---:|---:|
| top1_relevance / recall (mean, 20) | **0.60** | **0.88** |
| groundedness (mean) | 1.00 | 1.00 |
| latency_p95, ms | 0.3–2 (шум стабу, не вимір продуктивності) | 1–5 |
| tokens_used (проксі: слів у відповіді) | 16.0 | 80.9 |

**groundedness 1.00 для обох — не перемога, а тавтологія**: і baseline, і агентний pipeline лише
цитують реальні `chunk_id` з корпусу, жодної генерації тексту немає (немає LLM-виклику в усьому
графі) — галюцинувати тут нічим. `top1_relevance`/`groundedness` рахуються правилом (рядковий
пошук потрібних `chunk_id` у відповіді), не LLM-суддею і не ручною оцінкою 1–5: нема LLM-виклику,
якому можна було б доручити суддівство, а ручна оцінка 20×2 відповідей на кожен прогін — не
відтворювана. `tokens_used` — проксі (слова у відповіді), не токени моделі: моделі в цьому графі
немає.

**Де агентний виграє, де нема різниці** (повна таблиця — `eval_results.md`):

- **Виграє переконливо** (0.00–0.50 → 1.00) на запитах, що лежать у домені знаннєвого графа —
  canonical multi-hop, і будь-який запит "мерчант → тариф + підписант" (`a114-rate-and-signer`,
  `a207-tariff-and-signer`, `a331-signer-and-nbu`, `t1-rate-and-signer`). Це домен, у якому
  `graph_filter`/`graph_traversal` реально щось знають.
- **Різниці нема** (0.50 = 0.50) на multi-hop запитах ПОЗА цим доменом — vendor/auditor/finding
  (`northwind-service-and-incidents`, `f101-control-and-auditor`, `globex-auditor-and-finding`,
  `acme-findings-and-severity`): наш `kgraph` моделює лише `Merchant → Tariff/Contract → Signer`
  (той граф, що явно попросив Homework), не `Vendor → Auditor/Finding` — для цих запитів
  `graph_filter`/`graph_traversal` не дають нічого понад `vector_search`, і агентний pipeline
  зводиться до того самого bi-encoder топ-N, що й baseline.
- Жодного випадку, де агентний pipeline ГІРШИЙ за baseline — найгірше, що буває, це "та сама
  якість, дорожче" (latency/tokens), ніколи "гірша якість".

## 6. Decision sheet — Retrieval Maturity Ladder

**Щабель:** GraphRAG + агентний планувальник з бюджетом (рядок 5 типової шкали: naive vector →
vector + rerank → hybrid vector/keyword → + семантичний кеш → **GraphRAG + routing/budget** → adaptive
planner з SLO).

**Чому саме цей щабель, не нижчий.** Канонічний запит Оксани структурно потребує двох різних
операцій — семантичного пошуку (що таке тариф T-2) і графового обходу (хто підписав контракт цього
мерчанта) — які жоден одинарний vector-пошук не поєднає: eval (п. 5) вимірює це прямо — на запитах
усередині домену графа baseline дає 0.00–0.50 relevance, агентний — стабільно 1.00. Переходити до
GraphRAG без переходу до agentic routing теж не варіант: без `classify`/`budget_gate`/`cache_check`
не було б ні явної зупинки на "недостатньо доказів", ні контролю вартості повторних запитів.

**Чому не вищий.** Усі три компоненти наступного щабля (adaptive planner з SLO/token budget) —
явно поза обсягом: `classify` — regex, не LLM-планувальник, що сам вирішує глибину обходу;
`graph_traversal` — безумовний повний обхід, не SLO-кероване рішення "ще один hop чи вже досить";
cross-encoder (`tev1:4b`) не під'єднано live (нижче, чому) — pipeline покладається на bi-encoder +
лексичний guard, не на модель, що читає запит і уривок разом.

**Що розглядатиму далі, якщо query patterns зміняться:**

1. **Запити вийдуть за межі Merchant/Tariff/Signer** (vendor/auditor/finding, як 5 із 10 multi-hop
   у eval-наборі показують уже зараз) → розширити `kgraph`-схему на `Vendor -audited_by->
   Auditor`, `Finding -affects-> Vendor` — та сама механіка (`buildGraph`, Markdown-таблиці), інша
   онтологія.
2. **Запити виходять за межі одного hop-фрейму** (не "мерчант→тариф→підписант", а довільна
   композиція) → `graph_traversal` стає наступником `graph_filter` (не сусідом по fan-out) —
   послідовний 2-hop замість поточного паралельного наближення (задокументовано в п. 1 як чесне
   обмеження).
3. **Обсяг корпусу виросте за межі "лінійний скан по графу непомітний"** → `kgraph` із
   adjacency-мапи в пам'яті на справжню graph-базу (FalkorDB — case study з Homework, не
   залежність; вікі курсу позначає її `gated pending raw`, тож не зараз).
4. **Ollama підніме версію, що тягне `tev1:4b`** (зараз встановлена 0.34.2, модель вимагає новішу
   — `brew upgrade ollama` на цій машині впав з незрозумілою помилкою "ollama not installed", бо
   бінарник встановлено не через brew) → включити cross-encoder живим шаром над bi-encoder, не
   лише кодом, що підтримує це архітектурно (`systemone.New()` вже викликається з `rerank`/кешу,
   повертає `nil` і не впливає на поведінку, доки `SYSTEMONE_URL` не задано).
5. **LLM-Wiki / OKF** (третя нога KB-модуля) — за межами цього тижня (Вебінар 9), але decision
   sheet мав її згадати: поточний `kgraph` — лише Merchant/Tariff/Signer, без онтології верхнього
   рівня, яку LLM-Wiki додав би.
