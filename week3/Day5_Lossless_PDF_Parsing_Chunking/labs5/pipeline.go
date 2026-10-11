package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"google.golang.org/adk/v2/agent"

	"github.com/dimetron/ai-eng-course/labs/week3/Day5_Lossless_PDF_Parsing_Chunking/labs5/internal/chunkeval"
	"github.com/dimetron/ai-eng-course/labs/week3/Day5_Lossless_PDF_Parsing_Chunking/labs5/internal/kgraph"
	"github.com/dimetron/ai-eng-course/labs/week3/Day5_Lossless_PDF_Parsing_Chunking/labs5/internal/mdblocks"
	"github.com/dimetron/ai-eng-course/labs/week3/Day5_Lossless_PDF_Parsing_Chunking/labs5/internal/vecindex"
)

// Source — документ, який прийшов на вхід конвеєра.
type Source struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // "pdf" | "markdown"
}

// Document — документ після lossless-парсингу: увесь зміст у Markdown,
// таблиці — Markdown-таблицями із заголовками стовпчиків. Blocks — той самий
// Markdown, уже розкладений на заголовки / абзаци / списки / таблиці
// (internal/mdblocks). Саме з Blocks ви будуєте чанки.
type Document struct {
	ID       string           `json:"id"`
	Path     string           `json:"path"`
	Markdown string           `json:"markdown"`
	Blocks   []mdblocks.Block `json:"blocks"`
}

// Chunk — одиниця майбутньої бази знань Research Agent.
//
// Цей JSON-формат — контракт із ДЗ 6: labs6 читає саме такі чанки
// (див. labs6/testdata/chunks.json). Додавайте поля, але не перейменовуйте
// наявні — інакше ДЗ 6 не прочитає ваш індекс.
type Chunk struct {
	ID          string `json:"id"`
	DocumentID  string `json:"document_id"`
	ParentID    string `json:"parent_id,omitempty"`    // порожній для батьківських чанків
	Level       int    `json:"level"`                  // 0 = розділ, 1 = підрозділ, 2 = абзац/таблиця
	Kind        string `json:"kind"`                   // "heading" | "text" | "table" | "list"
	SectionPath string `json:"section_path,omitempty"` // напр. "2 Findings / 2.1 Security Findings"
	Text        string `json:"text"`
}

// Entity — сутність, витягнута з чанка (Vendor, Finding, Merchant, Contract…).
// SourceChunkID — provenance: без нього аудитор не знайде первинний фрагмент.
// Row — індекс рядка таблиці, звідки взято сутність; не йде в чанк-контракт
// ДЗ 6, служить лише index_graph, щоб з'єднати ребром сутності одного рядка
// (Vendor —has_finding→ Finding), не вигадуючи окремого формату для цього.
type Entity struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	SourceChunkID string `json:"source_chunk_id"`
	Row           int    `json:"row"`
}

// Batch тече вузлами після chunk: кожен вузол додає свою частину.
type Batch struct {
	Document     Document `json:"document"`
	Chunks       []Chunk  `json:"chunks"`
	Entities     []Entity `json:"entities,omitempty"`
	VectorsAdded int      `json:"vectors_added"`
	NodesAdded   int      `json:"nodes_added"`
}

// MarkdownConverter перетворює PDF на Markdown. У main.go це docling-mcp
// (internal/docling); у тестах — фейк без мережі й без Python.
type MarkdownConverter interface {
	ToMarkdown(ctx context.Context, path string) (string, error)
}

// Stores — сховища, які наповнюють вузли index_*. Вони живуть поза графом:
// вузли лишаються функціями, а після прогону тест (або ваш A1) читає сховища
// напряму.
type Stores struct {
	Vector *vecindex.Index
	Graph  *kgraph.Graph
}

// NewStores повертає порожні in-memory сховища.
func NewStores() Stores {
	return Stores{Vector: vecindex.New(), Graph: kgraph.New()}
}

