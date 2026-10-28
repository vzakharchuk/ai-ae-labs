// specialists.go — три джерела факту команди LEDGERWORKS (Homework п.1).
//
// Чому три спеціалісти, а не один: кожен дивиться у СВОЄ джерело (incident-db,
// PCI DSS/НБУ-комплаєнс, CRM інтеграцій) з різним форматом даних і різними
// режимами збою (incident-db стабільна, CRM вміє мовчати). Один агент з трьома
// інструментами міг би те саме технічно, але тоді контекст одного джерела
// протікав би в запит до іншого (підказка для компліансу могла б підмішатись
// у запит до CRM) і один збій (integration_desk мовчить) обвалював би весь
// контекст розмови, а не лише свій розділ. Три окремі під-гілки (fan-out,
// engine.go) ізолюють і контекст, і збій: правильний компроміс, коли людина
// читатиме підсумок, а не стенограму — для чернетки "на коліні" одного
// агента з трьома інструментами було б досить.
package main

import (
	"errors"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// Finding — один факт із ідентифікатором джерела. Спеціаліст має право
// говорити лише те, що повернув інструмент.
type Finding struct {
	Claim  string `json:"claim"`
	Source string `json:"source"`
}

const sourcedFactsInstruction = `Виклич інструмент ПЕРШИМ, перш ніж щось відповідати.
Відповідай лише фактами з інструмента, кожен факт одним рядком, джерело в дужках.
Нічого не додавай від себе й нічого не вигадуй, якщо інструмент не повернув факту.`

// --- risk_desk ---------------------------------------------------------------

type riskInput struct {
	Topic string `json:"topic" jsonschema:"Тема, за якою потрібні факти"`
}

type riskOutput struct {
	Findings []Finding `json:"findings"`
}

// lookupRisk — фікстура бази інцидентів LEDGERWORKS. Справжня система
// ходила б у incident-db; форма (факт + джерело) та сама.
func lookupRisk(_ agent.Context, _ riskInput) (riskOutput, error) {
	return riskOutput{Findings: []Finding{
		{Claim: "Cutover 2025-03 відкотили за 41 хв через розсинхрон токенів", Source: "incident-db:INC-418"},
		{Claim: "Rollback-ранбук для A-114 перевірено востаннє у вересні", Source: "runbook:cutover-rollback"},
	}}, nil
}

// --- compliance_desk -----------------------------------------------------

type complianceInput struct {
	Topic string `json:"topic" jsonschema:"Тема, за якою потрібні факти"`
}

type complianceOutput struct {
	Findings []Finding `json:"findings"`
}

// lookupCompliance — фікстура бази вимог PCI DSS / НБУ.
func lookupCompliance(_ agent.Context, _ complianceInput) (complianceOutput, error) {
	return complianceOutput{Findings: []Finding{
		{Claim: "PCI DSS 4.0 вимагає сегментації мережі до переходу на новий процесор", Source: "pci-dss:req-1.3"},
		{Claim: "Вимога НБУ 2026 по тарифу T-2 набирає чинності до 01.04.2026", Source: "nbu:2026-req"},
	}}, nil
}

// --- integration_desk ------------------------------------------------------

type integrationInput struct {
	Topic string `json:"topic" jsonschema:"Тема, за якою потрібні факти"`
}

type integrationOutput struct {
	Findings []Finding `json:"findings"`
}

// ErrIntegrationDeskDown — симульований збій CRM-джерела (Homework п.6). Без
// префікса "integration_desk:" — Section.Node (document.go) уже називає
// розділ, і HasUnverified() склеює їх сам; подвійний префікс лише плутав би
// лог.
var ErrIntegrationDeskDown = errors.New("CRM не відповіла (CRM_TIMEOUT)")

// integrationFailing — пакетний перемикач для демонстрації стримування збою
// (`go run . -fail-integration ...`). Поза тестами лишається false: команда
// за замовчуванням працює на трьох живих джерелах, збій — окрема, навмисна
// демонстрація, не постійний стан.
var integrationFailing = false

// lookupIntegration — фікстура CRM-даних про мерчантів на legacy-інтеграції.
func lookupIntegration(_ agent.Context, _ integrationInput) (integrationOutput, error) {
	if integrationFailing {
		return integrationOutput{}, ErrIntegrationDeskDown
	}
	return integrationOutput{Findings: []Finding{
		{Claim: "A-114, A-207 лишаються на legacy-інтеграції понад 90 днів", Source: "crm:legacy-report-2026-10"},
		{Claim: "Контакт для координації міграції: integration-desk@ledgerworks.ua", Source: "crm:contact-book"},
	}}, nil
}

// --- спільна збірка ----------------------------------------------------------

// ModelFor resolves the model ONE specialist should use. With a real
// provider every specialist shares the same model.LLM (stateless, no
// conflict). Offline (fakellm), each specialist needs its OWN scripted
// instance naming ITS OWN tool — sharing one fakellm across three
// specialists would have compliance_desk and integration_desk try to call
// "lookup_risk_desk" too, since a scripted model cannot be told at runtime
// which agent is asking. toolName/args/text describe the one scripted turn
// that specialist's offline fallback plays.
type ModelFor func(toolName string, args map[string]any, text string) model.LLM

// newSpecialist будує одного llmagent з одним functiontool. Generics на fn
// виводять тип аргументу/результату — той самий виклик, що робить
// functiontool.New для одного інструмента в стартері, лише без повторення
// для кожного спеціаліста окремо.
func newSpecialist[In, Out any](provider ModelFor, name, toolName, toolDesc, desc string, offlineArgs map[string]any, offlineText string, fn func(agent.Context, In) (Out, error)) (agent.Agent, error) {
	t, err := functiontool.New(functiontool.Config{Name: toolName, Description: toolDesc}, fn)
	if err != nil {
		return nil, err
	}
	return llmagent.New(llmagent.Config{
		Name: name, Model: provider(toolName, offlineArgs, offlineText), Description: desc,
		Instruction: sourcedFactsInstruction,
		Tools:       []tool.Tool{t},
	})
}

func newRiskDesk(provider ModelFor) (agent.Agent, error) {
	return newSpecialist(provider, "risk_desk_agent", "lookup_risk_desk",
		"Факти про ризики міграції з бази інцидентів.",
		"Спеціаліст з ризиків міграції.",
		map[string]any{"topic": "міграція"}, "Cutover 2025-03 відкотили за 41 хв (incident-db:INC-418).",
		lookupRisk)
}

func newComplianceDesk(provider ModelFor) (agent.Agent, error) {
	return newSpecialist(provider, "compliance_desk_agent", "lookup_compliance_desk",
		"Факти про вимоги PCI DSS / НБУ.",
		"Спеціаліст з комплаєнсу PCI DSS та вимог НБУ.",
		map[string]any{"topic": "міграція"}, "PCI DSS 4.0 вимагає сегментації мережі (pci-dss:req-1.3).",
		lookupCompliance)
}

func newIntegrationDesk(provider ModelFor) (agent.Agent, error) {
	return newSpecialist(provider, "integration_desk_agent", "lookup_integration_desk",
		"Факти про мерчантів на legacy-інтеграції з CRM.",
		"Спеціаліст з інтеграцій мерчантів.",
		map[string]any{"topic": "міграція"}, "A-114, A-207 лишаються на legacy понад 90 днів (crm:legacy-report-2026-10).",
		lookupIntegration)
}
