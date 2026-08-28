package chat

import (
	"context"

	"github.com/joyqi/iota/internal/ui"
	"github.com/joyqi/iota/tool"
)

// Interactor is the chat-side tool.Interactor: it maps an AskSpec onto the
// tabbed surface engine — one tab per question (short header as the chip,
// Panel.Prompt as the body), wizard Enter (EnterAdvances), and the engine's
// inline "Other…" editor for custom answers (Panel.Custom). It is created
// UNBOUND in cmd/root (the dispatcher is built before the UI exists) and
// bound to the live UI at the top of chat.Run — tool calls only happen
// inside Run, so a bound UI is an invariant, not a race.
type Interactor struct {
	u *ui.UI
}

// NewInteractor returns an unbound Interactor for the dispatcher's Env.
func NewInteractor() *Interactor { return &Interactor{} }

// bind attaches the live UI (called once, before the first ReadInput).
func (it *Interactor) bind(u *ui.UI) { it.u = u }

// Ask implements tool.Interactor.
func (it *Interactor) Ask(ctx context.Context, spec tool.AskSpec) (tool.AskResult, error) {
	if it.u == nil {
		return tool.AskResult{Declined: true}, nil // unbound: non-interactive
	}
	panels := make([]ui.Panel, len(spec.Questions))
	for i, q := range spec.Questions {
		items := make([]string, 0, len(q.Options))
		for _, o := range q.Options {
			label := o.Label
			if o.Description != "" {
				label += "  " + DimStyle.Sprint(o.Description)
			}
			items = append(items, label)
		}
		kind := ui.PanelList
		if q.Multiple {
			kind = ui.PanelMulti
		}
		panels[i] = ui.Panel{Title: q.Header, Kind: kind, Items: items, Prompt: q.Question, Custom: q.AllowCustom}
	}
	r, err := it.u.Tabbed(ctx, ui.TabbedSpec{Panels: panels, EnterAdvances: true})
	if err != nil {
		return tool.AskResult{}, err
	}
	if r.Cancelled {
		return tool.AskResult{Declined: true}, nil
	}

	res := tool.AskResult{Answers: make([]tool.AskAnswer, len(spec.Questions))}
	for i, q := range spec.Questions {
		pr := r.Panels[i]
		otherIdx := len(q.Options)
		if q.Multiple {
			for _, c := range pr.Checked {
				switch {
				case q.AllowCustom && c == otherIdx:
					res.Answers[i].Custom = pr.Custom
				case c < len(q.Options):
					res.Answers[i].Selected = append(res.Answers[i].Selected, q.Options[c].Label)
				}
			}
			continue
		}
		if q.AllowCustom && pr.Cursor == otherIdx {
			res.Answers[i].Custom = pr.Custom
			continue
		}
		if pr.Cursor < len(q.Options) {
			res.Answers[i].Selected = []string{q.Options[pr.Cursor].Label}
		}
	}
	return res, nil
}