// load перевіряє вхід: шлях із повідомлення користувача → Source.
// Тип визначається за вмістом (magic bytes %PDF), а не лише за розширенням.
func load(_ agent.Context, path string) (Source, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Source{}, fmt.Errorf("порожній шлях: введіть шлях до PDF або Markdown")
	}
	f, err := os.Open(path)
	if err != nil {
		return Source{}, fmt.Errorf("не вдалося прочитати %q: %w", path, err)
	}
	defer f.Close()
	head := make([]byte, 5)
	n, _ := f.Read(head)
	if bytes.HasPrefix(head[:n], []byte("%PDF-")) {
		return Source{Path: path, Kind: "pdf"}, nil
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".txt":
		return Source{Path: path, Kind: "markdown"}, nil
	}
	return Source{}, fmt.Errorf("%q: непідтримуваний формат — очікуємо PDF або Markdown", path)
}

// newParse повертає вузол parse. PDF іде в docling-mcp, Markdown читається як є;
// далі mdblocks розкладає Markdown на блоки.
//
// TODO(студент): перевірте на своєму PDF, що кожна таблиця вийшла цілою
// Markdown-таблицею із заголовками (mdblocks.Tables(doc.Blocks) — швидкий
// лічильник). Сканований PDF без текстового шару потребує OCR — docling уміє,
// але це повільніше; зазначте в README.
func newParse(conv MarkdownConverter) func(agent.Context, Source) (Document, error) {
	return func(ctx agent.Context, src Source) (Document, error) {
		var md string
		switch src.Kind {
		case "pdf":
			out, err := conv.ToMarkdown(ctx, src.Path)
			if err != nil {
				return Document{}, fmt.Errorf("parse %q: %w", src.Path, err)
			}
			md = out
		case "markdown":
			data, err := os.ReadFile(src.Path)
			if err != nil {
				return Document{}, fmt.Errorf("parse %q: %w", src.Path, err)
			}
			md = string(data)
		default:
			return Document{}, fmt.Errorf("parse: невідомий тип %q", src.Kind)
		}
		if strings.TrimSpace(md) == "" {
			return Document{}, fmt.Errorf("parse %q: порожній документ", src.Path)
		}
		return newDocument(documentID(src.Path), src.Path, md), nil
	}
}

// newDocument — єдиний спосіб зібрати Document: Blocks завжди відповідають
// Markdown. У своїх тестах будуйте документи теж через нього.
func newDocument(id, path, md string) Document {
	return Document{ID: id, Path: path, Markdown: md, Blocks: mdblocks.Parse(md)}
}

