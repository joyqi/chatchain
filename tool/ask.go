package tool

import (
	"context"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"chatchain/provider"
)

// The ask set: model-initiated user interaction — `choose` (1–4 single/multi
// select questions, optional custom input) and `confirm` (one yes/no). Pure
// UX, no side effects, no approval gate; enabled by default in interactive
// sessions (opt out with `ask: false`). Without an Interactor (non-interactive
// runs) the factory returns no tools, so the model never sees them.
func newAskSet(env Env, _ yaml.Node) ([]Tool, error) {
	if env.Interact == nil {
		return nil, nil
	}
	return []Tool{&chooseTool{it: env.Interact}, &confirmTool{it: env.Interact}}, nil
}

const (
	askMaxQuestions = 4
	askHeaderMax    = 16 // runes; headers are tab chips, not sentences
)

type chooseTool struct{ it Interactor }

func (t *chooseTool) Def() provider.ToolDef {
	return provider.ToolDef{
		Name: "choose",
		Description: "Ask the user to pick between options on an interactive selector. " +
			"Use ONLY when you are blocked on a decision that is genuinely the user's to make " +
			"and the choices are enumerable; for open-ended discussion just ask in text. " +
			"Each question's `header` is a TAB LABEL — keep it under ~12 characters. " +
			"Unless allow_custom is false the user can always answer with their own text instead. " +
			"The user may also decline to answer; proceed sensibly when that happens.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"questions": map[string]any{
					"type":     "array",
					"minItems": 1,
					"maxItems": askMaxQuestions,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"header": map[string]any{
								"type":        "string",
								"description": "Very short tab label (max ~12 chars), e.g. \"Auth\", \"Library\"",
							},
							"question": map[string]any{
								"type":        "string",
								"description": "The complete question, one line",
							},
							"options": map[string]any{
								"type":     "array",
								"minItems": 2,
								"items": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"label":       map[string]any{"type": "string"},
										"description": map[string]any{"type": "string"},
									},
									"required": []string{"label"},
								},
							},
							"multiple": map[string]any{
								"type":        "boolean",
								"description": "Allow selecting several options (default false)",
							},
							"allow_custom": map[string]any{
								"type":        "boolean",
								"description": "Offer an \"Other…\" free-text answer (default true)",
							},
						},
						"required": []string{"header", "question", "options"},
					},
				},
			},
			"required": []string{"questions"},
		},
	}
}

func (t *chooseTool) Call(ctx context.Context, args map[string]any) (string, bool, error) {
	spec, err := parseChooseArgs(args)
	if err != nil {
		return err.Error(), true, nil
	}
	res, err := t.it.Ask(ctx, spec)
	if err != nil {
		return "", false, err
	}
	if res.Declined {
		return "The user declined to answer.", false, nil
	}
	var b strings.Builder
	for i, q := range spec.Questions {
		a := res.Answers[i]
		b.WriteString(q.Header)
		b.WriteString(": ")
		switch {
		case a.Custom != "":
			b.WriteString(a.Custom)
			b.WriteString(" (custom answer)")
		case len(a.Selected) > 0:
			b.WriteString(strings.Join(a.Selected, ", "))
		default:
			b.WriteString("(nothing selected)")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

// parseChooseArgs validates the model's arguments into an AskSpec.
func parseChooseArgs(args map[string]any) (AskSpec, error) {
	raw, _ := args["questions"].([]any)
	if len(raw) == 0 {
		return AskSpec{}, fmt.Errorf("choose: questions must be a non-empty array")
	}
	if len(raw) > askMaxQuestions {
		return AskSpec{}, fmt.Errorf("choose: at most %d questions per call", askMaxQuestions)
	}
	spec := AskSpec{Questions: make([]AskQuestion, 0, len(raw))}
	for i, rq := range raw {
		m, _ := rq.(map[string]any)
		if m == nil {
			return AskSpec{}, fmt.Errorf("choose: questions[%d] must be an object", i)
		}
		header, _ := m["header"].(string)
		question, _ := m["question"].(string)
		if strings.TrimSpace(header) == "" || strings.TrimSpace(question) == "" {
			return AskSpec{}, fmt.Errorf("choose: questions[%d] needs header and question", i)
		}
		if r := []rune(header); len(r) > askHeaderMax {
			header = string(r[:askHeaderMax-1]) + "…"
		}
		q := AskQuestion{
			Header:      strings.TrimSpace(header),
			Question:    strings.TrimSpace(question),
			Multiple:    boolArg(m, "multiple", false),
			AllowCustom: boolArg(m, "allow_custom", true),
		}
		opts, _ := m["options"].([]any)
		for _, ro := range opts {
			om, _ := ro.(map[string]any)
			if om == nil {
				continue
			}
			label, _ := om["label"].(string)
			if strings.TrimSpace(label) == "" {
				continue
			}
			desc, _ := om["description"].(string)
			q.Options = append(q.Options, AskOption{Label: strings.TrimSpace(label), Description: strings.TrimSpace(desc)})
		}
		if len(q.Options) == 0 {
			return AskSpec{}, fmt.Errorf("choose: questions[%d] needs at least one option with a label", i)
		}
		spec.Questions = append(spec.Questions, q)
	}
	return spec, nil
}

func boolArg(m map[string]any, key string, def bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return def
}

type confirmTool struct{ it Interactor }

func (t *confirmTool) Def() provider.ToolDef {
	return provider.ToolDef{
		Name: "confirm",
		Description: "Ask the user a single yes/no question on an interactive prompt. " +
			"Use ONLY for a decision that is genuinely the user's to make (e.g. consent " +
			"before something hard to reverse). The user may decline to answer.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{
					"type":        "string",
					"description": "The complete yes/no question, one line",
				},
				"yes_label": map[string]any{
					"type":        "string",
					"description": "Label for the affirmative choice (default \"Yes\")",
				},
				"no_label": map[string]any{
					"type":        "string",
					"description": "Label for the negative choice (default \"No\")",
				},
			},
			"required": []string{"question"},
		},
	}
}

func (t *confirmTool) Call(ctx context.Context, args map[string]any) (string, bool, error) {
	question, _ := args["question"].(string)
	if strings.TrimSpace(question) == "" {
		return "confirm: question is required", true, nil
	}
	yes, _ := args["yes_label"].(string)
	if strings.TrimSpace(yes) == "" {
		yes = "Yes"
	}
	no, _ := args["no_label"].(string)
	if strings.TrimSpace(no) == "" {
		no = "No"
	}
	res, err := t.it.Ask(ctx, AskSpec{Questions: []AskQuestion{{
		Header:   "Confirm",
		Question: strings.TrimSpace(question),
		Options:  []AskOption{{Label: yes}, {Label: no}},
	}}})
	if err != nil {
		return "", false, err
	}
	if res.Declined {
		return "The user declined to answer.", false, nil
	}
	sel := ""
	if len(res.Answers) > 0 && len(res.Answers[0].Selected) > 0 {
		sel = res.Answers[0].Selected[0]
	}
	return "The user chose: " + sel, false, nil
}
