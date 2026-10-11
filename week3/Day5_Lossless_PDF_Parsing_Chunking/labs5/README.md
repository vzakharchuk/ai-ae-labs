# Лабораторна 5 — ingestion-конвеєр: PDF → Markdown → ієрархічні чанки

**Станом на 09/2026:** Go 1.27.1, ADK Go v2.5.0, docling-mcp 3.2.1. Жодного LLM, ключа або `.env`.
Завдання — у [Homework.md](Homework.md).

## Запуск

```bash
cd week3/Day5_Lossless_PDF_Parsing_Chunking/labs5
go run . console          # введіть: testdata/ledgerworks_soc2.md
go test -v .              # 9 passed, 1 skipped — контракт TestChunk_TableStaysWhole
```

Граф (`graph.go`):

```text
Start → load → parse → chunk → extract_entities → index_vector → index_graph → report
```

| Файл | Що в ньому | Ваша робота |
|---|---|---|
| `pipeline.go` | вузли й типи `Source`, `Document`, `Chunk`, `Entity`, `Batch`, `Stores` | `TODO(студент)`: `chunk`, `extract_entities`, `index_*`, `report` |
| `graph.go` | топологія | ++ A3: fan-out на кілька документів |
| `main.go` | лише wiring: docling-клієнт + launcher | — |
| `main_test.go` | табличні тести на `StrictContextMock` | зняти `t.Skip`, переписати тести-заглушки |
| `internal/docling` | MCP-клієнт до docling-mcp | читати, не змінювати |
| `internal/embed` | ембединги (чистий Go за замовчуванням, Ollama — через `OLLAMA_EMBED_MODEL`), `Cosine`, `Rerank` | у `index_vector`, A1 |
| `internal/mdblocks` | Markdown → блоки (`Document.Blocks`); таблиця — один блок; глибина заголовка з нумерації, а не з `#` | у `chunk`, `extract_entities` |
| `internal/chunkeval` | токени, min/avg/max, скільки таблиць цілі, naive baseline | у `report`, порівняння для README |
| `internal/vecindex` | in-memory індекс зі стаб-ембедингом | у `index_vector`, A1 |
| `internal/kgraph` | in-memory граф із provenance | у `index_graph` |
| `testdata/` | тестовий документ: `.md`, `.pdf` і реальний вихід docling `.docling.md` | додати свій PDF |

## PDF через docling-mcp

