// document.go — зведення трьох розділів у документ, стримування збою
// (Homework п.6) і облік токенів (п.7).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/dimetron/ai-eng-course/labs/internal/labrun"
)

// sectionOrder — порядок розділів документа. Фіксований список, не ітерація
// мапи JoinNode-результату: той самий ключ мапи в Go не гарантує порядку між
// запусками, а порядок розділів у документі, який читає людина, має бути
// стабільним.
var sectionOrder = []string{"risk_desk", "compliance_desk", "integration_desk"}

// Section — один розділ документа: що сказав спеціаліст, чи заземлено це на
// факти з інструмента (Status), і якщо ні — чому.
type Section struct {
	Node     string    `json:"node"`
	Text     string    `json:"text"`
	Findings []Finding `json:"findings"`
	// Status — "verified" (інструмент відповів, факти є) або "unverified"
	// (інструмент не відповів або відповідь без жодного факту).
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Verified reports whether this section is grounded in a real tool result.
func (s Section) Verified() bool { return s.Status == "verified" }

// Usage — токени одного LLM-виклику, по авторах (Homework п.7).
type Usage struct {
	Author string `json:"author"`
	Prompt int32  `json:"prompt"`
	Output int32  `json:"output"`
	Total  int32  `json:"total"`
}

// Document — зведений підсумок команди: розділи в фіксованому порядку плюс
// хеш, яким approval gate прив'язує схвалення до ЦІЄЇ версії тексту.
type Document struct {
	Sections []Section `json:"sections"`
	Hash     string    `json:"hash"`
	Usage    []Usage   `json:"usage"`
}

// HasUnverified reports whether any section is not grounded in a tool fact —
// the signal that must block approve regardless of the human's decision
// (Homework п.6, "сильна форма твердження").
func (d Document) HasUnverified() (bool, []string) {
	var reasons []string
	for _, s := range d.Sections {
		if !s.Verified() {
			reasons = append(reasons, fmt.Sprintf("%s: %s", s.Node, s.Error))
		}
	}
	return len(reasons) > 0, reasons
}

// runSpecialist runs one specialist agent in its OWN isolated session
// (labrun.Run — fresh session.InMemoryService() per call), so a sibling's
// context never leaks in, and catches any failure INSIDE the Section instead
// of letting it propagate as a Go error that would abort the whole engine
// run. A gap is a value, not a crash — the same containment principle Day 8
// applied to tool calls, applied here to a whole specialist branch.
func runSpecialist(ctx context.Context, node string, ag agent.Agent, task string) (Section, []Usage) {
	res, err := labrun.Run(ctx, ag, task)
	usage := usageOf(node, res.Events)
	if err != nil {
		return Section{Node: node, Status: "unverified", Error: err.Error()}, usage
	}
	findings := findingsOf(res.ToolResults)
	if len(findings) == 0 {
		reason := "інструмент не повернув жодного факту"
		if toolErr := toolErrorOf(res.ToolResults); toolErr != "" {
			reason = toolErr
		}
		return Section{Node: node, Text: res.Final, Status: "unverified", Error: reason}, usage
	}
	return Section{Node: node, Text: res.Final, Findings: findings, Status: "verified"}, usage
}

// findingsOf decodes every FunctionResponse's "findings" field — the ground
// truth from the tool, not the model's paraphrase of it. A model that
// invents a fact the tool never returned cannot make it appear here.
func findingsOf(results []*genai.FunctionResponse) []Finding {
	var out []Finding
	for _, r := range results {
		if r == nil {
			continue
		}
		raw, ok := r.Response["findings"]
		if !ok {
			continue
		}
		b, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var fs []Finding
		if json.Unmarshal(b, &fs) == nil {
			out = append(out, fs...)
		}
	}
	return out
}

// toolErrorOf returns the first tool error message found, if any. The ADK
// runtime itself turns a tool's Go error into {"error": msg} on the
// FunctionResponse rather than failing the whole run (verified in
// internal/llminternal/base_flow.go's callTool) — so an unverified section's
// Error here is the SAME message the model saw, not something we guessed.
func toolErrorOf(results []*genai.FunctionResponse) string {
	for _, r := range results {
		if r == nil {
			continue
		}
		if msg, ok := r.Response["error"].(string); ok && msg != "" {
			return msg
		}
	}
	return ""
}

// usageOf collects the UsageMetadata of every model call this specialist's
// run made, tagged with the node name (not the LLM's own author string — all
// three specialists' models may share a name, but the node that ran them
// does not).
func usageOf(node string, events []*session.Event) []Usage {
	var out []Usage
	for _, e := range events {
		if e == nil || e.LLMResponse.UsageMetadata == nil {
			continue
		}
		u := e.LLMResponse.UsageMetadata
		out = append(out, Usage{Author: node, Prompt: u.PromptTokenCount, Output: u.CandidatesTokenCount, Total: u.TotalTokenCount})
	}
	return out
}

// Merge zveds the JoinNode's map[nodeName]output into a Document in
// sectionOrder, computes its hash, and flattens every branch's token usage —
// also in sectionOrder, for the same reason (stable report, not map order).
func Merge(sections map[string]Section, usage map[string][]Usage) Document {
	doc := Document{}
	for _, name := range sectionOrder {
		if s, ok := sections[name]; ok {
			doc.Sections = append(doc.Sections, s)
		}
		doc.Usage = append(doc.Usage, usage[name]...)
	}
	doc.Hash = hashDocument(doc)
	return doc
}

// hashDocument hashes the verified content deterministically: same facts,
// same order, same hash — so Store.Spend's re-check at send time can tell a
// document that changed after approval from one that did not.
func hashDocument(doc Document) string {
	var b strings.Builder
	for _, s := range doc.Sections {
		fmt.Fprintf(&b, "%s|%s|", s.Node, s.Status)
		for _, f := range s.Findings {
			fmt.Fprintf(&b, "%s<-%s;", f.Claim, f.Source)
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
