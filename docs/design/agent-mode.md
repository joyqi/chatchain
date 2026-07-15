# Agent mode (P1) — AGENTS.md, skills, load_skill, project sessions

## Switch

Agent mode is **explicitly opt-in**: `--agent` flag or per-provider config
`agent: true` (YAML truthy: `on`/`true`/`yes`). Off means byte-for-byte today's
behavior — nothing below activates.

The **project root** anchors everything: the git root of the working directory,
falling back to the cwd itself outside a repository.

## AGENTS.md — a volatile system-prompt overlay

Discovery follows the Codex reference semantics (the de-facto standard,
https://agents.md/): walk from the project root **down** to the cwd, at most
one `AGENTS.md` per directory, concatenated root-first with blank-line joins
(nearer files appear later and therefore override), total capped at **32 KiB**.

Injection is a **volatile overlay**, never baked into history:

- The user's own system prompt (`-s`/config `system`) stays in `history[0]`
  and is persisted exactly as today.
- At each send, `composeSystem()` builds the outgoing system content =
  user system prompt + AGENTS.md chain + skills catalog (below). The overlay
  is applied to a **send-time copy** of the history; the in-memory/persisted
  history never contains it.
- Freshness: stat the chain's per-file mtimes once per REPL turn (per-file,
  not a collapsed newest — an mtime-preserving replace of an older chain file
  is still detected); unchanged mtimes
  reuse the previous composition (byte-identical prefix keeps provider prompt
  caches warm), changes re-read and print a dim `AGENTS.md reloaded` notice.
  Tool-loop rounds within a turn use that turn's snapshot.
- Because nothing is persisted, resuming a session in another directory
  applies **that** directory's AGENTS.md — the correct ambient semantics.
- Local token estimates do not count the overlay (provider-reported usage
  does); accepted imprecision, documented here.

Chain files are read through a per-file cap (`readFileCapped`) so the 32 KiB
bound applies before any bytes load — a pathological multi-gigabyte AGENTS.md
never enters memory. Non-interactive `-m` runs compose the overlay once for
their single send (same semantics, no freshness loop).

## Skills — discovery, catalog, activation

Per the Agent Skills spec (https://agentskills.io/specification):

- **Discovery dirs, precedence high→low** (same-name skill: higher wins):
  1. `<project root>/.agents/skills/` (project)
  2. `~/.chatchain/skills/` (chatchain-native user dir)
  3. `~/.agents/skills/` (cross-client user dir)
- A skill = `<dir>/SKILL.md` with YAML frontmatter; required `name` (1–64
  chars, `[a-z0-9-]`, no leading/trailing/double hyphen, must equal the
  directory name) and `description` (1–1024 chars). Invalid skills are
  skipped with a startup warning, never fatal.
- **Level 1**: names + descriptions render into a catalog block inside the
  volatile overlay (modelled on `skills-ref to-prompt`), with an instruction
  that a skill is used by calling the `load_skill` tool with its name (paths
  stay encapsulated behind the tool). Every catalog field is XML-escaped
  (descriptions come from arbitrary, possibly cloned, skill files and land in
  the system prompt — markup must not break out of the block), and the
  rendered catalog is capped at 32 KiB with an omission note, mirroring the
  AGENTS.md cap.
- **Levels 2/3**: activation is exactly that — the model calls `load_skill`
  with the skill's name (getting the SKILL.md body plus the skill's
  directory), reads referenced files through the same tool's `file` argument,
  and runs bundled scripts through the existing `run_command` tool (the
  spec's own script guidance — `uv run`, `npx`, `go run` — is argv-shaped, so
  the argv-only run_command decision stands).
- The skills catalog participates in the same mtime-based freshness check as
  AGENTS.md: the probe stats the discovery roots (add/remove detection) AND
  each discovered skill's SKILL.md (in-place description edits are detected
  too, at the cost of N extra stats per turn).
- `allowed-tools` (spec-experimental) is ignored in P1.

## Toolsets and load_skill

Built-in tools are grouped into named **toolsets** (one source file per set in
`tool/`, named after it): a provider's `tools:` config keys are set names, and
each set decodes one shared config instance for all its tools. Current sets:
`command` (run_command; config = allowed program globs) and `agent`
(load_skill; no settings yet). Future sets slot in the same way (`code` for
editing, `web` for browse/search). Agent mode auto-registers the `agent` set;
a `tools:` entry may still declare it explicitly (the configured instance
wins).

`load_skill` — the agent set's activation tool (this replaced the
general-purpose `read_file`, which could read anything on the machine):

- Arguments: `skill` (required, the catalog name), optional `file` (a path
  relative to the skill's directory), optional `offset`/`limit` line window.
- Without `file`: returns the SKILL.md body (frontmatter consumed) headed by
  the skill's name and directory — the directory is what the model passes to
  `run_command` for bundled scripts.
- With `file`: serves a file bundled in the skill's directory; absolute paths
  and `..` escapes are rejected (symlink escapes wait for the P2
  workspace-trust pass). Size cap + output truncation with a continuation
  marker, as before.
- Read-only: no write/edit tools in P1 (those arrive with the P2 permission
  framework).

## Tool-loop safety cap

`executeWithTools` gains a max-rounds guard (generous, e.g. 50 rounds),
returning a clear error when exceeded. Applies in all modes — a runaway loop
is a defect everywhere; the cap is far above legitimate use.

## Project-scoped sessions

Directory-as-index layout; meta stays the source of truth:

```
~/.chatchain/sessions/<id>/                      # normal sessions, unchanged
~/.chatchain/sessions/projects/<slug>/<id>/      # agent-mode sessions
```

- `<slug>` encodes the absolute project root (path separators → `-`,
  Claude Code style).
- `sessionMeta` gains `cwd` (omitempty; recorded in both modes) — labels and
  any future reorganisation read it from meta, not from the path.
- Listing a project's sessions is one readdir of its bucket — O(own sessions).
- A **locator** resolves ids across both layouts for `--resume`, `/session`
  and deletion; unique-prefix matching semantics unchanged.
- Agent mode: `/session` and `--resume` list only the current project's
  bucket. Normal mode: the global list walks both layouts (project sessions
  are labelled with their project), so nothing is ever invisible.
- Old sessions (no `cwd`, flat layout) behave exactly as today.

## Out of scope for P1 (P2+)

write_file/edit_file/list_dir/grep, the interactive permission framework
(allowlist + session-scoped always-allow), workspace trust for project
skills, `allowed-tools`, skills validation command, cwd/git context injection.

## Testing

AGENTS.md chain assembly (nesting, cap, mtime reuse); composeSystem overlay
(history stays clean, persisted bundle has no overlay); skills discovery
precedence + frontmatter validation (invalid skipped); catalog rendering;
load_skill (resolution, jail, window, cap, errors); loop cap; project slug encoding;
locator across layouts; agent-off = byte-identical behavior (regression).
