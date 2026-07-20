package chat

import (
	"context"
	"strings"

	"chatchain/internal/ui"
	"chatchain/tool"
)

// Interactor is the chat-side tool.Interactor: it maps an AskSpec onto the
// tabbed surface engine (one tab per question, Enter commits all, ESC
// declines) and follows up with a text-input surface for every question the
// user answered with "Other…". It is created UNBOUND in cmd/root (the
// dispatcher is built before the UI exists) and bound to the live UI at the
// top of chat.Run — tool calls only happen inside Run, so a bound UI is an
// invariant, not a race.
type Interactor struct {
	u *ui.UI
}

// NewInteractor returns an unbound Interactor for the dispatcher's Env.
func NewInteractor() *Interactor { return &Interactor{} }

// bind attaches the live UI (called once, before the first ReadInput).
func (it *Interactor) bind(u *ui.UI) { it.u = u }

const askOtherItem = "Other…"

// Ask implements tool.Interactor.
func (it *Interactor) Ask(ctx context.Context, spec tool.AskSpec) (tool.AskResult, error) {
	if it.u == nil {
		return tool.AskResult{Declined: true}, nil // unbound: non-interactive
	}
	panels := make([]ui.Panel, len(spec.Questions))
	for i, q := range spec.Questions {
		items := make([]string, 0, len(q.Options)+1)
		for _, o := range q.Options {
			label := o.Label
			if o.Description != "" {
				label += "  " + DimStyle.Sprint(o.Description)
			}
			items = append(items, label)
		}
		if q.AllowCustom {
			items = append(items, askOtherItem)
		}
		kind := ui.PanelList
		if q.Multiple {
			kind = ui.PanelMulti
		}
		panels[i] = ui.Panel{Title: q.Header, Kind: kind, Items: items, Prompt: q.Question}
	}
	r, err := it.u.Tabbed(ctx, ui.TabbedSpec{Panels: panels})
	if err != nil {
		return tool.AskResult{}, err
	}
	if r.Cancelled {
		return tool.AskResult{Declined: true}, nil
	}

	res := tool.AskResult{Answers: make([]tool.AskAnswer, len(spec.Questions))}
	var customQs []int // question indices answered with "Other…"
	for i, q := range spec.Questions {
		pr := r.Panels[i]
		otherIdx := len(q.Options)
		if q.Multiple {
			for _, c := range pr.Checked {
				if q.AllowCustom && c == otherIdx {
					customQs = append(customQs, i)
					continue
				}
				if c < len(q.Options) {
					res.Answers[i].Selected = append(res.Answers[i].Selected, q.Options[c].Label)
				}
			}
			continue
		}
		if q.AllowCustom && pr.Cursor == otherIdx {
			customQs = append(customQs, i)
			continue
		}
		if pr.Cursor < len(q.Options) {
			res.Answers[i].Selected = []string{q.Options[pr.Cursor].Label}
		}
	}

	// Phase 2: one input tab per custom answer. Cancelling here declines the
	// whole ask — a half-answered spec would be more confusing than a retry.
	if len(customQs) > 0 {
		inputs := make([]ui.Panel, len(customQs))
		for j, qi := range customQs {
			inputs[j] = ui.Panel{
				Title:       spec.Questions[qi].Header,
				Kind:        ui.PanelInput,
				Prompt:      spec.Questions[qi].Question,
				Placeholder: "your answer",
				InputWidth:  60,
			}
		}
		r2, err := it.u.Tabbed(ctx, ui.TabbedSpec{Panels: inputs})
		if err != nil {
			return tool.AskResult{}, err
		}
		if r2.Cancelled {
			return tool.AskResult{Declined: true}, nil
		}
		for j, qi := range customQs {
			res.Answers[qi].Custom = strings.TrimSpace(r2.Panels[j].Text)
		}
	}
	return res, nil
}
