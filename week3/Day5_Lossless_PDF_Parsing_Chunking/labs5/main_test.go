// Скелет тестів для ДЗ 5.
//
// Homework.md, пункт 7, вимагає табличні тести на кожен вузол графа через
// agent.StrictContextMock — і `go test ./...` зеленим. Цей файл дає готову
// форму, щоб ви не витрачали перші 40 хвилин на «як узагалі підсунути
// agent.Context у функцію вузла».
//
// ЯК ЦИМ КОРИСТУВАТИСЬ
//
//	go test ./...            — зараз зелено: тести перевіряють ЗАГЛУШКИ
//	                           (один гігантський чанк — це поточна поведінка).
//	                           Це навмисно: зелений старт означає, що
//	                           середовище живе, і далі ви ламаєте тести
//	                           свідомо, а не воюєте з інструментами.
//
// Ваш шлях: реалізуєте TODO у pipeline.go → тести нижче ПАДАЮТЬ, бо вони
// описують стару поведінку → переписуєте їх під нову. Місця, які треба
// переписати, позначені TODO(студент). Тест «таблиця не ріжеться»
// (TestChunk_TableStaysWhole) уже описує ЦІЛЬОВУ поведінку і має падати, доки
// chunk — заглушка: це контракт ДЗ, а не заготовка. Він єдиний тут із t.Skip —
// зніміть skip, коли візьметесь за chunking.
//
// Тестам не потрібні ні Python, ні docling: PDF-шлях перевіряється фейковим
// конвертером (fakeConverter). Справжній docling-mcp перевіряє opt-in тест
// internal/docling (DOCLING_E2E=1), а його реальний вихід для тестового PDF
// збережено в testdata/ledgerworks_soc2.docling.md.
//
// Чому StrictContextMock, а не nil: вузли приймають agent.Context. Передати
// туди nil можна лише доти, доки ви його не використовуєте; щойно у вашому
// коді з'явиться ctx.InvocationID() або емісія події, nil дасть panic у
// найгіршому місці — у проді. StrictContextMock панікує ГУЧНО і одразу на
// будь-якому методі, про який ви не подумали, тож помилка знаходиться в тесті,
// а не на демо.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"

	"github.com/dimetron/ai-eng-course/labs/internal/labrun"
	"github.com/dimetron/ai-eng-course/labs/week3/Day5_Lossless_PDF_Parsing_Chunking/labs5/internal/chunkeval"
)

// nodeCtx — суворий фейк agent.Context. Вбудований StrictContextMock означає:
// будь-який метод, який ваш вузол викличе, а ми його тут не передбачили,
// впаде з panic, а не поверне тихо нульове значення.
type nodeCtx struct {
	agent.StrictContextMock
}

func newNodeCtx() *nodeCtx {
	return &nodeCtx{StrictContextMock: agent.NewStrictContextMock(context.Background())}
}

// fakeConverter підміняє docling-mcp: повертає заданий Markdown або помилку.
type fakeConverter struct {
	markdown string
	err      error
}

func (f fakeConverter) ToMarkdown(context.Context, string) (string, error) {
	return f.markdown, f.err
}

const (
	fixtureMD     = "testdata/ledgerworks_soc2.md"
	fixturePDF    = "testdata/ledgerworks_soc2.pdf"
	fixtureDocMD  = "testdata/ledgerworks_soc2.docling.md"
	fixtureTables = 3
)

// --- Вузол load --------------------------------------------------------------

func TestLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	docx := filepath.Join(dir, "doc.docx")
	if err := os.WriteFile(docx, []byte("PK\x03\x04"), 0o600); err != nil {
		t.Fatalf("підготовка фікстури: %v", err)
	}

	tests := []struct {
		name     string
		path     string
		wantKind string
		wantErr  bool
	}{
		{name: "markdown", path: fixtureMD, wantKind: "markdown"},
		{name: "pdf за magic bytes", path: fixturePDF, wantKind: "pdf"},
		{name: "пробіли навколо шляху", path: "  " + fixtureMD + "\n", wantKind: "markdown"},
		{name: "файла немає", path: filepath.Join(dir, "ghost.md"), wantErr: true},
		{name: "порожній шлях", path: "", wantErr: true},
		{name: "непідтримуваний формат", path: docx, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := load(newNodeCtx(), tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("load(%q) = %+v, err = nil; очікували помилку", tt.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("load(%q) error = %v", tt.path, err)
			}
			if got.Kind != tt.wantKind {
				t.Errorf("load(%q).Kind = %q, очікували %q", tt.path, got.Kind, tt.wantKind)
			}
		})
	}
}

// --- Вузол parse -------------------------------------------------------------

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		conv     fakeConverter
		src      Source
		contains string
		wantErr  string
	}{
		{name: "markdown читається як є", src: Source{Path: fixtureMD, Kind: "markdown"}, contains: "| F-101"},
		{name: "pdf іде в конвертер", conv: fakeConverter{markdown: "| a |\n|---|\n| 1 |"}, src: Source{Path: fixturePDF, Kind: "pdf"}, contains: "| 1 |"},
		{name: "помилка конвертера", conv: fakeConverter{err: errors.New("docling down")}, src: Source{Path: fixturePDF, Kind: "pdf"}, wantErr: "docling down"},
		{name: "порожній результат", conv: fakeConverter{markdown: "  \n"}, src: Source{Path: fixturePDF, Kind: "pdf"}, wantErr: "порожній"},
		{name: "markdown-файла немає", src: Source{Path: "testdata/ghost.md", Kind: "markdown"}, wantErr: "ghost.md"},
		{name: "невідомий тип", src: Source{Path: fixtureMD, Kind: "docx"}, wantErr: "невідомий тип"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := newParse(tt.conv)(newNodeCtx(), tt.src)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parse() err = %v, очікували %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse() error = %v", err)
			}
			if got.ID != "ledgerworks_soc2" {
				t.Errorf("Document.ID = %q, очікували ім'я файла без розширення", got.ID)
			}
			if !strings.Contains(got.Markdown, tt.contains) {
				t.Errorf("Markdown не містить %q", tt.contains)
			}
		})
	}
}

// TestDoclingFixtureKeepsTables фіксує, що дає docling на тестовому PDF: усі
// три таблиці — цілими, із заголовками. Зверніть увагу на пастку: docling
// сплющує рівні заголовків — і "1 Scope", і "1.1 Vendors in scope" стають
// "##". Ієрархію для Parent-Child доведеться відновлювати з нумерації розділів.
func TestDoclingFixtureKeepsTables(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(fixtureDocMD)
	if err != nil {
		t.Fatal(err)
	}
	md := string(data)
	if got := strings.Count(md, "\n|--"); got != fixtureTables {
		t.Errorf("таблиць у виході docling = %d, очікували %d", got, fixtureTables)
	}
	if strings.Contains(md, "\n### ") {
		t.Log("docling повернув ### — рівні заголовків збережено; перевірте свою логіку ієрархії")
	}
}

// --- Вузол chunk -------------------------------------------------------------

// TestChunk_TitleIsImplicitParent: один непронумерований заголовок (Depth 0,
// назва документа) сам стає parent-чанком, а наступний блок — його дитиною.
// Найменший випадок правила 1/2 з TODO в pipeline.go.
func TestChunk_TitleIsImplicitParent(t *testing.T) {
	t.Parallel()
	got, err := chunk(newNodeCtx(), newDocument("d1", "", "# Розділ\n\nабзац\n"))
	if err != nil {
		t.Fatalf("chunk() error = %v", err)
	}
	if len(got.Chunks) != 2 {
		t.Fatalf("чанків = %d, очікували 2 (parent + 1 child)", len(got.Chunks))
	}
	parent, child := got.Chunks[0], got.Chunks[1]
	if parent.Level != 0 || parent.ParentID != "" || parent.Kind != "heading" {
		t.Errorf("parent = %+v; очікували Level=0, ParentID=\"\", Kind=heading", parent)
	}
	if child.Level != 2 || child.ParentID != parent.ID || child.Text != "абзац" {
		t.Errorf("child = %+v; очікували Level=2, ParentID=%q, Text=\"абзац\"", child, parent.ID)
	}
	if got.Chunks[0].DocumentID != "d1" || got.Chunks[1].DocumentID != "d1" {
		t.Errorf("DocumentID втрачено: %+v", got.Chunks)
	}
}