`parse` передає PDF у [docling-mcp](https://github.com/docling-project/docling-mcp) — MCP-сервер над
[docling](https://github.com/docling-project/docling), який розпізнає розмітку сторінки й таблиці. Клієнт —
офіційний Go SDK `github.com/modelcontextprotocol/go-sdk` через stdio; два виклики на документ:

```text
convert_document_into_docling_document(source)  → document_key
export_docling_document_to_markdown(document_key) → markdown
```

**Що потрібно:** [`uv`](https://docs.astral.sh/uv/getting-started/installation/) (у dev-контейнері вже є). Сервер
стартує сам, лише коли приходить перший PDF:

```bash
uvx --from='docling-mcp[local]' docling-mcp-server --transport stdio
```

Перший запуск завантажує docling і моделі розмітки (сотні MB, кілька хвилин; прогрес — у stderr).

| Змінна | За замовчуванням | Навіщо |
|---|---|---|
| `DOCLING_MCP_CMD` | команда вище | свій сервер або пін версії: `uvx --from=docling-mcp[local]==3.2.1 docling-mcp-server --transport stdio` |
| `DOCLING_MCP_CONVERSION_MODE` | `local` | `remote` + `DOCLING_MCP_SERVICE_URL` — конвертація через Docling Serve |
| `DOCLING_MCP_DO_OCR` | `false` | `true` для сканованого PDF без текстового шару (завантажить OCR-моделі) |

Перевірити справжній сервер на тестовому PDF (з кореня репозиторію):

```bash
DOCLING_E2E=1 go test -run TestRealServer -v ./week3/Day5_Lossless_PDF_Parsing_Chunking/labs5/internal/docling
```

Звичайний `go test ./...` docling не запускає: PDF-шлях у тестах лаби йде через фейковий конвертер.

## Що видно на тестовому PDF

`testdata/ledgerworks_soc2.docling.md` — справжній вихід docling для `ledgerworks_soc2.pdf`:

- **3/3 таблиці цілі**, із заголовками стовпчиків — lossless-частина працює.
- **Рівні заголовків сплющено**: і `1 Scope`, і `1.1 Vendors in scope` стають `##`. Parent-Child
  ієрархію доведеться відновлювати з нумерації розділів, а не з кількості `#`.

## Експорт для ДЗ 6

ДЗ 6 читає масив `Chunk` у JSON (поля фіксує `TestChunkJSONContract`). Збережіть свої чанки й
запустіть ДЗ 6 так: `CHUNKS=…/chunks.json go run . console`.

---

# Домашка 11.10.2026

## Власний документ: OWASP ASVS 4.0.3, розділ V1

Узяв розділ "V1 Architecture, Design and
Threat Modeling" з офіційного [OWASP ASVS 4.0.3](https://github.com/OWASP/ASVS)
(вирізав сторінки 17-21 із повного стандарту через `qpdf`). 5 сторінок, 3
рівні заголовків, **14 окремих таблиць вимог**.

Прогнав через `docling-mcp` (моделі завантажились раз, ~87с). Знайшов реальну пастку: `mdblocks.numberRE` розпізнає лише
цифрову нумерацію (`^\d+(\.\d+)*`), а ASVS нумерує розділи з літерним
префіксом (`V1`, `V1.1`) — ієрархія не розпізнавалась узагалі, усі
заголовки падали в `Depth=0`.

Взяв уже
сконвертований docling-ом Markdown і зняв префікс `V` **лише** в рядках-
заголовках (`sed -E 's/^(##+) V([0-9])/\1 \2/'`), зберіг як окрему фікстуру.

- `owasp_asvs_v1_architecture.pdf` — оригінал;
- `owasp_asvs_v1_architecture.docling.md` — сирий output docling.
- `owasp_asvs_v1_architecture.md` — виправлена версія.

Побічний ефект docling на цьому документі: рядок `1.12.1 [DELETED, DUPLICATE
OF 12.4.1]` (видалена вимога всередині таблиці V1.12) вийшов окремим рядком
поза таблицею й випадково збігся з цифровою нумерацією — розпарсився як
заголовок `Depth=3`. Відома межа, не виправляв.

```
go run . console   # testdata/owasp_asvs_v1_architecture.md
Отримано 33 чанків (3 parents, 30 children). Розміри children (токени): n=30, tokens min/avg/max = 3/81.7/463. Tables: 8/8 preserved.
```

`463` токени в одному child — це велика таблиця на 18 рядків (V1.12),
навмисно НЕ розрізана, хоч і перевищує `maxChildTokens=200`: правило 3
("таблиця — рівно один child") важливіше за бюджет довжини.

## Статистика нарізки (тестова фікстура)

```
Отримано 15 чанків (5 parents, 10 children). Розміри children (токени): n=10, tokens min/avg/max = 12/32.0/78. Tables: 3/3 preserved.
```

5 parents: назва документа + 4 секції (`1 Scope`, `2 Findings`,
`3 Tariffs and Signers`, `4 Conclusion`). 10 children: 3 таблиці + 7
текстових/list-блоків. Усе зійшлось з ручним підрахунком по вихідному
Markdown (перевірив окремим тестом, `TestChunk_HierarchyAndSectionPath`).

## Naive chunking vs структурний — таблиця вендорів до/після

Homework просить порівняти на 800 токенах, але тестовий документ (≈350
слів) цілком влазить в одне вікно такого розміру — порівняння вийшло б
беззмістовним (`preserved=3/3` просто бо нічого не розрізалось). Узяв
менший розмір, який реально демонструє злам:

**Naive, вікна по 80 токенів, без перетину** (`chunkeval.NaiveWindows`):
`preserved=1/3` — 2 з 3 таблиць розрізані. Ось вікно, де таблиця вендорів
рветься буквально посеред рядка:

```
...window[0] закінчується:
| Acme Bank JSC | Card acquiring | SOC2 Type II | Q3 2025 | Kovalenko

window[1] починається:
& Partners | | Northwind Cloud | Hosting | ...
```

`"Kovalenko"` і `"& Partners"` — одна клітинка таблиці, розірвана навпіл
між двома чанками. Заголовки стовпчиків (`| Vendor | Service | ... |`)
лишились тільки у window[0] — рядки Northwind і Globex у window[1] вже без
них.

**Структурний (`chunk`, без параметра розміру вікна):** `preserved=3/3` —
усі три таблиці цілі, кожна — один child з повним заголовком і усіма
рядками, незалежно від довжини.

На ще меншому вікні (40 токенів) наївний підхід ламає всі три таблиці
(`preserved=0/3`) — зі зменшенням вікна стає тільки гірше, бо шанс, що межа
вікна впаде всередині таблиці, зростає. Структурний підхід від розміру
вікна взагалі не залежить — межі він бере не з довжини, а з розмітки
документа.

## Entity resolution і граф — без A2, дублікати не злиті семантично

`extract_entities` дає стабільні ID (`vendor:acme-bank-jsc`), тож той самий
Vendor із двох різних таблиць (вендорів і знахідок) зливається в **один**
вузол графа — `index_graph.AddNode` повертає `false` на повтор і домерджує
`Sources`. Це вже дає дедуплікацію **за точним збігом імені**: 19 сирих
сутностей → 14 унікальних вузлів, 8 ребер (`Vendor —has_finding→ Finding`,
`Merchant —has_contract→ Contract`).

Чого тут немає (це ++ A2, не зробив): якщо той самий вендор назвати
по-іншому в двох документах (`"Acme Bank JSC"` і `"ACME Bank JSC"` — інший
регістр, чи "Acme Bank" без суфіксу) — `slugify` дасть різні ID, і це будуть
два різні вузли. Embedding-based similarity з confidence scoring і manual
queue (A2) якраз вирішує саме це; тут — лише точний збіг після нормалізації
регістру.

## Обмеження

- `mdblocks.numberRE` не розпізнає нумерацію з літерним префіксом (`V1.1`,
  ISO-стиль `A.1.2`) — обійшов це для ASVS препроцесингом Markdown, код не
  міняв (бо спільний для Week 2).
- Entity resolution — лише точний збіг імені після slugify; без A2 схожі,
  але по-різному написані імена не зливаються.
- `extract_entities` знає чотири конкретні назви колонок (`Vendor`,
  `Finding ID`, `Merchant`, `Contract`) — таблиця з іншими назвами колонок
  (як усі 8 таблиць ASVS: `# | Description | L1 | L2 | L3 | CWE`) дає 0
  сутностей. Це чесно, не помилка: ASVS не про вендорів чи мерчантів.
- Vector-індекс — стаб-ембединг (`internal/vecindex`), не справжні вектори;
  `internal/embed` із `OLLAMA_EMBED_MODEL` дав би реальні, не підключав.
- Knowledge graph — in-memory, перезапуск процесу губить граф.

## Експорт для ДЗ 6

`testdata/chunks.json` — реальний вихід `chunk()` на `ledgerworks_soc2.md`
(15 чанків), той самий JSON-формат, що й у `labs6/testdata/chunks.json`.