// documentID — стабільний ідентифікатор документа з імені файла.
func documentID(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// chunk нарізає документ ієрархічно.
//
// TODO(студент): Parent-Child chunking — головна частина ДЗ. Ідіть по
// doc.Blocks (вони вже розібрані, див. internal/mdblocks):
//  1. KindHeading з Depth == 1 відкриває новий parent (Level 0, ParentID "").
//     Depth > 1 — підрозділ: оновіть SectionPath ("2 Findings / 2.1 …").
//     Depth == 0 — заголовок без номера (назва документа): теж parent.
//  2. Решта блоків — children (Level 2) поточного parent: ParentID,
//     SectionPath, DocumentID заповнені; Kind = string(block.Kind).
//  3. KindTable — РІВНО один child, Text = block.Text без змін. Не ріжте.
//  4. Довгий KindText (> maxChildTokens, див. chunkeval.Tokens) — розбийте
//     по реченнях на кілька children.
//  5. Блок до першого заголовка теж потребує parent — створіть неявний.
//
// Текст parent — його заголовок (як у labs6/testdata/chunks.json).
func chunk(_ agent.Context, doc Document) (Batch, error) {
	if strings.TrimSpace(doc.Markdown) == "" {
		return Batch{}, fmt.Errorf("порожній документ")
	}

	b := &chunkBuilder{docID: doc.ID}
	for _, block := range doc.Blocks {
		if block.Kind == mdblocks.KindHeading {
			b.openHeading(block)
			continue
		}
		b.addBlock(block)
	}
	return Batch{Document: doc, Chunks: b.chunks}, nil
}

// chunkBuilder tracks the running state Parent-Child chunking needs while
// walking doc.Blocks once, in order: the current parent's ID, the section
// path two levels deep (top heading / current subsection), and monotonic
// counters so every chunk ID is unique and stable within one document.
type chunkBuilder struct {
	docID  string
	chunks []Chunk

	parentID   string
	haveParent bool
	topTitle   string // current Depth-1 (or Depth-0) heading
	subTitle   string // current Depth>1 heading, "" until one is seen

	parentSeq int
	childSeq  int
}

// sectionPath is "top / sub" once a subsection heading has been seen, or just
// "top" before that — the shape labs6/testdata/chunks.json expects.
func (b *chunkBuilder) sectionPath() string {
	if b.subTitle == "" {
		return b.topTitle
	}
	return b.topTitle + " / " + b.subTitle
}

// openHeading applies rule 1: Depth 0 or 1 starts a new parent; Depth>1 only
// updates the running subsection title of the CURRENT parent.
func (b *chunkBuilder) openHeading(block mdblocks.Block) {
	if block.Depth <= 1 {
		b.openParent(block.Title)
		return
	}
	if !b.haveParent {
		// A subsection header with no parent heading before it — rule 5's
		// "implicit parent" case, just triggered by a heading instead of a
		// plain block.
		b.openParent("")
	}
	b.subTitle = block.Title
}

// openParent starts a new Level-0 chunk and resets the subsection tracker:
// a new top heading always closes whatever subsection came before it.
func (b *chunkBuilder) openParent(title string) {
	b.parentSeq++
	b.parentID = fmt.Sprintf("%s-p%d", b.docID, b.parentSeq)
	b.chunks = append(b.chunks, Chunk{
		ID:          b.parentID,
		DocumentID:  b.docID,
		Level:       0,
		Kind:        "heading",
		SectionPath: title,
		Text:        title,
	})
	b.topTitle = title
	b.subTitle = ""
	b.haveParent = true
}

// addBlock applies rules 2-5 for one non-heading block: a table becomes
// exactly one child (rule 3); long text is split by sentences (rule 4);
// everything else is one child as-is. A block arriving before any heading at
// all gets an implicit, titleless parent first (rule 5).
func (b *chunkBuilder) addBlock(block mdblocks.Block) {
	if !b.haveParent {
		b.openParent("")
	}
	if block.Kind == mdblocks.KindText {
		for _, part := range splitLongText(block.Text, maxChildTokens) {
			b.addChild(string(block.Kind), part)
		}
		return
	}
	b.addChild(string(block.Kind), block.Text)
}

func (b *chunkBuilder) addChild(kind, text string) {
	b.childSeq++
	b.chunks = append(b.chunks, Chunk{
		ID:          fmt.Sprintf("%s-c%d", b.docID, b.childSeq),
		DocumentID:  b.docID,
		ParentID:    b.parentID,
		Level:       2,
		Kind:        kind,
		SectionPath: b.sectionPath(),
		Text:        text,
	})
}

// sentenceRE splits on '.', '!' or '?' followed by whitespace (or end of
// text) — a stable, offline approximation, not real sentence segmentation
// (an abbreviation like "e.g." would split early). Good enough for the
// token-budget rule below, which only needs sentence-ish boundaries.
var sentenceRE = regexp.MustCompile(`[^.!?]+[.!?]+(\s+|$)`)

// splitLongText breaks text into chunkeval.Tokens-bounded pieces on sentence
// boundaries, never splitting a sentence itself. text under the limit comes
// back as a single-element slice unchanged.
func splitLongText(text string, maxTokens int) []string {
	if chunkeval.Tokens(text) <= maxTokens {
		return []string{text}
	}
	sentences := sentenceRE.FindAllString(text, -1)
	if len(sentences) == 0 {
		return []string{text} // no sentence boundary found — keep it whole
	}
	var out []string
	var cur strings.Builder
	curTokens := 0
	for _, s := range sentences {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		n := chunkeval.Tokens(s)
		if curTokens > 0 && curTokens+n > maxTokens {
			out = append(out, cur.String())
			cur.Reset()
			curTokens = 0
		}
		if cur.Len() > 0 {
			cur.WriteByte(' ')
		}
		cur.WriteString(s)
		curTokens += n
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// maxChildTokens — стеля довжини текстового child (у chunkeval.Tokens).
// Таблиці на неї не зважають: краще великий цілий чанк, ніж дві половини.
const maxChildTokens = 200

// entityColumn pairs a table column name (matched case-insensitively, same
// as Table.Cell) with the Entity.Type it names. A slice, not a map: order is
// the priority newIndexGraph uses to pick which entity of a row is the edge's
// From side (Vendor before Finding gives Vendor —has_finding→ Finding, the
// TODO's own example, without hand-coding that pairing separately).
type entityColumn struct {
	column string
	typ    string
}

// One table can match several entries at once (the findings table names both
// a Finding and the Vendor it is against) — probing every known column on
// every table is simpler and more robust than hand-listing which table has
// which columns, and a table matching none of them (e.g. the ASVS requirement
// tables, which have no Vendor/Merchant/Contract column) safely yields no
// entities.
var entityColumns = []entityColumn{
	{"Vendor", "Vendor"},
	{"Finding ID", "Finding"},
	{"Merchant", "Merchant"},
	{"Contract", "Contract"},
}

// extractEntities витягує сутності з чанків.
//
// Читає сутності з чанків Kind == "table": кожен child chunk, що зберіг
// таблицю (chunk не ріже їх — правило 3), парситься назад у Table через
// mdblocks.Parse, а відомі колонки (entityColumns) дають типізовані сутності.
// Row — індекс рядка таблиці: newIndexGraph використовує його, щоб з'єднати
// ребром сутності одного рядка. SourceChunkID — ID саме цього table chunk, так
// аудитор знаходить первинний рядок. Дублікати між таблицями (той самий
// Vendor у таблиці вендорів і в таблиці знахідок) тут НЕ зливаються —
// resolution/merge — робота index_graph (AddNode повертає false на повтор).
func extractEntities(_ agent.Context, b Batch) (Batch, error) {
	for _, c := range b.Chunks {
		if c.Kind != "table" {
			continue
		}
		blocks := mdblocks.Parse(c.Text)
		if len(blocks) == 0 || blocks[0].Table == nil {
			continue // таблиця не розпарсилась назад — зафіксувати як межу в README, не падати
		}
		tbl := *blocks[0].Table
		for row := range tbl.Rows {
			for _, ec := range entityColumns {
				name, ok := tbl.Cell(row, ec.column)
				if !ok || name == "" {
					continue
				}
				b.Entities = append(b.Entities, Entity{
					ID:            entityID(ec.typ, name),
					Type:          ec.typ,
					Name:          name,
					SourceChunkID: c.ID,
					Row:           row,
				})
			}
		}
	}
	return b, nil
}

// entityID builds the stable ID the TODO asks for: "vendor:acme-bank-jsc".
// Stable across runs and across tables — the same Vendor named in two tables
// gets the same ID, which is exactly what lets index_graph recognise it as
// the same node instead of a duplicate.
func entityID(entityType, name string) string {
	return strings.ToLower(entityType) + ":" + slugify(name)
}

var slugNonAlnumRE = regexp.MustCompile(`[^a-z0-9]+`)

// slugify lowercases name and replaces every run of non-alphanumeric
// characters with one hyphen, trimming leading/trailing hyphens — "Acme Bank
// JSC" → "acme-bank-jsc".
func slugify(name string) string {
	s := slugNonAlnumRE.ReplaceAllString(strings.ToLower(name), "-")
	return strings.Trim(s, "-")
}

// newIndexVector повертає вузол index_vector над idx.
//
// TODO(студент): для кожного child (ParentID != "") — idx.Add(c.ID, c.Text)
// і VectorsAdded++. Parents не індексуйте: їх підвантажують за ParentID
// після пошуку. Ембединг — стаб (internal/vecindex), так і напишіть у README.
// Справжні вектори — internal/embed: нульовий embed.Client — той самий
// детермінований стаб на чистому Go, embed.New() з
// OLLAMA_EMBED_MODEL=embeddinggemma — модель Ollama
// (https://docs.ollama.com/capabilities/embeddings); пошук — embed.Rerank.
func newIndexVector(idx *vecindex.Index) func(agent.Context, Batch) (Batch, error) {
	return func(_ agent.Context, b Batch) (Batch, error) {
		for _, c := range b.Chunks {
			if c.ParentID == "" {
				continue // parent — немає сенсу шукати по заголовку окремо від дітей
			}
			if err := idx.Add(c.ID, c.Text); err != nil {
				return b, fmt.Errorf("index_vector: %s: %w", c.ID, err)
			}
			b.VectorsAdded++
		}
		return b, nil
	}
}

// newIndexGraph повертає вузол index_graph над g.
//
// TODO(студент): кожна Entity → g.AddNode(kgraph.Node{ID, Type, Name,
// Sources: []string{SourceChunkID}}); NodesAdded++ лише коли AddNode
// повернув true (повтор — це злиття provenance, не новий вузол). Ребра між
// сутностями одного рядка таблиці (Vendor —has_finding→ Finding) — g.AddEdge.
func newIndexGraph(g *kgraph.Graph) func(agent.Context, Batch) (Batch, error) {
	return func(_ agent.Context, b Batch) (Batch, error) {
		for _, e := range b.Entities {
			added, err := g.AddNode(kgraph.Node{ID: e.ID, Type: e.Type, Name: e.Name, Sources: []string{e.SourceChunkID}})
			if err != nil {
				return b, fmt.Errorf("index_graph: node %s: %w", e.ID, err)
			}
			if added {
				b.NodesAdded++
			}
		}
		// Ребра між сутностями одного рядка таблиці: entityColumns перевіряє
		// Vendor перед Finding/Merchant/Contract (той самий порядок, що в
		// extractEntities), тож перша сутність рядка — природний "From"
		// (Vendor —has_finding→ Finding), без окремого хардкоду пар типів.
		rows := map[string][]Entity{}
		var order []string
		for _, e := range b.Entities {
			key := fmt.Sprintf("%s#%d", e.SourceChunkID, e.Row)
			if _, ok := rows[key]; !ok {
				order = append(order, key)
			}
			rows[key] = append(rows[key], e)
		}
		for _, key := range order {
			row := rows[key]
			if len(row) < 2 {
				continue // рядок дав лише один тип сутності — нема з чим з'єднати
			}
			from := row[0]
			for _, to := range row[1:] {
				if _, err := g.AddEdge(kgraph.Edge{
					From:   from.ID,
					Rel:    "has_" + strings.ToLower(to.Type),
					To:     to.ID,
					Source: from.SourceChunkID,
				}); err != nil {
					return b, fmt.Errorf("index_graph: edge %s->%s: %w", from.ID, to.ID, err)
				}
			}
		}
		return b, nil
	}
}

// report рахує статистику нарізки.
//
// TODO(студент): кількість чанків за рівнями (parents/children),
// min/avg/max у токенах, `Tables: K/K preserved`. Готові лічильники — у
// internal/chunkeval: Measure (min/avg/max), Preserved (скільки таблиць
// документа лежать цілими в чанках; таблиці документа —
// mdblocks.Tables(b.Document.Blocks)), NaiveWindows (baseline для README).
func report(_ agent.Context, b Batch) (string, error) {
	var parents, children int
	var childTexts, chunkTexts []string
	for _, c := range b.Chunks {
		chunkTexts = append(chunkTexts, c.Text)
		if c.ParentID == "" {
			parents++
			continue
		}
		children++
		childTexts = append(childTexts, c.Text)
	}
	sizes := chunkeval.Measure(childTexts)

	tables := mdblocks.Tables(b.Document.Blocks)
	tableTexts := make([]string, len(tables))
	for i, t := range tables {
		tableTexts[i] = t.Text
	}
	preserved := chunkeval.Preserved(tableTexts, chunkTexts)

	return fmt.Sprintf(
		"Отримано %d чанків (%d parents, %d children). Розміри children (токени): %s. Tables: %d/%d preserved.",
		len(b.Chunks), parents, children, sizes, preserved, len(tables),
	), nil
}
