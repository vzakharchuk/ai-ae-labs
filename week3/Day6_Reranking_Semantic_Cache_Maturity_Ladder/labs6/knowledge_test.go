package main

import (
	"testing"

	"github.com/dimetron/ai-eng-course/labs/week3/Day6_Reranking_Semantic_Cache_Maturity_Ladder/labs6/internal/corpus"
)

func testChunks(t *testing.T) []corpus.Chunk {
	t.Helper()
	chunks, err := corpus.Load(defaultChunks)
	if err != nil {
		t.Fatal(err)
	}
	return chunks
}

// TestBuildGraph_MerchantTariffContract перевіряє основний ланцюг на
// фікстурі: Merchant A-114 → Tariff T-2 → Contract CT-2025-014, зі
// стабільними ID і provenance на таблицю тарифів.
func TestBuildGraph_MerchantTariffContract(t *testing.T) {
	t.Parallel()
	g, err := buildGraph(testChunks(t))
	if err != nil {
		t.Fatal(err)
	}
	if nodes, edges := g.Stats(); nodes != 10 || edges != 9 {
		t.Fatalf("Stats = %d nodes, %d edges; want 10, 9", nodes, edges)
	}
	merchant, ok := g.Node("merchant:a-114")
	if !ok || merchant.Sources[0] != "t-tariffs" {
		t.Fatalf("merchant:a-114 = %+v, ok=%v", merchant, ok)
	}
	var sawTariff, sawContract bool
	for _, n := range g.Neighbors("merchant:a-114", "") {
		switch n.ID {
		case "tariff:t-2":
			sawTariff = true
		case "contract:ct-2025-014":
			sawContract = true
		}
	}
	if !sawTariff || !sawContract {
		t.Errorf("merchant:a-114 neighbours missing tariff/contract: tariff=%v contract=%v", sawTariff, sawContract)
	}
}

// TestBuildGraph_SignerSplitsByGenderedVerb — реальний баг, знайдений на
// цьому самому корпусі: "підписав" і "підписала" різняться в корені
// ("підписа" + "в"/"ла"), а не лише суфіксом. Це тест на те, що обидва
// підписанти впізнаються як ОКРЕМІ сутності, не зливаються в одну.
func TestBuildGraph_SignerSplitsByGenderedVerb(t *testing.T) {
	t.Parallel()
	g, err := buildGraph(testChunks(t))
	if err != nil {
		t.Fatal(err)
	}
	signers := g.ByType("Signer")
	if len(signers) != 2 {
		t.Fatalf("Signer nodes = %d, want 2 (Олег Мельник, Ірина Бойко): %v", len(signers), signers)
	}
	oleg, ok := g.Node("signer:олег-мельник")
	if !ok {
		t.Fatal("signer:олег-мельник not found")
	}
	if len(oleg.Sources) != 2 {
		t.Errorf("Олег Мельник sources = %v, want 2 contracts merged (CT-2025-014, CT-2025-007)", oleg.Sources)
	}
	if _, ok := g.Node("signer:ірина-бойко"); !ok {
		t.Error("signer:ірина-бойко not found — \"підписала\" (feminine) was not recognised")
	}
}

// TestBuildGraph_SignerTraversal — the Contract -signed_by-> Signer edge
// graph_traversal will walk.
func TestBuildGraph_SignerTraversal(t *testing.T) {
	t.Parallel()
	g, err := buildGraph(testChunks(t))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"contract:ct-2025-014": "signer:олег-мельник",
		"contract:ct-2025-031": "signer:ірина-бойко",
		"contract:ct-2025-007": "signer:олег-мельник",
	}
	for contract, wantSigner := range tests {
		signers := g.Neighbors(contract, "signed_by")
		if len(signers) != 1 || signers[0].ID != wantSigner {
			t.Errorf("Neighbors(%s, signed_by) = %v, want [%s]", contract, signers, wantSigner)
		}
	}
}

// TestBuildGraph_SkipsUnrelatedTables: the Vendors and Findings tables in
// this corpus have neither a Merchant nor a Tariff column — indexTariffTable
// must skip them quietly, not error or invent entities from the wrong table.
func TestBuildGraph_SkipsUnrelatedTables(t *testing.T) {
	t.Parallel()
	g, err := buildGraph(testChunks(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"merchant:acme-bank-jsc", "merchant:f-101"} {
		if _, ok := g.Node(id); ok {
			t.Errorf("%s should not exist — it would mean the Vendors/Findings tables leaked into Merchant extraction", id)
		}
	}
}

func TestParsePipeTable(t *testing.T) {
	t.Parallel()
	header, rows, ok := parsePipeTable("| A | B |\n|---|---|\n| 1 | 2 |\n| 3 | 4 |")
	if !ok || len(header) != 2 || len(rows) != 2 {
		t.Fatalf("parsePipeTable = %v, %v, %v", header, rows, ok)
	}
	if rows[0][0] != "1" || rows[1][1] != "4" {
		t.Errorf("rows = %v", rows)
	}
	if _, _, ok := parsePipeTable("звичайний текст без таблиці"); ok {
		t.Error("non-table text reported ok=true")
	}
}
