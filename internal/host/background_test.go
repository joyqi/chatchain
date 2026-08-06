package host

import (
	"errors"
	"testing"
)

func TestParseCmuxBackground(t *testing.T) {
	cases := []struct {
		name     string
		json     string
		dark, ok bool
	}{
		{"light", `{"render_grid":{"terminal_background":"#FEFFFF"}}`, false, true},
		{"dark", `{"render_grid":{"terminal_background":"#1E1E1E"}}`, true, true},
		{"missing field", `{"render_grid":{}}`, false, false},
		{"garbage", `not json`, false, false},
		{"named color", `{"render_grid":{"terminal_background":"white"}}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dark, ok := parseCmuxBackground([]byte(tc.json))
			if dark != tc.dark || ok != tc.ok {
				t.Fatalf("= %v/%v, want %v/%v", dark, ok, tc.dark, tc.ok)
			}
		})
	}
}

func TestDarkHexBoundary(t *testing.T) {
	if dark, ok := darkHex("#808080"); !ok || dark {
		t.Fatalf("mid gray = %v/%v, want light", dark, ok)
	}
	if dark, ok := darkHex("#000000"); !ok || !dark {
		t.Fatalf("black = %v/%v, want dark", dark, ok)
	}
}

type bgHost struct{ dark, ok bool }

func (b bgHost) Name() string                 { return "bg" }
func (b bgHost) DarkBackground() (bool, bool) { return b.dark, b.ok }

type inertHost struct{}

func (inertHost) Name() string { return "inert" }

func TestPresenterDarkBackground(t *testing.T) {
	// The first host that KNOWS wins; hosts without the capability or
	// without an answer fall through; nobody knows → ok=false.
	p := &Presenter{hosts: []Host{inertHost{}, bgHost{ok: false}, bgHost{dark: true, ok: true}}}
	if dark, ok := p.DarkBackground(); !ok || !dark {
		t.Fatalf("= %v/%v, want dark/true", dark, ok)
	}
	p = &Presenter{hosts: []Host{inertHost{}}}
	if _, ok := p.DarkBackground(); ok {
		t.Fatal("inert-only presenter must not know its background")
	}
}

func TestDetectBackgroundCmuxProbe(t *testing.T) {
	old := cmuxQuery
	defer func() { cmuxQuery = old }()
	cmuxQuery = func(path, sid string) ([]byte, error) {
		if path != "/bin/cmux" || sid != "surf-1" {
			t.Errorf("query path=%q sid=%q", path, sid)
		}
		return []byte(`{"render_grid":{"terminal_background":"#FEFFFF"}}`), nil
	}
	env := Env{
		Getenv: func(k string) string {
			if k == "CMUX_SURFACE_ID" {
				return "surf-1"
			}
			return ""
		},
		LookPath: func(string) (string, error) { return "/bin/cmux", nil },
	}
	if dark := DetectBackground(env); dark {
		t.Fatal("cmux reported a light background; DetectBackground said dark")
	}
}

func TestDetectBackgroundFallsThroughToOSC(t *testing.T) {
	oldQ := queryTerminalDark
	defer func() { queryTerminalDark = oldQ }()
	queryTerminalDark = func() bool { return true }
	env := Env{
		Getenv:   func(string) string { return "" },
		LookPath: func(string) (string, error) { return "", errors.New("absent") },
	}
	if !DetectBackground(env) {
		t.Fatal("with no host probe, the OSC answer must stand")
	}
}

func TestDetectBackgroundLatch(t *testing.T) {
	oldQ, oldLatch := queryTerminalDark, bgUnsupported
	defer func() { queryTerminalDark, bgUnsupported = oldQ, oldLatch }()
	bgUnsupported = true
	queryTerminalDark = func() bool {
		t.Fatal("latched: the terminal must not be queried again")
		return false
	}
	env := Env{
		Getenv:   func(string) string { return "" },
		LookPath: func(string) (string, error) { return "", errors.New("absent") },
	}
	if !DetectBackground(env) {
		t.Fatal("latched default must be dark")
	}
}