// TestChunk_HierarchyAndSectionPath — основний контракт Parent-Child: нова
// Depth-1 секція відкриває parent, Depth-2 підсекція лише оновлює SectionPath
// (не створює власного чанка), а діти несуть повний шлях "верх / підрозділ".
func TestChunk_HierarchyAndSectionPath(t *testing.T) {
	t.Parallel()
	md := "# Doc\n\n## 1 Scope\n\n### 1.1 Vendors\n\nтекст про вендорів\n\n## 2 Findings\n\nтекст про знахідки\n"
	got, err := chunk(newNodeCtx(), newDocument("d2", "", md))
	if err != nil {
		t.Fatalf("chunk() error = %v", err)
	}
	// Doc(parent) → 1 Scope(parent) → "текст про вендорів"(child, 1 Scope / 1.1 Vendors) → 2 Findings(parent) → "текст про знахідки"(child, 2 Findings)
	var parents, children int
	bySectionPath := map[string]Chunk{}
	for _, c := range got.Chunks {
		if c.Level == 0 {
			parents++
			continue
		}
		children++
		bySectionPath[c.SectionPath] = c
	}
	if parents != 3 {
		t.Fatalf("parents = %d, очікували 3 (Doc, 1 Scope, 2 Findings)", parents)
	}
	if children != 2 {
		t.Fatalf("children = %d, очікували 2", children)
	}
	vendors, ok := bySectionPath["1 Scope / 1.1 Vendors"]
	if !ok {
		t.Fatalf("немає child з SectionPath %q; маємо %v", "1 Scope / 1.1 Vendors", bySectionPath)
	}
	if vendors.Text != "текст про вендорів" {
		t.Errorf("text = %q", vendors.Text)
	}
	findings, ok := bySectionPath["2 Findings"]
	if !ok {
		t.Fatalf("немає child з SectionPath %q (нова Depth-1 секція мала скинути subsection)", "2 Findings")
	}
	if findings.Text != "текст про знахідки" {
		t.Errorf("text = %q", findings.Text)
	}
}

// TestChunk_LongTextSplitsBySentence — правило 4: текстовий блок довший за
// maxChildTokens розбивається на кілька children по межах речень, і жодне
// речення не розрізається навпіл.
func TestChunk_LongTextSplitsBySentence(t *testing.T) {
	t.Parallel()
	sentence := "Це одне тестове речення для вимірювання довжини тексту. "
	long := strings.Repeat(sentence, 40) // довше за maxChildTokens=200
	md := "# Doc\n\n" + long
	got, err := chunk(newNodeCtx(), newDocument("d3", "", md))
	if err != nil {
		t.Fatalf("chunk() error = %v", err)
	}
	var children []Chunk
	for _, c := range got.Chunks {
		if c.Level == 2 {
			children = append(children, c)
		}
	}
	if len(children) < 2 {
		t.Fatalf("children = %d, очікували розбиття на ≥2 — довгий текст не розрізано", len(children))
	}
	for _, c := range children {
		if n := chunkeval.Tokens(c.Text); n > maxChildTokens {
			t.Errorf("child на %d токенів перевищує maxChildTokens=%d: %q", n, maxChildTokens, c.Text)
		}
		if !strings.HasSuffix(strings.TrimSpace(c.Text), ".") {
			t.Errorf("child не закінчується на межі речення: %q", c.Text)
		}
	}
	if strings.Join(collectTexts(children), " ") != strings.TrimSpace(long) {
		t.Errorf("розбиття загубило чи переставило текст")
	}
}

