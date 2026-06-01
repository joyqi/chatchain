# ChatChain

A lightweight, cross-platform AI chat CLI built with Go. Supports multiple providers, streaming responses, file attachments, and an interactive terminal UI.

## Features

- **Multi-provider** — OpenAI, OpenAI Responses API, Anthropic, Gemini, and Vertex AI, with custom base URL support
- **MCP tool support** — connect external MCP tool servers (filesystem, GitHub, databases, etc.) and let AI providers use them during chat
- **Interactive model selection** — arrow-key navigation with filtering
- **Slash-command completion** — type `/` and the command menu pops up and live-filters as you type; the command is highlighted inline (no manual Tab needed)
- **Streaming responses** — real-time token output with loading spinner
- **Markdown highlighting** — inline ANSI styling for headings, bold, italic, code, tables, and code blocks in streaming output
- **File attachments** — send images, PDFs, and text files alongside messages with Tab-completion for file paths
- **Non-interactive mode** — single message in, response out, pipe-friendly
- **Conversation history** — full context maintained within a session
- **Session persistence** — every interactive session is auto-saved (losslessly: messages, tool calls, attachments, reasoning) to `~/.chatchain/sessions/`. Resume with `/session` in chat or `--resume[=<id>]` at launch; auto-titled by the model after the first reply; `--no-save` runs ephemerally
- **Context management** — live token accounting against the context window (configurable via `--context-window` / `/context`), with `/compact` LLM-summarization of older history (and automatic compaction before the window fills)
- **Switch model mid-chat** — `/model` re-selects the model without restarting
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
| `--config` | `-c` | Path to config file (default: `~/.chatchain.yaml`) |
| `--verbose` | `-v` | Print HTTP request/response bodies for debugging |

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
    key: sk-deepseek-xxx
    url: https://api.deepseek.com/v1
    model: deepseek-chat
    system: "You are a helpful coding assistant"

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

#### MCP Variable Expansion

MCP server config values (`command`, `args`, `url`, `env`, `headers`) support VS Code-style variable expansion:

| Variable | Expands to |
|----------|-----------|
| `${workspaceFolder}` / `${cwd}` | Current working directory |
| `${userHome}` | User home directory |
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

### Chat Commands

In interactive mode, the following commands are available. Typing `/` on an empty
line opens the command menu automatically — it live-filters as you keep typing, and
the command is colorized inline (green once complete, cyan while a valid prefix):

| Command | Description |
|---------|-------------|
| `/file [path]` | Attach a file (image, PDF, or text). With no path, opens a file browser; with a path, supports Tab completion. |
| `/files` | Manage attached files — multi-select (Space) and remove (Enter) |
| `/session` | List saved sessions and resume one |
| `/sessions` | Clean up saved sessions — multi-select and delete |
| `/model` | Switch the model for the current session |
| `/context [size]` | Show/set the context window (e.g. `200k`, `1m`); no arg opens a picker |
| `/compact [hint]` | Summarize older history to free context; optional hint guides what to keep |
| `/status` | Show provider, model, context usage, and last-turn token counts |
| `/mcp` | Show connected MCP servers and their tools |

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

# Interactive system prompt input (prompts System> after model selection)
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
You> /files
  [1] photo.png (image/png, 240.0 KB)
  [2] report.pdf (application/pdf, 100.0 KB)
You> Summarize the report and describe the photo
Assistant> ...
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
│   ├── chat.go          # Chat loop, model selection, tool-call loop, spinner
│   ├── file.go          # File attachment reading and MIME detection
│   ├── markdown.go      # Streaming markdown highlighter (ANSI)
│   └── styles.go        # Terminal style definitions
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
- [promptui](https://github.com/manifoldco/promptui) — Interactive prompts
- [readline](https://github.com/ergochat/readline) — Line editing with tab completion (CJK-aware)
- [spinner](https://github.com/briandowns/spinner) — Loading spinners
- [openai-go](https://github.com/openai/openai-go) — OpenAI SDK
- [anthropic-sdk-go](https://github.com/anthropics/anthropic-sdk-go) — Anthropic SDK
- [go-genai](https://github.com/googleapis/go-genai) — Google Gemini SDK
- [go-sdk (MCP)](https://github.com/modelcontextprotocol/go-sdk) — Model Context Protocol SDK

## License

MIT
