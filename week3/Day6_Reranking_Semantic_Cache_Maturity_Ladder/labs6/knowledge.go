// knowledge.go builds the Merchant -> Tariff / Contract -> Signer graph the
// Homework asks graph_filter and graph_traversal to query (Homework.md,
// "Якщо щось не працює": "Граф Merchant → Contract → Signer для
// graph_traversal побудуйте з цих чанків самі").
//
// Extraction is deliberately narrow: one known table shape (the tariffs
// table labs5 already produces) and one known sentence shape (signer
// lines). This corpus is fixed and small — a general entity extractor
// belongs in Lab 5 (internal/mdblocks + entity columns), not duplicated
// here for a one-off graph.
package main

import (
	"regexp"
	"strings"

	"github.com/dimetron/ai-eng-course/labs/week3/Day6_Reranking_Semantic_Cache_Maturity_Ladder/labs6/internal/corpus"
	"github.com/dimetron/ai-eng-course/labs/week3/Day6_Reranking_Semantic_Cache_Maturity_Ladder/labs6/internal/kgraph"
)

// Entity types this graph knows about.
const (
	entityMerchant = "Merchant"
	entityTariff   = "Tariff"
	entityContract = "Contract"
	entitySigner   = "Signer"
)

// signerRE captures "Договір <contract> підписав(ла|...) <Ім'я Прізвище>" —
// the exact shape the LEDGERWORKS fixture's c-sign-* chunks use. The verb
// stem is "підписа" (not "підписав"): masculine adds "в", feminine adds
// "ла" — "підписав" and "підписала" share only the first 7 letters, not 8,
// so \S* has to start right after the stem, not after the masculine form.
var signerRE = regexp.MustCompile(`Договір\s+(CT-\d{4}-\d{3})\s+підписа\S*\s+([А-ЯІЇЄҐ][\p{L}'’]+\s+[А-ЯІЇЄҐ][\p{L}'’]+)`)

// entityID builds a stable id ("merchant:a-114") — same scheme as Lab 5's
// extract_entities, so the two labs' graphs would merge cleanly if they ever
// shared a store.
func entityID(entityType, name string) string {
	return strings.ToLower(entityType) + ":" + slugify(name)
}

// slugNonAlnumRE matches runs of anything that is NOT a Unicode letter or
// digit. \p{L}/\p{N}, not the ASCII a-z0-9 Lab 5 uses for its (Latin-script)
// vendor names: signer names here are Cyrillic ("Олег Мельник"), and an
// ASCII-only class would strip every character, collapsing every name to
// the same empty id — which is exactly the bug this comment is replacing.
var slugNonAlnumRE = regexp.MustCompile(`[^\p{L}\p{N}]+`)