func collectTexts(chunks []Chunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.Text
	}
	return out
}

func TestChunk_EmptyDocumentIsError(t *testing.T) {
	t.Parallel()
	if _, err := chunk(newNodeCtx(), Document{}); err == nil {
		t.Fatal("chunk(порожній) = nil error; порожній документ має бути помилкою")
	}
}

// TestChunk_TableStaysWhole — КОНТРАКТ ДЗ, а не заготовка.
//
// Головна вимога тижня: жодна таблиця не може бути розрізана між двома
// чанками. Тест перевіряє це напряму на реальному виході docling: кожна
// таблиця — рівно один чанк Kind == "table", з усіма рядками й заголовком.
func TestChunk_TableStaysWhole(t *testing.T) {
	data, err := os.ReadFile(fixtureDocMD)
	if err != nil {
		t.Fatal(err)
	}
	got, err := chunk(newNodeCtx(), newDocument("ledgerworks_soc2", fixtureDocMD, string(data)))
	if err != nil {
		t.Fatalf("chunk() error = %v", err)
	}

	var tables []Chunk
	for _, c := range got.Chunks {
		if c.Kind == "table" {
			tables = append(tables, c)
		}
	}
	if len(tables) != fixtureTables {
		t.Fatalf("чанків типу table = %d, очікували %d — таблицю розрізано або втрачено", len(tables), fixtureTables)
	}
	wantRows := map[string][]string{
		"Vendor":     {"Acme Bank JSC", "Northwind Cloud", "Globex Payments"},
		"Finding ID": {"F-101", "F-102", "F-103", "F-104", "F-105", "Severity"},
		"Merchant":   {"A-114", "A-207", "A-331", "NBU 2026 requirement"},
	}
	for header, rows := range wantRows {
		var whole bool
		for _, c := range tables {
			if !strings.HasPrefix(firstTableRow(c.Text), "| "+header) {
				continue
			}
			whole = true
			for _, row := range rows {
				if !strings.Contains(c.Text, row) {
					whole = false
					t.Errorf("у таблиці %q немає %q — рядок або заголовок втрачено:\n%s", header, row, c.Text)
				}
			}
			if c.ParentID == "" {
				t.Errorf("таблиця %q без ParentID — зв'язок із розділом втрачено", header)
			}
		}
		if !whole {
			t.Errorf("таблицю з заголовком %q не знайдено цілою", header)
		}
	}
}

// firstTableRow — перший рядок таблиці (рядок заголовків), щоб "Vendor" у
// таблиці знахідок не сплутати з таблицею вендорів.
func firstTableRow(text string) string {
	for line := range strings.Lines(text) {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "|") {
			return line
		}
	}
	return ""
}

// --- Вузли extract_entities / index_vector / index_graph ---------------------

// chunkedFixture ганяє load→chunk→extractEntities на реальній фікстурі, щоб
// кожен тест нижче перевіряв вузол на справжніх даних, а не на одному
// вигаданому чанку.
func chunkedFixture(t *testing.T) Batch {
	t.Helper()
	data, err := os.ReadFile(fixtureMD)
	if err != nil {
		t.Fatal(err)
	}
	ctx := newNodeCtx()
	batch, err := chunk(ctx, newDocument("ledgerworks_soc2", fixtureMD, string(data)))
	if err != nil {
		t.Fatalf("chunk() error = %v", err)
	}
	batch, err = extractEntities(ctx, batch)
	if err != nil {
		t.Fatalf("extractEntities() error = %v", err)
	}
	return batch
}

