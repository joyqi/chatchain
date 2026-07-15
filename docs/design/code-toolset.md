# The code toolset — locate → read → edit, verified by run_command

The `code` built-in toolset gives the model the minimal coding loop used by
every mainstream coding agent (Claude Code, Codex CLI, Gemini CLI, Aider):
**locate** (glob/grep/list_dir) → **read** (read_file) → **edit**
(edit_file/write_file). The **verify** step is deliberately not part of this
set — builds and tests run through the `command` set's argv-only run_command,
so execution policy stays in one place.

## Survey conclusions adopted

- **Edits are exact string replacements with a uniqueness constraint**
  (Claude Code Edit, Gemini CLI replace), not diff/patch application (Codex
  apply_patch, Aider): string matching is the most LLM-robust editing scheme;
  a patch format can come later as a separate tool if edit failure rates
  warrant it.
- **Read-before-edit is enforced**, with external-modification detection: the
  set keeps a session ledger of `path → mtime at last read`; edit_file and
  write_file (on existing files) refuse when the file was never read or
  changed on disk since. A successful write refreshes the ledger.
- **Reads are line-numbered** (`%6d\t`), all outputs are capped (64 KB per
  call) with continuation markers naming the next offset.

## Tools

| tool | args | notes |
|---|---|---|
| `glob` | pattern, path? | `**` via doublestar; bare patterns match any depth; newest first; caps 200 shown / 10000 examined |
| `grep` | pattern, path?, include?, context? | Go/RE2 regex; `path:line: text` (context lines `-`); 100-match cap; 10 MB/file cap; long lines truncated |
| `list_dir` | path? | one level; `dir/` + `file (size)`; 500-entry cap |
| `read_file` | path, offset?, limit? | numbered window; 20 MB read / 64 KB output caps; binary sniff (NUL) rejects |
| `edit_file` | path, old_string, new_string, replace_all? | unique-match string replace; snippet of the change echoed back |
| `write_file` | path, content | whole-file create/overwrite; parents auto-created |

All paths are **jailed to the project root** (`Env.ProjectRoot`, cwd
fallback): relative paths resolve against the root, absolute paths must stay
inside it, `..` escapes are rejected. glob/grep skip `.git` and the root
`.gitignore`'s matches (re-read per call; nested .gitignore files are a later
refinement), plus binary files.

## Approval — the first slice of the P2 permission framework

Mutating tools implement the package-private `approver` interface; the
`tool.ApprovalReporter` capability surfaces it through Registry and Merge
(parts without the capability — MCP servers — keep their old behavior).

- **Interactive (chat.Run)**: before an approval-requiring call, the tool
  header is already on screen; a selector asks Allow once / Allow for this
  session / Deny. Session allowance is per tool name and lives for the Run.
  A denial becomes an isError tool result ("The user declined this call."),
  so the provider still receives a result for every call id.
- **Non-interactive (-m / chat.Once)**: approval-requiring calls are rejected
  with a result explaining how to enable them.
- **`tools.code.auto_write: true`** waives approval entirely (the tools then
  report RequiresApproval=false), which also unlocks writes in -m runs.

## Config

```yaml
tools:
  code:                # empty = jail to project root + confirm writes
    # auto_write: true # skip write confirmations (and allow writes in -m)
```

## Deferred (v2+)

apply_patch-style multi-file edits, background/persistent processes (a
command-set evolution), LSP diagnostics, LLM edit self-correction (Gemini
editCorrector), nested .gitignore semantics, symlink-escape hardening under
workspace trust, a diff preview inside the approval prompt, and the `web`
set. The rest of P2 (workspace trust, broader permission policies) builds on
the same ApprovalReporter seam.

## Testing

Path jail (absolute/`..`); glob depth default, gitignore exclusion, mtime
order; grep format, include filter, context, binary skip, bad regex;
list_dir; read_file numbering/window/binary/empty; edit_file read-gate,
uniqueness, replace_all, staleness, snippet; write_file create/parents,
overwrite gate, ledger refresh by writes; approval matrix incl. auto_write
and Merge routing (tool/code_test.go).
