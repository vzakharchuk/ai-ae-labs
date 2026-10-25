// workflow.go — AgentWorkflow: той самий агент, що в кроці 0, але його цикл
// тепер Temporal Workflow (п.3, 20 балів). Жодного I/O тут напряму — кожен
// LLM-виклик (googleadk.NewModel) і кожен виклик open_refund_case/
// search_nodes/open_nodes — Activity (activities.go, tools.go). Сесія живе
// в session.InMemoryService() усередині workflow; стійкість їй дає історія
// подій Temporal, а SQLite/Dgraph — Activity LoadSession/PersistSession/
// RememberSession довкола цього ходу.
package main

import (
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/contrib/googleadk"
	"go.temporal.io/sdk/workflow"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// Request — вхід AgentWorkflow: усе, що потрібно відомо ДО старту ходу.
// Model — ім'я, не model.LLM: лише ім'я перетинає межу workflow-входу,
// справжню модель worker реконструює через googleadk.Config.Models
// (app.go), так само як errType, а не Go-тип, перетинає межу Activity.
type Request struct {
	UserID    string
	SessionID string
	Message   string
	Model     string
}

// AgentWorkflow — ПУНКТ 3.
func AgentWorkflow(ctx workflow.Context, req Request) (string, error) {
	ao := workflow.ActivityOptions{StartToCloseTimeout: time.Minute}
	actx := workflow.WithActivityOptions(ctx, ao)

	var snap *googleadk.SessionSnapshot
	if err := workflow.ExecuteActivity(actx, ActivityLoadSession, req.UserID, req.SessionID).Get(ctx, &snap); err != nil {
		return "", fmt.Errorf("load session: %w", err)
	}

	svc := session.InMemoryService()
	wfCtx := googleadk.NewContext(ctx)

	if snap != nil {
		if _, err := googleadk.ImportSession(wfCtx, svc, snap); err != nil {
			return "", fmt.Errorf("import session: %w", err)
		}
	} else if _, err := svc.Create(wfCtx, &session.CreateRequest{AppName: appName, UserID: req.UserID, SessionID: req.SessionID}); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}

	b := &budget{}
	refund, err := functiontool.New(functiontool.Config{
		Name:        "open_refund_case",
		Description: "Opens a refund case for one transaction of one merchant. Needs transaction_id and merchant_id exactly as the user wrote them. May return status invalid_argument with an observation: read it, fix the named field, call again.",
	}, openRefundCaseDurable(b))
	if err != nil {
		return "", err
	}
	payouts, err := functiontool.New(functiontool.Config{
		Name:        "get_merchant_payouts",
		Description: "Reads one merchant's recent payouts.",
	}, getMerchantPayouts)
	if err != nil {
		return "", err
	}
	search, err := functiontool.New(functiontool.Config{
		Name:        "search_nodes",
		Description: "Searches earlier sessions by keyword and returns what is remembered.",
	}, searchNodesDurable())
	if err != nil {
		return "", err
	}
	open, err := functiontool.New(functiontool.Config{
		Name:        "open_nodes",
		Description: "Returns everything remembered about the named ids (merchants, transactions, tickets).",
	}, openNodesDurable())
	if err != nil {
		return "", err
	}

	root, err := llmagent.New(llmagent.Config{
		Name: "refund_agent", Description: "Opens refund cases for LEDGERWORKS.",
		Model: googleadk.NewModel(req.Model), Instruction: instruction,
		Tools: []tool.Tool{refund, payouts, search, open},
	})
	if err != nil {
		return "", err
	}
	r, err := runner.New(runner.Config{AppName: appName, Agent: root, SessionService: svc})
	if err != nil {
		return "", err
	}

	msg := genai.NewContentFromText(req.Message, genai.RoleUser)
	var final string
	for ev, runErr := range r.Run(wfCtx, req.UserID, req.SessionID, msg, agent.RunConfig{}) {
		if runErr != nil {
			return "", fmt.Errorf("run: %w", runErr)
		}
		if t := answerText(ev); t != "" {
			final = t
		}
	}

	newSnap, err := googleadk.ExportSession(wfCtx, svc, appName, req.UserID, req.SessionID)
	if err != nil {
		return "", fmt.Errorf("export session: %w", err)
	}
	if err := workflow.ExecuteActivity(actx, ActivityPersistSession, newSnap).Get(ctx, nil); err != nil {
		return "", fmt.Errorf("persist session: %w", err)
	}
	if err := workflow.ExecuteActivity(actx, ActivityRememberSession, newSnap).Get(ctx, nil); err != nil {
		return "", fmt.Errorf("remember session: %w", err)
	}

	return final, nil
}

// answerText reads the plain-text answer out of one runner event, the way
// printEvent (main.go) does for the step-0 CLI, minus the tool-call/
// observation logging the workflow does not need.
func answerText(ev *session.Event) string {
	if ev == nil || ev.Content == nil {
		return ""
	}
	for _, p := range ev.Content.Parts {
		if p != nil && strings.TrimSpace(p.Text) != "" {
			return strings.TrimSpace(p.Text)
		}
	}
	return ""
}