// TestExtractEntities_FindsKnownTypes перевіряє, що вузол реально читає
// типізовані сутності з таблиць фікстури — Vendor з SourceChunkID на таблицю
// вендорів, і що таблиця без жодної відомої колонки (0 рядків) не дає жодної
// сутності, а не падає.
func TestExtractEntities_FindsKnownTypes(t *testing.T) {
	t.Parallel()
	batch := chunkedFixture(t)

	var acme *Entity
	for i := range batch.Entities {
		if batch.Entities[i].Type == "Vendor" && batch.Entities[i].Name == "Acme Bank JSC" {
			acme = &batch.Entities[i]
			break
		}
	}
	if acme == nil {
		t.Fatalf("Vendor %q не знайдено серед %d сутностей", "Acme Bank JSC", len(batch.Entities))
	}
	if acme.ID != "vendor:acme-bank-jsc" {
		t.Errorf("ID = %q, очікували стабільний %q", acme.ID, "vendor:acme-bank-jsc")
	}
	if acme.SourceChunkID == "" {
		t.Error("SourceChunkID порожній — provenance втрачено")
	}

	empty, err := extractEntities(newNodeCtx(), Batch{Chunks: []Chunk{
		{ID: "t1", Kind: "table", Text: "| # | Description |\n|---|---|\n| 1 | щось |"},
	}})
	if err != nil {
		t.Fatalf("extractEntities(невідома таблиця) error = %v", err)
	}
	if len(empty.Entities) != 0 {
		t.Errorf("таблиця без відомих колонок дала %d сутностей, очікували 0", len(empty.Entities))
	}
}

// TestIndexVector_ChildrenOnly: TODO прямо каже — VectorsAdded == кількість
// children (ParentID != ""), parents не індексуються, і st.Vector.Len()
// узгоджений з VectorsAdded.
func TestIndexVector_ChildrenOnly(t *testing.T) {
	t.Parallel()
	batch := chunkedFixture(t)
	wantChildren := 0
	for _, c := range batch.Chunks {
		if c.ParentID != "" {
			wantChildren++
		}
	}

	st := NewStores()
	got, err := newIndexVector(st.Vector)(newNodeCtx(), batch)
	if err != nil {
		t.Fatalf("index_vector error = %v", err)
	}
	if got.VectorsAdded != wantChildren {
		t.Errorf("VectorsAdded = %d, очікували %d (кількість children)", got.VectorsAdded, wantChildren)
	}
	if st.Vector.Len() != wantChildren {
		t.Errorf("st.Vector.Len() = %d, очікували %d", st.Vector.Len(), wantChildren)
	}
}

// TestIndexGraph_DedupesAndLinksSameRow: NodesAdded ≤ len(Entities) (дублі
// Vendor у двох таблицях не створюють два вузли), і ребро Vendor→Finding
// реально з'являється для сутностей одного рядка таблиці знахідок.
func TestIndexGraph_DedupesAndLinksSameRow(t *testing.T) {
	t.Parallel()
	batch := chunkedFixture(t)
	if len(batch.Entities) == 0 {
		t.Fatal("фікстура не дала жодної сутності — перевірте extractEntities")
	}

	st := NewStores()
	got, err := newIndexGraph(st.Graph)(newNodeCtx(), batch)
	if err != nil {
		t.Fatalf("index_graph error = %v", err)
	}
	if got.NodesAdded <= 0 || got.NodesAdded > len(batch.Entities) {
		t.Errorf("NodesAdded = %d, очікували 0 < n ≤ %d (дублі злиті)", got.NodesAdded, len(batch.Entities))
	}

	node, ok := st.Graph.Node("vendor:acme-bank-jsc")
	if !ok {
		t.Fatal("вузол vendor:acme-bank-jsc не знайдено")
	}
	if len(node.Sources) < 2 {
		t.Errorf("Sources = %v; Acme згадується у двох таблицях, provenance мав злитись", node.Sources)
	}

	findings := st.Graph.Neighbors("vendor:acme-bank-jsc", "has_finding")
	if len(findings) == 0 {
		t.Fatal("немає жодного ребра vendor:acme-bank-jsc -has_finding-> *")
	}
	var gotF101 bool
	for _, f := range findings {
		if f.ID == "finding:f-101" {
			gotF101 = true
		}
	}
	if !gotF101 {
		t.Errorf("серед сусідів немає finding:f-101: %v", findings)
	}
}

