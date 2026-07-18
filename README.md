# ChatChain

A lightweight, cross-platform AI chat CLI built with Go. Supports multiple providers, streaming responses, file attachments, and an interactive terminal UI.

## Features

- **Multi-provider** — OpenAI, OpenAI Responses API, Anthropic, Gemini, and Vertex AI, with custom base URL support
- **MCP tool support** — connect external MCP tool servers (filesystem, GitHub, databases, etc.) and let AI providers use them during chat, with tool names namespaced per server (`mcp__<server>__<tool>`) so same-named tools never collide
- **Interactive model selection** — arrow-key list at startup
- **Slash-command completion** — a suggestion row appears when the line starts with `/` and narrows as you type; press Tab to cycle through the completions
- **Streaming responses** — real-time token output with a busy spinner in the status line; keep typing while a reply streams (type-ahead — queued submits are sent in order); press **Esc** (cancels the innermost running scope) or **Ctrl+C** (cancels the turn) to interrupt a streaming reply — the partial reply is kept in history and marked interrupted
- **Markdown highlighting** — inline ANSI styling for headings, bold, italic, code, tables, and code blocks in streaming output; inline LaTeX math (`$...$`, `\(...\)`) is approximated in Unicode, and display math (`$$...$$`, `\[...\]`) renders as a 2D block — stacked fractions, roots with a drawn vinculum, matrices, aligned environments, sums/integrals with limits, drawn accents (`\hat`/`\vec`/`\bar`), and math fonts (`\mathbb`/`\mathcal`); anything unsupported falls back to a readable single-line approximation, never raw LaTeX
- **File attachments** — send images, PDFs, and text files alongside messages; `/file` opens a tabbed surface with the attached list and a directory browser
- **Non-interactive mode** — single message in, response out, pipe-friendly
- **Conversation history** — full context maintained within a session
- **Session persistence** — every interactive session is auto-saved (losslessly: messages, tool calls, attachments, reasoning) to `~/.chatchain/sessions/`. Resume with `/session` in chat or `--resume[=<id>]` at launch (any unique id prefix works), and resuming echoes the last few exchanges back to the terminal; auto-titled by the model after the first reply; `--no-save` runs ephemerally
- **Context management** — live token accounting against the context window (configurable via `--context-window` or the `/model` Context tab), with `/compact` LLM-summarization of older history; when the window nears full a confirmation is offered before compacting (declining snoozes the prompt until usage grows further)
- **Model settings mid-chat** — `/model` opens a tabbed panel over the model, context window, reasoning effort, and temperature, all persisted with the session and replayed on resume
- **Conversation export** — `/export` renders the session to a single self-contained HTML file (inline CSS, dark mode with a toggle, syntax-highlighted code) or a plain Markdown document; saved sessions export the full on-disk log, so compaction never hides older rounds (ephemeral `--no-save` sessions export the current in-memory view)
- **Agent mode** — opt-in via `--agent` (or `agent: true` per provider): layered `AGENTS.md` instructions and [Agent Skills](https://agentskills.io/specification) are injected as a volatile system-prompt overlay, the `agent` toolset (`load_skill`) is auto-enabled, and sessions are grouped per project
- **Request inspector** — `/debug` opens a two-tab console: a **Verbose** toggle turns recording on/off (off by default), and **Messages** browses the captured API calls (newest first), each summarized by action and content (e.g. `Chat 你好…`) rather than raw method/URL — drill into any one to read its `↑ Request` and `↓ Response` bodies, pretty-printed, with `c` to copy to the clipboard. Nothing is printed to the terminal; it replaces the old `-v` flag
- **System prompt** — set via flag or interactive input
- **Config file** — persistent API keys, default models, custom provider aliases, and MCP server definitions via `~/.chatchain.yaml`
- **Styled terminal output** — color-coded prompts

## Install

### Homebrew (macOS)

```bash
brew tap joyqi/tap
brew install chatchain
```

### Go

```bash
go install github.com/joyqi/chatchain@latest
```

### Build from source

```bash
git clone https://github.com/joyqi/chatchain.git
cd chatchain
go build -o chatchain .
```

## Claude Code Plugin

ChatChain provides a [Claude Code](https://docs.anthropic.com/en/docs/claude-code) plugin, allowing you to call other LLMs directly within Claude Code.

### Plugin Install

```bash
# Add the marketplace
/plugin marketplace add joyqi/chatchain

# Install the plugin
/plugin install chatchain@chatchain-marketplace
```

### Plugin Usage

**Slash command** — manually ask another LLM:

```
/chatchain:ask openai gpt-4o "What is the meaning of life?"
/chatchain:ask anthropic claude-sonnet-4-20250514 "Explain monads"
/chatchain:ask gemini "Write a haiku"
```

**Agent skill** — Claude automatically uses ChatChain when you ask it to query another LLM:

```
> Use chatchain to ask GPT what is 1+1
> Ask Gemini to explain quicksort via chatchain
```

### Local Testing

```bash
claude --plugin-dir ./chatchain-plugin
```

## Usage

```bash
chatchain [openai|anthropic|gemini|vertexai|openresponses] [flags]
```

### Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--key` | `-k` | API key (or set via env var) |
| `--url` | `-u` | Custom base URL |
| `--model` | `-M` | Model name (skip interactive selection) |
| `--temperature` | `-t` | Sampling temperature, 0.0-2.0 (omit to use provider default) |
| `--message` | `-m` | Send a single message and print the response (non-interactive, use `-` to read from stdin) |
| `--system` | `-s` | System prompt |
| `--system-input` | `-S` | Enter system prompt interactively |
| `--list` | `-l` | List configured providers, or models for a given provider |
| `--mcp` | | MCP server (command string or URL, repeatable) |
| `--agent` | | Enable agent mode (AGENTS.md overlay, skills, `load_skill`, project-scoped sessions) |
| `--config` | `-c` | Path to config file (default: `~/.chatchain.yaml`) |

### Environment Variables

| Variable | Provider |
|----------|----------|
| `OPENAI_API_KEY` | OpenAI / OpenResponses |
| `ANTHROPIC_API_KEY` | Anthropic |
| `GOOGLE_API_KEY` | Gemini / Vertex AI |

### Config File

ChatChain supports YAML config files for persistent settings and custom provider aliases.

#### Config Lookup Order

1. `~/.chatchain.yaml` or `~/.chatchain.yml` (global)
2. `./.chatchain.yaml` or `./.chatchain.yml` (project-local, merges over global)
3. `-c/--config <path>` (explicit, highest priority, used alone)

Same-name providers in later files override earlier ones.

#### Priority

For individual values: **CLI flag > env var > config file**.

#### Example

```yaml
# ~/.chatchain.yaml
providers:
  openai:
    key: sk-official
    model: gpt-4o

  deepseek:                  # custom alias
    type: openai             # underlying provider type
    key: ${env:DEEPSEEK_KEY} # key/url/system_file expand ${…} variables
    url: https://api.deepseek.com/v1
    model: deepseek-chat
    system: "You are a helpful coding assistant"

  reviewer:
    type: openresponses
    key: sk-xxx
    model: gpt-5.2
    system_file: ${chatchainHome}/prompts/reviewer.md  # prompt from a file (inline `system` wins)
    effort: high             # default reasoning effort: low|medium|high|xhigh|max
    mcp_servers: [github]    # load only these MCP servers; [] = none; key absent = all

  claude:
    type: anthropic
    key: sk-ant-xxx
    model: claude-sonnet-4-20250514

# MCP tool servers
mcp_servers:
  filesystem:
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "${workspaceFolder}"]

  github:
    url: https://mcp.example.com/sse
    headers:
      Authorization: "Bearer ${env:GITHUB_TOKEN}"
```

#### Variable Expansion

Provider config values (`key`, `url`, `system_file`) and MCP server values (`command`, `args`, `url`, `env`, `headers`) support VS Code-style variable expansion:

| Variable | Expands to |
|----------|-----------|
| `${workspaceFolder}` / `${cwd}` | Current working directory |
| `${userHome}` | User home directory |
| `${chatchainHome}` | chatchain's global directory (`~/.chatchain`) |
| `${pathSeparator}` / `${/}` | OS path separator (`/` or `\`) |
| `${env:VAR}` | Value of environment variable `VAR` |

Unknown variables are left untouched.

With this config:

```bash
# Use the "deepseek" alias — resolves to OpenAI provider with DeepSeek's key/URL/model
chatchain deepseek -m "hello"

# Config key used, no need for -k
chatchain openai -m "hi" -M gpt-4o

# CLI flag overrides config
chatchain openai -k sk-override -m "hi" -M gpt-4o
```

### Built-in Toolsets

Besides MCP servers, ChatChain ships built-in tools grouped into named
**toolsets** that you enable per provider in the config file. A toolset is
enabled by listing it under that provider's `tools:` key; the value is the
set's shared configuration, and an empty value uses its defaults. Available
sets: `shell` (running bash commands, sandboxed), `code` (reading, searching,
and editing project files), and `agent` (skill activation; auto-enabled by
agent mode).

```yaml
providers:
  claude:
    type: anthropic
    key: sk-ant-xxx
    model: claude-sonnet-4-20250514
    tools:
      shell:                 # empty → sandboxed, network blocked
      code:

  openai:
    key: sk-official
    model: gpt-4o
    tools:
      shell:
        network: true        # allow network inside the sandbox
        write: [~/go/pkg]    # extra sandbox-writable paths
```

#### `shell` — `bash`

Lets the model run real bash command lines — pipes, redirects, `&&` chaining,
heredocs — and returns their combined stdout/stderr. The model calls it with
`command` (required) and an optional `cwd` (defaults to the project root).

Safety model — the same one Claude Code and Codex CLI use:

- **OS sandbox by default.** On macOS commands run under Seatbelt
  (`sandbox-exec`, built into the system); on Linux under
  [bubblewrap](https://github.com/containers/bubblewrap) (`bwrap`, if
  installed). Inside the sandbox, file **writes are confined to the project
  root plus temp/cache directories** (add more via `write:`), and **network
  access is blocked** unless `network: true`.
- **Sandboxed calls run without prompting.** Where no sandbox is available
  (Windows, Linux without bwrap) or with `sandbox: off`, every call instead
  asks for confirmation in the chat (allow once / allow for this session /
  deny), and non-interactive `-m` runs reject it — set `auto_run: true` to
  waive that.
- Output is capped at 32 KB and 512 lines (head + tail kept, middle elided,
  bounded even while streaming). Each call is capped at **10 minutes**; while a command runs, the status-line spinner
  shows the elapsed time — press **ESC** (or Ctrl+C) to terminate it.

Design: docs/design/shell-toolset.md

#### `code` — coding tools

The coding loop: `glob` and `grep` locate files (`.git`, `.gitignore` matches,
and binaries excluded), `list_dir` explores, `read_file` returns line-numbered
content, and `edit_file` (exact, unique string replacement) / `write_file`
change files. Everything is confined to the **project root** (the git root of
the working directory). Verification — builds, tests — goes through the
`shell` set's `bash`, so enable it alongside.

Safety model:

- A file must be **read before it can be modified**, and a file that changed
  on disk since it was read must be re-read first — the model can never
  blind-overwrite your edits.
- Every modifying call asks for confirmation in the chat (allow once / allow
  for this session / deny). Non-interactive `-m` runs reject modifications
  outright. Set `auto_write: true` under `tools: code:` to skip confirmations
  and allow `-m` writes:

```yaml
    tools:
      code:
        auto_write: true   # optional; default asks before every write
```

Design: docs/design/code-toolset.md

### Agent Mode

Agent mode is explicitly opt-in — pass `--agent`, or set `agent: true` on a
provider in the config file. Off means exactly the ordinary chat behavior.

```yaml
providers:
  claude:
    type: anthropic
    key: sk-ant-xxx
    agent: true
```

Everything is anchored at the **project root**: the git root of the working
directory, or the working directory itself outside a repository.

#### AGENTS.md

Following the [AGENTS.md convention](https://agents.md/), every `AGENTS.md`
from the project root down to the current directory (at most one per
directory) is concatenated root-first — nearer files come later and override —
capped at 32 KiB, and appended to the system prompt as a **volatile overlay**:
composed at send time, never stored in the conversation history or the session
file, and re-read automatically when a file changes between turns (a dim
`AGENTS.md reloaded` notice is printed). Resuming a session elsewhere applies
that directory's `AGENTS.md`.

#### Skills

Skills follow the [Agent Skills specification](https://agentskills.io/specification):
a skill is a directory containing a `SKILL.md` with `name` and `description`
frontmatter. Discovery directories, highest precedence first (same-name skill:
higher wins):

1. `<project root>/.agents/skills/` — project skills
2. `~/.chatchain/skills/` — chatchain user skills
3. `~/.agents/skills/` — cross-client user skills

Discovered skills are advertised to the model as a name + description catalog
inside the overlay; the model activates one by calling `load_skill` with the
skill's name, reads files the skill references through the same tool's `file`
argument, and runs bundled scripts through `bash` (enable the `shell`
toolset for the provider if your skills need scripts). Invalid skills are
skipped with a warning, never fatal.

#### `agent` — `load_skill`

Agent mode auto-enables the `agent` toolset. Its `load_skill` tool activates a
skill by name: it returns the skill's instructions (the `SKILL.md` body) and
directory, and the optional `file` argument reads a file bundled inside that
directory — reads never leave the skill's directory. Output is size-capped
with an optional `offset`/`limit` line window. The set can also be enabled
explicitly under `tools:` like any other, agent mode or not.

#### Project-Scoped Sessions

Sessions started in agent mode are stored per project under
`~/.chatchain/sessions/projects/<slug>/`, and `/session` and `--resume` list
only the current project's sessions there (`--resume=<id>` with an id from
anywhere still works). Normal-mode sessions stay in the flat global store,
whose list also shows every project's sessions labelled with their project —
nothing is ever invisible.

### Chat Commands

In interactive mode, the following commands are available. When the line starts
with `/`, a suggestion row appears below the input and narrows as you keep typing;
press Tab to cycle through the completions:

| Command | Description |
|---------|-------------|
| `/file [path]` | Attach a file (image, PDF, or text). With a path, attaches directly. With no path, opens a tabbed selector: "Attached" to remove attachments, "Add" to pick one from a directory browser. |
| `/session` | Tabbed selector over saved sessions: "Resume" to resume one, "Delete" to multi-select and delete others. |
| `/model` | Tabbed settings for the current session: "Model" picks the model, "Context" the context window, "Effort" the reasoning effort (`default`, `low`, `medium`, `high`, `xhigh`, `max` — passed to the provider verbatim, so a level the model doesn't support surfaces as an API error and you pick another), "Temperature" a slider (`default` omits the parameter). Enter applies all four tabs; only changed values are announced. |
| `/compact [hint]` | Summarize older history to free context; optional hint guides what to keep |
| `/export [file]` | Export the conversation (saved sessions: the full on-disk log, so compaction never hides older rounds) to a single self-contained HTML file — the default — or Markdown with a `.md`/`.markdown` extension. With no argument, a selector picks the format and the filename is generated from the session title. Never overwrites an existing file. |
| `/status` | Show provider, model, context usage, and last-turn token counts |
| `/tools` | Tabbed read-only view of the model's capabilities: a "Tools" tab (every built-in and MCP tool with its source) and an "MCP" tab (server status, endpoints, and tools) |
| `/skills` | List discovered agent skills — name, source (project/user), description, and any invalid skills that were skipped. Agent mode only. |

Attached files are sent with your next message, then cleared automatically.

#### Supported File Types

| Type | Extensions |
|------|-----------|
| Images | `.jpg`, `.jpeg`, `.png`, `.gif`, `.webp` |
| Documents | `.pdf` |
| Text | `.txt`, `.md`, `.go`, `.py`, `.js`, `.ts`, `.json`, `.yaml`, `.html`, `.css`, `.sql`, `.csv`, and more |

### Examples

```bash
# Interactive model selection
chatchain openai -k sk-xxx

# Specify model directly
chatchain openai -k sk-xxx -M gpt-4o

# Use Anthropic
chatchain anthropic -M claude-sonnet-4-20250514

# Use Gemini
chatchain gemini -M gemini-2.5-flash

# Use Vertex AI (with custom endpoint)
chatchain vertexai -u https://your-proxy.com/api/vertex-ai -M gemini-2.5-flash -m "Hello"

# Use OpenAI Responses API
chatchain openresponses -M gpt-4o -m "Hello"

# With system prompt
chatchain openai -M gpt-4o -s 'You are a helpful translator' -m "Translate to French: hello"

# Interactive system prompt input (prompts inside the chat UI before the first message)
chatchain openai -M gpt-4o -S

# Non-interactive mode (requires -M)
chatchain openai -M gpt-4o -m "Explain quicksort in one paragraph"

# Adjust temperature
chatchain anthropic -M claude-sonnet-4-20250514 -t 0.5 -m "Write a haiku"

# Custom API endpoint
chatchain openai -u https://your-proxy.com/v1 -k sk-xxx

# With MCP tools (ad-hoc server via CLI flag)
chatchain openai -M gpt-4o --mcp "npx -y @modelcontextprotocol/server-filesystem /tmp"

# Multiple MCP servers
chatchain anthropic -M claude-sonnet-4-20250514 --mcp "npx -y @modelcontextprotocol/server-filesystem /tmp" --mcp "https://mcp.example.com/sse"

# MCP servers from config file are loaded automatically
chatchain openai -M gpt-4o

# Read message from stdin (pipe-friendly)
echo "Explain quicksort" | chatchain openai -M gpt-4o -m -
cat prompt.txt | chatchain openai -M gpt-4o -m -

# Use a provider alias from config
chatchain deepseek -m "Explain quicksort"

# List all configured providers and aliases
chatchain -l

# List available models for a provider
chatchain -l openai
chatchain -l deepseek
```

### File Attachment Example

```
You> /file photo.png
  Attached: photo.png (image/png, 245760 bytes)
You> /file report.pdf
  Attached: report.pdf (application/pdf, 102400 bytes)
You> /file
  (tabbed selector — "Attached" to remove, "Add" to browse and add)
You> Summarize the report and describe the photo
...
```

## Project Structure

```
chatchain/
├── main.go              # Entry point
├── cmd/
│   └── root.go          # CLI definition (cobra)
├── config/
│   └── config.go        # Config file loading and merging
├── chat/
│   ├── run.go           # Interactive chat loop, rendered via the internal/ui facade
│   ├── chat.go          # Non-interactive Once path (-m), tool-call loop, shared helpers
│   ├── file.go          # File attachment reading and MIME detection
│   └── styles.go        # Terminal style definitions
├── internal/
│   ├── ui/              # Bubbletea inline UI (composer, status line, tabbed surfaces)
│   ├── markdown/        # Streaming markdown renderer (ANSI, chroma code blocks)
│   ├── mathtext/        # LaTeX math → terminal text rendering
│   └── textwidth/       # Terminal display-width measurement
├── mcp/
│   └── manager.go       # MCP client manager (stdio + HTTP transports)
└── provider/
    ├── provider.go      # Provider interface, ToolProvider, Message types
    ├── openai.go          # OpenAI Chat Completions (+ tool calling)
    ├── openresponses.go   # OpenAI Responses API (+ tool calling)
    ├── anthropic.go       # Anthropic (+ tool calling)
    ├── gemini.go          # Gemini (+ tool calling)
    └── vertexai.go        # Vertex AI (+ tool calling)
```

## Dependencies

- [cobra](https://github.com/spf13/cobra) — CLI framework
- [fatih/color](https://github.com/fatih/color) — Terminal styling
- [bubbletea/v2](https://github.com/charmbracelet/bubbletea) — Terminal UI framework (inline mode)
- [bubbles/v2](https://github.com/charmbracelet/bubbles) — UI components (textarea, progress)
- [lipgloss/v2](https://github.com/charmbracelet/lipgloss) — Style and layout for terminal output (v1 still used by the markdown renderer)
- [x/ansi](https://github.com/charmbracelet/x) — ANSI escape sequence handling
- Provider APIs are spoken directly by a minimal built-in wire client
  (`internal/llm`) — no official SDKs (see docs/design/internal-llm-client.md)
- [go-sdk (MCP)](https://github.com/modelcontextprotocol/go-sdk) — Model Context Protocol SDK

## License

MIT