func slugify(name string) string {
	return strings.Trim(slugNonAlnumRE.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

// buildGraph scans every chunk once: table chunks feed indexTariffTable
// (Merchant/Tariff/Contract), and any chunk whose text matches signerRE
// feeds the Contract -signed_by-> Signer edge, regardless of Kind — the
// fixture's signer sentences are Kind "list", but the rule does not depend
// on that.
func buildGraph(chunks []corpus.Chunk) (*kgraph.Graph, error) {
	g := kgraph.New()
	for _, c := range chunks {
		if c.Kind == "table" {
			if err := indexTariffTable(g, c); err != nil {
				return nil, err
			}
		}
		if err := indexSigner(g, c); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// indexTariffTable reads one table chunk. A table without both a Merchant
// and a Tariff column (the Vendors and Findings tables in this corpus) is
// silently skipped — it is not an error, just not this table.
func indexTariffTable(g *kgraph.Graph, c corpus.Chunk) error {
	header, rows, ok := parsePipeTable(c.Text)
	if !ok {
		return nil
	}
	merchantCol, tariffCol, contractCol := colIndex(header, "Merchant"), colIndex(header, "Tariff"), colIndex(header, "Contract")
	if merchantCol < 0 || tariffCol < 0 {
		return nil
	}
	for _, row := range rows {
		merchantName, tariffName := cellAt(row, merchantCol), cellAt(row, tariffCol)
		if merchantName == "" || tariffName == "" {
			continue
		}
		merchantID, tariffID := entityID(entityMerchant, merchantName), entityID(entityTariff, tariffName)
		if _, err := g.AddNode(kgraph.Node{ID: merchantID, Type: entityMerchant, Name: merchantName, Sources: []string{c.ID}}); err != nil {
			return err
		}
		if _, err := g.AddNode(kgraph.Node{ID: tariffID, Type: entityTariff, Name: tariffName, Sources: []string{c.ID}}); err != nil {
			return err
		}
		if _, err := g.AddEdge(kgraph.Edge{From: merchantID, Rel: "has_tariff", To: tariffID, Source: c.ID}); err != nil {
			return err
		}
		if contractName := cellAt(row, contractCol); contractCol >= 0 && contractName != "" {
			contractID := entityID(entityContract, contractName)
			if _, err := g.AddNode(kgraph.Node{ID: contractID, Type: entityContract, Name: contractName, Sources: []string{c.ID}}); err != nil {
				return err
			}
			if _, err := g.AddEdge(kgraph.Edge{From: merchantID, Rel: "has_contract", To: contractID, Source: c.ID}); err != nil {
				return err
			}
		}
	}
	return nil
}

// indexSigner adds the Signer node and the Contract -signed_by-> Signer
// edge for one chunk, if it matches signerRE. A Contract node should
// already exist from the tariffs table; if this chunk's contract ID is not
// among them (data drift, or a signer chunk for a contract the tariffs
// table never listed), a stub Contract node is created so the edge still
// has somewhere to land — provenance then degrades to "the signer chunk
// only", which stays auditable rather than silently dropping the fact.
func indexSigner(g *kgraph.Graph, c corpus.Chunk) error {
	m := signerRE.FindStringSubmatch(c.Text)
	if m == nil {
		return nil
	}
	contractName, signerName := m[1], m[2]
	contractID, signerID := entityID(entityContract, contractName), entityID(entitySigner, signerName)
	if _, err := g.AddNode(kgraph.Node{ID: signerID, Type: entitySigner, Name: signerName, Sources: []string{c.ID}}); err != nil {
		return err
	}
	if _, ok := g.Node(contractID); !ok {
		if _, err := g.AddNode(kgraph.Node{ID: contractID, Type: entityContract, Name: contractName, Sources: []string{c.ID}}); err != nil {
			return err
		}
	}
	if _, err := g.AddEdge(kgraph.Edge{From: contractID, Rel: "signed_by", To: signerID, Source: c.ID}); err != nil {
		return err
	}
	return nil
}

// signerProvenance maps "<contractID>|<signerID>" to the chunk that fact
// came from. buildGraph already walks the same sentences (indexSigner), but
// Graph.Neighbors returns Nodes, not Edges, so it drops the per-edge Source —
// a Signer's merged Sources (every contract they signed) isn't enough to
// cite the ONE sentence that supports "this contract, this signer". This is
// the lookup graph_traversal (retrieval.go) needs to keep that citation
// precise instead of citing "one of this signer's contracts, pick any".
func signerProvenance(chunks []corpus.Chunk) map[string]string {
	out := map[string]string{}
	for _, c := range chunks {
		m := signerRE.FindStringSubmatch(c.Text)
		if m == nil {
			continue
		}
		contractID, signerID := entityID(entityContract, m[1]), entityID(entitySigner, m[2])
		out[contractID+"|"+signerID] = c.ID
	}
	return out
}

// parsePipeTable splits a Markdown pipe table's Text (header row, "|---|"
// separator, data rows — exactly what Lab 5's chunker stores verbatim) into
// a header and rows. ok is false for anything that is not a pipe table at
// all, so callers can skip non-table chunks without treating that as an
// error.
func parsePipeTable(text string) (header []string, rows [][]string, ok bool) {
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "|") {
		return nil, nil, false
	}
	header = splitRow(lines[0])
	for _, l := range lines[2:] { // lines[1] is the "|---|---|" separator
		rows = append(rows, splitRow(l))
	}
	return header, rows, true
}

func splitRow(line string) []string {
	parts := strings.Split(strings.Trim(line, "|"), "|")
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = strings.TrimSpace(p)
	}
	return out
}

func colIndex(header []string, name string) int {
	for i, h := range header {
		if strings.EqualFold(h, name) {
			return i
		}
	}
	return -1
}

func cellAt(row []string, idx int) string {
	if idx < 0 || idx >= len(row) {
		return ""
	}
	return row[idx]
}
