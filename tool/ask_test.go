package tool

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fakeInteractor records the spec and plays back a scripted result.
type fakeInteractor struct {
	spec AskSpec
	res  AskResult
}

func (f *fakeInteractor) Ask(_ context.Context, spec AskSpec) (AskResult, error) {
	f.spec = spec
	return f.res, nil
}

func askTools(t *testing.T, it Interactor) (choose, confirm Tool) {
	t.Helper()
	tools, err := newAskSet(Env{Interact: it}, yaml.Node{})
	if err != nil || len(tools) != 2 {
		t.Fatalf("newAskSet: %d tools, %v", len(tools), err)
	}
	return tools[0], tools[1]
}

// Without an Interactor the set contributes NOTHING — the model never sees
// tools it cannot use (-m mode).
func TestAskSetAbsentWithoutInteractor(t *testing.T) {
	tools, err := newAskSet(Env{}, yaml.Node{})
	if err != nil || len(tools) != 0 {
		t.Fatalf("want no tools without an Interactor, got %d, %v", len(tools), err)
	}
}

func TestChooseParsesAndFormats(t *testing.T) {
	fi := &fakeInteractor{res: AskResult{Answers: []AskAnswer{
		{Selected: []string{"OAuth"}},
		{Selected: []string{"requests", "httpx"}},
		{Custom: "use k3s instead"},
	}}}
	choose, _ := askTools(t, fi)

	args := map[string]any{"questions": []any{
		map[string]any{
			"header": "Auth", "question": "Which auth?",
			"options":      []any{map[string]any{"label": "OAuth", "description": "standard"}, map[string]any{"label": "API key"}},
			"allow_custom": false,
		},
		map[string]any{
			"header": "Libraries", "question": "Which libraries?",
			"options":  []any{map[string]any{"label": "requests"}, map[string]any{"label": "httpx"}},
			"multiple": true,
		},
		map[string]any{
			"header": "Deploy", "question": "How to deploy?",
			"options": []any{map[string]any{"label": "k8s"}, map[string]any{"label": "VM"}},
		},
	}}
	text, isErr, err := choose.Call(context.Background(), args)
	if err != nil || isErr {
		t.Fatalf("Call: %v %v", isErr, err)
	}

	// The spec reached the interactor with defaults applied.
	if len(fi.spec.Questions) != 3 {
		t.Fatalf("spec questions = %d", len(fi.spec.Questions))
	}
	if fi.spec.Questions[0].AllowCustom || !fi.spec.Questions[2].AllowCustom {
		t.Error("allow_custom: explicit false and default true both required")
	}
	if !fi.spec.Questions[1].Multiple || fi.spec.Questions[0].Multiple {
		t.Error("multiple flag mangled")
	}
	if fi.spec.Questions[0].Options[0].Description != "standard" {
		t.Error("option description lost")
	}

	want := "Auth: OAuth\nLibraries: requests, httpx\nDeploy: use k3s instead (custom answer)"
	if text != want {
		t.Fatalf("result:\n%q\nwant:\n%q", text, want)
	}
}

func TestChooseValidation(t *testing.T) {
	choose, _ := askTools(t, &fakeInteractor{})
	for name, args := range map[string]map[string]any{
		"no questions":   {"questions": []any{}},
		"missing header": {"questions": []any{map[string]any{"question": "q", "options": []any{map[string]any{"label": "a"}}}}},
		"no options":     {"questions": []any{map[string]any{"header": "H", "question": "q", "options": []any{}}}},
		"five questions": {"questions": []any{
			map[string]any{"header": "1", "question": "q", "options": []any{map[string]any{"label": "a"}}},
			map[string]any{"header": "2", "question": "q", "options": []any{map[string]any{"label": "a"}}},
			map[string]any{"header": "3", "question": "q", "options": []any{map[string]any{"label": "a"}}},
			map[string]any{"header": "4", "question": "q", "options": []any{map[string]any{"label": "a"}}},
			map[string]any{"header": "5", "question": "q", "options": []any{map[string]any{"label": "a"}}},
		}},
	} {
		if _, isErr, _ := choose.Call(context.Background(), args); !isErr {
			t.Errorf("%s: want a tool error", name)
		}
	}
}

// A too-long header is truncated to a tab-chip length, not rejected.
func TestChooseHeaderTruncated(t *testing.T) {
	fi := &fakeInteractor{res: AskResult{Answers: []AskAnswer{{Selected: []string{"a"}}}}}
	choose, _ := askTools(t, fi)
	choose.Call(context.Background(), map[string]any{"questions": []any{
		map[string]any{"header": "an unreasonably long tab header", "question": "q",
			"options": []any{map[string]any{"label": "a"}, map[string]any{"label": "b"}}},
	}})
	if r := []rune(fi.spec.Questions[0].Header); len(r) > askHeaderMax {
		t.Fatalf("header not truncated: %q", fi.spec.Questions[0].Header)
	}
	if !strings.HasSuffix(fi.spec.Questions[0].Header, "…") {
		t.Fatalf("truncated header should end with …: %q", fi.spec.Questions[0].Header)
	}
}

func TestChooseDeclined(t *testing.T) {
	choose, _ := askTools(t, &fakeInteractor{res: AskResult{Declined: true}})
	text, isErr, err := choose.Call(context.Background(), map[string]any{"questions": []any{
		map[string]any{"header": "H", "question": "q",
			"options": []any{map[string]any{"label": "a"}, map[string]any{"label": "b"}}},
	}})
	if err != nil || isErr {
		t.Fatalf("declined must not be an error: %v %v", isErr, err)
	}
	if text != "The user declined to answer." {
		t.Fatalf("text = %q", text)
	}
}

func TestConfirm(t *testing.T) {
	fi := &fakeInteractor{res: AskResult{Answers: []AskAnswer{{Selected: []string{"Ship it"}}}}}
	_, confirm := askTools(t, fi)
	text, isErr, err := confirm.Call(context.Background(), map[string]any{
		"question": "Deploy now?", "yes_label": "Ship it", "no_label": "Hold",
	})
	if err != nil || isErr {
		t.Fatalf("Call: %v %v", isErr, err)
	}
	if text != "The user chose: Ship it" {
		t.Fatalf("text = %q", text)
	}
	q := fi.spec.Questions[0]
	if q.AllowCustom || q.Multiple || len(q.Options) != 2 || q.Options[1].Label != "Hold" {
		t.Fatalf("confirm spec wrong: %+v", q)
	}
}

// `ask: false` (and false for any set) is an explicit opt-out Build honors
// and SetDisabled reports for the default-enable path.
func TestSetFalseDisables(t *testing.T) {
	var raw struct {
		Tools map[string]yaml.Node `yaml:"tools"`
	}
	if err := yaml.Unmarshal([]byte("tools:\n  ask: false\n  shell:\n"), &raw); err != nil {
		t.Fatal(err)
	}
	if !SetDisabled(raw.Tools, "ask") {
		t.Error("ask: false must report disabled")
	}
	if SetDisabled(raw.Tools, "shell") || SetDisabled(raw.Tools, "code") {
		t.Error("present-empty and absent sets are not disabled")
	}
	r := Build(Env{Interact: &fakeInteractor{}}, raw.Tools, nil)
	for _, def := range r.Tools() {
		if def.Name == "choose" || def.Name == "confirm" {
			t.Errorf("disabled ask set still built tool %q", def.Name)
		}
	}
}
