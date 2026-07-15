// Package agents implements the agent-mode workspace context
// (docs/design/agent-mode.md): AGENTS.md files and the Agent Skills catalog
// form a volatile system-prompt overlay, composed at send time and never
// stored in history. The chat layer drives the Overlay and renders its
// notices; the agent toolset's load_skill resolves skill names back to their
// files through the same skill discovery (skills.go).
package agents

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"chatchain/provider"
)

// agentsFileName is the exact file name looked up in each directory on the
// root→cwd path (per the Codex reference semantics, https://agents.md/).
const agentsFileName = "AGENTS.md"

// agentsChainCap bounds the joined chain; content past it is cut and marked
// with agentsTruncationMark so a runaway AGENTS.md cannot flood the context.
const agentsChainCap = 32 << 10 // 32 KiB

const agentsTruncationMark = "\n\n<!-- AGENTS.md chain truncated at 32 KiB -->"

// ProjectRoot returns the project root anchoring agent mode: the closest
// ancestor of cwd (inclusive) containing a .git entry — a directory in a
// normal checkout, a file in a linked worktree — falling back to cwd itself
// outside a repository.
func ProjectRoot(cwd string) string {
	for dir := cwd; ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return cwd
		}
		dir = parent
	}
}

// agentsChain is one assembled AGENTS.md overlay: the joined content plus the
// provenance the freshness probe and status lines need.
type agentsChain struct {
	Content string      // root-first concatenation, blank-line joined, capped
	Files   []string    // contributing AGENTS.md paths, root-first
	Stamps  []time.Time // per-file mtimes, parallel to Files
}

// agentsDirs lists the directories searched for AGENTS.md: the project root
// down to cwd, one entry per directory. A cwd outside the root degrades to
// the root alone.
func agentsDirs(root, cwd string) []string {
	dirs := []string{root}
	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return dirs
	}
	dir := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		dir = filepath.Join(dir, part)
		dirs = append(dirs, dir)
	}
	return dirs
}

// statAgentsChain probes the chain without reading it: which AGENTS.md files
// exist on the root→cwd path and the newest mtime among them. This is the
// per-turn freshness check — a handful of stats, no file reads.
func statAgentsChain(root, cwd string) ([]string, []time.Time) {
	var files []string
	var stamps []time.Time
	for _, dir := range agentsDirs(root, cwd) {
		path := filepath.Join(dir, agentsFileName)
		fi, err := os.Stat(path)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		files = append(files, path)
		// Per-file mtimes, not a collapsed newest: an mtime-preserving replace
		// (cp -p, rsync -t) of an older chain file must still be detected.
		stamps = append(stamps, fi.ModTime())
	}
	return files, stamps
}

// timesEqual compares two mtime slices with time.Equal (== on time.Time is
// unreliable across monotonic/location internals).
func timesEqual(a, b []time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

// loadAgentsChain assembles the AGENTS.md chain for root→cwd: at most one
// file per directory, concatenated root-first with blank-line joins (nearer
// files appear later and therefore override), total capped at agentsChainCap.
func loadAgentsChain(root, cwd string) agentsChain {
	files, stamps := statAgentsChain(root, cwd)
	parts := make([]string, 0, len(files))
	for _, path := range files {
		// Never read more than the chain cap from a single file: the cap must
		// bound memory BEFORE the read, or a pathological multi-gigabyte
		// AGENTS.md in a cloned repo would be loaded wholesale.
		data, err := readFileCapped(path, agentsChainCap+1)
		if err != nil {
			continue
		}
		parts = append(parts, strings.TrimRight(string(data), "\n"))
	}
	content := strings.Join(parts, "\n\n")
	if len(content) > agentsChainCap {
		content = truncateAtRune(content, agentsChainCap) + agentsTruncationMark
	}
	return agentsChain{Content: content, Files: files, Stamps: stamps}
}

// readFileCapped reads at most max bytes of a file.
func readFileCapped(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, int64(max)))
}