// --- Вузол report ------------------------------------------------------------

// TestReport_RealFixture перевіряє саме той рядок, який потрібен Homework
// п.5 на справжніх даних: усі 3 таблиці фікстури цілі (Tables: 3/3), і
// кількість parents/children збігається з TestChunk_HierarchyAndSectionPath
// (5 parents, 10 children — лічилось незалежно тим самим документом).
func TestReport_RealFixture(t *testing.T) {
	t.Parallel()
	batch := chunkedFixture(t)
	got, err := report(newNodeCtx(), batch)
	if err != nil {
		t.Fatalf("report() error = %v", err)
	}
	for _, want := range []string{"5 parents", "10 children", "Tables: 3/3 preserved"} {
		if !strings.Contains(got, want) {
			t.Errorf("report() = %q; немає підрядка %q", got, want)
		}
	}
}

func TestReport(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		chunks   []Chunk
		contains string
	}{
		{name: "порожній вхід", chunks: nil, contains: "0"},
		{name: "один чанк", chunks: []Chunk{{ID: "c1"}}, contains: "1"},
		{name: "три чанки", chunks: []Chunk{{ID: "c1"}, {ID: "c2"}, {ID: "c3"}}, contains: "3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := report(newNodeCtx(), Batch{Chunks: tt.chunks})
			if err != nil {
				t.Fatalf("report() error = %v", err)
			}
			if !strings.Contains(got, tt.contains) {
				t.Errorf("report() = %q; очікували підрядок %q", got, tt.contains)
			}
		})
	}
}

// --- Граф цілком ------------------------------------------------------------

// TestGraph_EarlyWin проганяє весь граф так само, як `go run . console`, але
// без launcher-а: ранній win із Homework, відтворюваний у CI.
func TestGraph_EarlyWin(t *testing.T) {
	t.Parallel()
	a, err := newGraph(fakeConverter{}, NewStores())
	if err != nil {
		t.Fatalf("newGraph: %v", err)
	}
	res, err := labrun.Run(context.Background(), a, fixtureMD)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// 15 = 5 parents (Doc, 1 Scope, 2 Findings, 3 Tariffs and Signers,
	// 4 Conclusion) + 10 children — рахунок зафіксовано в
	// TestChunk_HierarchyAndSectionPath, тут перевіряємо лише що report
	// бачить саме цей Batch, а не щось втрачене графом по дорозі.
	if !strings.Contains(res.Final, "Отримано 15 чанків") {
		t.Errorf("фінальна відповідь = %q; очікували рядок вузла report із 15 чанками", res.Final)
	}
	// 7 вузлів після Start — 7 подій: жоден вузол не випав із ланцюжка.
	if got := len(res.Events); got != 7 {
		t.Errorf("подій = %d, очікували 7 (load … report)", got)
	}
}

// --- Контракт із ДЗ 6 --------------------------------------------------------

// TestChunkJSONContract: labs6 читає ваші чанки як JSON. Імена полів — це
// контракт між ДЗ 5 і ДЗ 6; якщо тест упав, ви перейменували поле.
func TestChunkJSONContract(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(Chunk{ID: "c", DocumentID: "d", ParentID: "p", Level: 2, Kind: "table", SectionPath: "s", Text: "t"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"id"`, `"document_id"`, `"parent_id"`, `"level"`, `"kind"`, `"section_path"`, `"text"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("JSON чанка без поля %s: %s", key, raw)
		}
	}
}
