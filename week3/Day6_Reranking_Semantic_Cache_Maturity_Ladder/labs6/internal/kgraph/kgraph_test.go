package kgraph

import (
	"reflect"
	"testing"
)

func TestAddNode(t *testing.T) {
	t.Parallel()
	var g Graph // zero value is usable
	tests := []struct {
		name      string
		node      Node
		wantAdded bool
		wantErr   bool
	}{
		{"new", Node{ID: "merchant:a-114", Type: "Merchant", Name: "A-114", Sources: []string{"c1"}}, true, false},
		{"repeat merges sources", Node{ID: "merchant:a-114", Type: "Merchant", Name: "A-114", Sources: []string{"c1", "c7"}}, false, false},
		{"no id", Node{Sources: []string{"c1"}}, false, true},
		{"no provenance", Node{ID: "merchant:x"}, false, true},
	}
	for _, tt := range tests { // sequential: cases share g
		added, err := g.AddNode(tt.node)
		if added != tt.wantAdded || (err != nil) != tt.wantErr {
			t.Errorf("%s: AddNode = %v, %v; want %v, err=%v", tt.name, added, err, tt.wantAdded, tt.wantErr)
		}
	}
	n, ok := g.Node("merchant:a-114")
	if !ok || !reflect.DeepEqual(n.Sources, []string{"c1", "c7"}) {
		t.Errorf("Node = %+v, %v; want sources [c1 c7]", n, ok)
	}
	n.Sources[0] = "mutated"
	if again, _ := g.Node("merchant:a-114"); again.Sources[0] != "c1" {
		t.Error("Node returned a shared slice")
	}
	if _, ok := g.Node("ghost"); ok {
		t.Error("Node(ghost) found")
	}
}

func TestEdgesAndQueries(t *testing.T) {
	t.Parallel()
	g := New()
	for _, n := range []Node{
		{ID: "merchant:a-114", Type: "Merchant", Name: "A-114"},
		{ID: "contract:ct-2025-014", Type: "Contract", Name: "CT-2025-014"},
		{ID: "tariff:t-2", Type: "Tariff", Name: "T-2"},
		{ID: "signer:oleg", Type: "Signer", Name: "Олег Мельник"},
	} {
		n.Sources = []string{"t1"}
		if _, err := g.AddNode(n); err != nil {
			t.Fatal(err)
		}
	}
	edges := []struct {
		e         Edge
		wantAdded bool
		wantErr   bool
	}{
		{Edge{From: "merchant:a-114", Rel: "has_contract", To: "contract:ct-2025-014", Source: "t1"}, true, false},
		{Edge{From: "merchant:a-114", Rel: "has_tariff", To: "tariff:t-2", Source: "t1"}, true, false},
		{Edge{From: "contract:ct-2025-014", Rel: "signed_by", To: "signer:oleg", Source: "t1"}, true, false},
		{Edge{From: "merchant:a-114", Rel: "has_contract", To: "contract:ct-2025-014", Source: "t2"}, false, false},
		{Edge{From: "merchant:a-114", Rel: "has_contract", To: "ghost"}, false, true},
		{Edge{From: "ghost", Rel: "has_contract", To: "merchant:a-114"}, false, true},
	}
	for _, tt := range edges {
		added, err := g.AddEdge(tt.e)
		if added != tt.wantAdded || (err != nil) != tt.wantErr {
			t.Errorf("AddEdge(%+v) = %v, %v; want %v, err=%v", tt.e, added, err, tt.wantAdded, tt.wantErr)
		}
	}
	if n, e := g.Stats(); n != 4 || e != 3 {
		t.Errorf("Stats = %d nodes, %d edges; want 4, 3", n, e)
	}
	ids := func(ns []Node) []string {
		var out []string
		for _, n := range ns {
			out = append(out, n.ID)
		}
		return out
	}
	if got := ids(g.Neighbors("merchant:a-114", "")); !reflect.DeepEqual(got, []string{"contract:ct-2025-014", "tariff:t-2"}) {
		t.Errorf("Neighbors = %v", got)
	}
	if got := g.Neighbors("merchant:a-114", "signed_by"); len(got) != 0 {
		t.Errorf("Neighbors(wrong rel) = %v", got)
	}
	if got := ids(g.ByType("Signer")); !reflect.DeepEqual(got, []string{"signer:oleg"}) {
		t.Errorf("ByType = %v", got)
	}
}