// truncateAtRune cuts s at max bytes, backing up to a rune boundary so the
// cut never splits a UTF-8 sequence.
func truncateAtRune(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Overlay manages the volatile overlay for one chat session: it caches the
// assembled AGENTS.md chain and the discovered skills, re-reading either only
// when its freshness probe shows the file set or newest mtime changed, so
// unchanged turns reuse the byte-identical composition (keeping provider
// prompt caches warm). Nil-safe: a nil overlay (agent mode off) is inert.
type Overlay struct {
	root, cwd string
	chain     agentsChain

	// Skills state. skillDirs is fixed at construction (precedence high→low);
	// skillPaths/skillStamps mirror agentsChain's Files/Stamps for the per-turn
	// freshness check.
	skillDirs     []string
	skills        []Skill
	skillWarnings []string
	skillPaths    []string
	skillStamps   []time.Time
}

// NewOverlay assembles the initial overlay for root/cwd.
func NewOverlay(root, cwd string) *Overlay {
	return newOverlayDirs(root, cwd, SkillRoots(root))
}

// newOverlayDirs is NewOverlay with explicit skill discovery directories, so
// tests inject temp dirs and never scan the real home.
func newOverlayDirs(root, cwd string, skillDirs []string) *Overlay {
	o := &Overlay{root: root, cwd: cwd, skillDirs: skillDirs, chain: loadAgentsChain(root, cwd)}
	o.reloadSkills()
	return o
}

// reloadSkills re-runs skill discovery and records the freshness probe taken
// over the result (the probe covers the discovered SKILL.md files, so it must
// be taken after discovery).
func (o *Overlay) reloadSkills() {
	o.skills, o.skillWarnings = DiscoverSkills(o.skillDirs)
	o.skillPaths, o.skillStamps = probeSkills(o.skillDirs, o.skills)
}

// Refresh probes the AGENTS.md chain's and the skills roots' freshness and
// rebuilds whichever changed. The two changes are reported separately so the
// caller can print the matching reload notice.
func (o *Overlay) Refresh() (agentsChanged, skillsChanged bool) {
	if o == nil {
		return false, false
	}
	if files, stamps := statAgentsChain(o.root, o.cwd); !slices.Equal(files, o.chain.Files) || !timesEqual(stamps, o.chain.Stamps) {
		o.chain = loadAgentsChain(o.root, o.cwd)
		agentsChanged = true
	}
	if paths, stamps := probeSkills(o.skillDirs, o.skills); !slices.Equal(paths, o.skillPaths) || !timesEqual(stamps, o.skillStamps) {
		o.reloadSkills()
		skillsChanged = true
	}
	return agentsChanged, skillsChanged
}

// Content returns the current overlay text — the AGENTS.md chain joined with
// the skills catalog — or "" when agent mode is off and neither exists.
func (o *Overlay) Content() string {
	if o == nil {
		return ""
	}
	catalog := skillsCatalog(o.skills)
	switch {
	case o.chain.Content == "":
		return catalog
	case catalog == "":
		return o.chain.Content
	}
	return o.chain.Content + "\n\n" + catalog
}

// FileCount returns how many AGENTS.md files feed the overlay.
func (o *Overlay) FileCount() int {
	if o == nil {
		return 0
	}
	return len(o.chain.Files)
}

// ChainSize returns the byte size of the assembled AGENTS.md chain alone (for
// the startup banner; Content may additionally carry the skills catalog).
func (o *Overlay) ChainSize() int {
	if o == nil {
		return 0
	}
	return len(o.chain.Content)
}

// SkillCount returns how many skills the catalog advertises.
func (o *Overlay) SkillCount() int {
	if o == nil {
		return 0
	}
	return len(o.skills)
}

// Skills returns the discovered skills (nil-safe copy) for the /skills view.
func (o *Overlay) Skills() []Skill {
	if o == nil {
		return nil
	}
	return append([]Skill(nil), o.skills...)
}

// Warnings returns the invalid-skill warnings from the latest discovery.
func (o *Overlay) Warnings() []string {
	if o == nil {
		return nil
	}
	return o.skillWarnings
}

// ComposeSendHistory returns the messages to send for one turn: history itself
// when the overlay is empty (agent mode off keeps the exact same slice),
// otherwise a shallow copy whose system message has the overlay appended — or
// a synthetic system message prepended when the user has none. history is
// never mutated, so persistence, compaction, and /export keep seeing the
// clean conversation.
func ComposeSendHistory(history []provider.Message, overlay string) []provider.Message {
	if overlay == "" {
		return history
	}
	if len(history) > 0 && history[0].Role == "system" {
		out := make([]provider.Message, len(history))
		copy(out, history)
		out[0].Content += "\n\n" + overlay
		return out
	}
	out := make([]provider.Message, 0, len(history)+1)
	out = append(out, provider.Message{Role: "system", Content: overlay})
	return append(out, history...)
}
