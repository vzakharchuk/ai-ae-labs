package main

import (
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/workflow"

	"github.com/dimetron/ai-eng-course/labs/week2/internal/refund"
)

// invocationIDContext overrides the one method FunctionNode.Run calls on a
// success path (InvocationID, to stamp the resulting session.Event) —
// agent.StrictContextMock panics on it otherwise, by design (see
// refund_test.go's toolContext for the same pattern). The rejected-input
// case below needs no override: validation fails before Run gets there.
type invocationIDContext struct {
	agent.StrictContextMock
}

func (c *invocationIDContext) InvocationID() string { return "test-invocation" }

// newFormatNodeForTest builds the exact node newGraph wires in as "format" —
// same refundOutputSchema(), same refund.Format — so this test exercises the
// real production schema, not a copy that could drift from it.
func newFormatNodeForTest(t *testing.T) *workflow.FunctionNode {
	t.Helper()
	schema, err := refundOutputSchema()
	if err != nil {
		t.Fatalf("refundOutputSchema: %v", err)
	}
	node, err := workflow.NewFunctionNodeWithSchema[refund.Output, string]("format", refund.Format, schema, nil, workflow.NodeConfig{})
	if err != nil {
		t.Fatalf("create format node: %v", err)
	}
	return node
}

// TestFormatNodeSchemaRejectsUnknownStatusBeforeRunning is the ++ Advanced
// proof: Status is a plain Go string, so "approved" would compile and reach
// refund.Format just fine with a schema-less node — and in fact
// refund.Format has its OWN internal check for exactly this ("incomplete or
// invalid refund result"). The point of this test is telling the two apart:
// the error here must come from schema validation, one layer BEFORE
// refund.Format's body ever runs, not from Format's own check. The two
// error strings are deliberately different (see refund.go), so a match on
// "validation failed" — the schema-layer wrapper's own wording — proves
// which one fired.
func TestFormatNodeSchemaRejectsUnknownStatusBeforeRunning(t *testing.T) {
	node := newFormatNodeForTest(t)
	ctx := agent.NewStrictContextMock(t.Context())
	bad := refund.Output{
		CaseID:        "rc-txn-2026-07-118845-A-114",
		TransactionID: "txn-2026-07-118845",
		MerchantID:    "A-114",
		Status:        "approved", // not in the schema's enum, and not a value Format accepts either
	}

	var gotErr error
	for _, runErr := range node.Run(&ctx, bad) {
		if runErr != nil {
			gotErr = runErr
		}
	}

	if gotErr == nil {
		t.Fatal(`expected a schema validation error for status="approved", got none`)
	}
	if !strings.Contains(gotErr.Error(), "validation failed") {
		t.Fatalf("error = %q, want it to name schema validation (format %T's own check reads differently)", gotErr, refund.Format)
	}
}

// TestFormatNodeSchemaAcceptsKnownStatus is the control: a Status the enum
// allows passes validation, reaches refund.Format, and produces the normal
// formatted answer — the schema adds a gate, it does not change behaviour
// for input that was always valid.
func TestFormatNodeSchemaAcceptsKnownStatus(t *testing.T) {
	node := newFormatNodeForTest(t)
	ctx := &invocationIDContext{StrictContextMock: agent.NewStrictContextMock(t.Context())}
	good := refund.Output{
		CaseID:        "rc-txn-2026-07-118845-A-114",
		TransactionID: "txn-2026-07-118845",
		MerchantID:    "A-114",
		Status:        "pending",
	}

	var gotOutput string
	var gotErr error
	for ev, runErr := range node.Run(ctx, good) {
		if runErr != nil {
			gotErr = runErr
			continue
		}
		if s, ok := ev.Output.(string); ok {
			gotOutput = s
		}
	}

	if gotErr != nil {
		t.Fatalf("unexpected error: %v", gotErr)
	}
	const want = "Кейс rc-txn-2026-07-118845-A-114: транзакція txn-2026-07-118845, мерчант A-114, статус pending"
	if gotOutput != want {
		t.Fatalf("output = %q, want %q", gotOutput, want)
	}
}
