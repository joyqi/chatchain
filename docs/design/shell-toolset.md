# The shell toolset — real bash behind an OS sandbox

The `shell` built-in toolset holds one tool, `bash`: a real shell command
line (pipes, redirects, chaining, heredocs), replacing the earlier argv-only
`run_command` and its program allow-list — with a full shell, argv
allow-listing is meaningless, so containment moved down a layer, to the OS.

## Survey — how the industry sandboxes agent shells

| agent | primitive | writes | network | prompts |
|---|---|---|---|---|
| Claude Code (sandbox-runtime) | macOS Seatbelt / Linux bubblewrap | workspace | localhost proxy + domain allowlist | none inside the sandbox |
| Codex CLI | macOS Seatbelt (`sandbox-exec`) / Linux bwrap + seccomp (Landlock fallback) | read-only / workspace-write modes | off by default | approval escalation |
| Gemini CLI | Seatbelt profiles or Docker/Podman | workspace | optional proxy | `--sandbox` flag |

Adopted consensus: **Seatbelt on macOS, bubblewrap on Linux; writes = project
root + temp; network off by default; sandboxed runs don't prompt, unsandboxed
runs need consent.** The domain-allowlist network proxy (Claude Code's) is
deliberate future work.

## Layering

- **internal/shell** — mechanism: `Run(ctx, Options) Result` (bash lookup,
  execution, timeout, streaming capped buffer + middle-elided output truncation) and the platform
  sandbox builders (`sandbox_darwin.go` Seatbelt, `sandbox_linux.go` bwrap,
  `sandbox_other.go` none). `Result` is structured (exit code, timeout,
  cancellation, setup error) — no model-facing wording here.
- **tool/shell.go** — policy: config decoding, the tool surface (Def/args),
  result formatting, and the approval contract.

## Sandbox semantics

Writable roots: the project root, `os.TempDir()`/`/tmp`, the user cache dir
(Go/npm/pip build caches live there — blocking it breaks every build), plus
configured `write:` extras (`~` expands). Network is blocked unless
`network: true`.

- **macOS**: `/usr/bin/sandbox-exec` with an allow-default SBPL profile that
  denies `file-write*` outside the writable roots and (optionally)
  `network*`; the last matching rule wins, so targeted allows override the
  broad denies. Paths enter via `-D` parameters, never spliced into the
  profile (quoting stays sandbox-exec's problem). `/tmp`//`/var` symlink
  twins under `/private` are added automatically.
- **Linux**: `bwrap --ro-bind / /` with the writable roots bind-mounted
  read-write on top, `--unshare-net` unless network is enabled,
  `--die-with-parent`. Requires bwrap on PATH and unprivileged user
  namespaces.
- **Windows / no bwrap**: no sandbox — see approval below.

## Approval

`bash` implements the same `approver`/`tool.ApprovalReporter` seam as the
code set: a sandboxed call is pre-contained and runs **without prompting**
(the industry default); an unsandboxed call (platform gap or `sandbox: off`)
asks allow once / allow for this session / deny, and is rejected outright in
non-interactive `-m` runs. `auto_run: true` waives approval for unsandboxed
calls.

## Config

```yaml
tools:
  shell:               # empty = sandbox auto + network blocked
    # sandbox: off     # disable the sandbox (calls then need approval)
    # network: true    # allow network inside the sandbox
    # auto_run: true   # skip approval for unsandboxed calls (incl. -m)
    # write: [~/go/pkg]  # extra sandbox-writable paths
```

The pre-bash allow-list config shape (`command: [git, ssh]`) is gone with the
`command` set name itself; a list-shaped value fails the mapping decode and
the set is skipped with a warning.

## Execution details

10-minute hard timeout per call, earlier cancellation via ESC (the chat layer
cancels the context; sandbox-exec execs bash in-process and bwrap uses
`--die-with-parent`, so the tree dies with the wrapper).

Output is capped on **both axes**, sized to the industry norms (Claude Code
clips bash output at 30k characters; Codex CLI clips by lines and bytes):
**32 KB** (head 8 KB + tail 22 KB — bytes bound the token cost, the actual
context guard) and **512 lines** (head 128 + tail 384 — keeps many-short-line
output readable), middle elided with markers that tell the model to narrow
via head/tail/grep. Compiler and test failures sit at the end, context at the
start, so the tail budget dominates. The output is also **capped while
streaming** (a head buffer + rolling tail ring, ~52 KB of memory total), so a
command printing gigabytes never grows iota's memory. cwd defaults to
the project root; relative `cwd` arguments resolve against it.

## Deferred (v2+)

Domain-allowlist network proxying (sandbox-runtime style), persistent shell
sessions and background processes, Landlock as a bwrap-less Linux fallback,
Windows sandboxing, per-command policy classification (Codex execpolicy
style), and a sandbox-denial → "retry unsandboxed with approval?" escalation
flow.

## Testing

internal/shell: shell semantics (pipes/exit codes/cwd), cancellation,
truncation, writable-path derivation, and real sandbox isolation (in-root
write allowed, out-of-root denied, `write:` extras honored) with an
environment probe that skips where the sandbox cannot run (e.g. nested
sandboxes). tool: config matrix (sandbox/auto_run → RequiresApproval),
legacy-shape rejection, model-facing formatting, registry/Merge integration.
