---
name: iota
description: >
  Send questions to other LLM models — GPT-4o, GPT-4, o1, o3, Gemini, Claude (via API), etc.
  Use this skill whenever the user wants to ask, query, chat with, or get answers from another AI model,
  including: "ask GPT...", "let Gemini explain...", "what does Claude think about...",
  "compare answers from different models", "get a second opinion from another LLM",
  or any request that involves calling OpenAI, Anthropic, Gemini, or Vertex AI models.
allowed-tools:
  - Bash(iota *)
---

# iota CLI — Agent Skill

iota is a CLI tool for chatting with multiple LLM providers.

## Prerequisites

First check if iota is installed:

```bash
command -v iota
```

If not installed, install via Homebrew:

```bash
brew install joyqi/tap/iota
```

Or via Go:

```bash
go install github.com/joyqi/iota@latest
```

## CRITICAL: Discover Providers and Models Before Calling

**DO NOT guess or hardcode provider names or model names.** Always discover them first using `iota -l`.

### Step 1: List available providers

```bash
iota -l
```

This shows all built-in providers and any custom aliases configured in `~/.iota.yaml`. Only use providers that appear in this list.

### Step 2: List available models for the chosen provider

```bash
iota -l <provider>
```

This queries the provider's API and returns the actual available models. Only use model names that appear in this list. If the user asks for a specific model (e.g. "ask GPT-4o"), find the closest match from the list.

### Step 3: Send the message

```bash
iota <provider> -M <model> -m "<message>"
```

## Key Flags

| Flag | Description |
|------|-------------|
| `-l, --list` | List configured providers (no arg), or models for a provider (with arg) |
| `-M, --model <model>` | Specify model — **must be a real model from `iota -l <provider>`** |
| `-m, --message <msg>` | Non-interactive mode: send a single message and exit (use `-` to read from stdin) |
| `-s, --system <prompt>` | Set system prompt |
| `-t, --temperature <val>` | Set temperature (0.0–2.0) |
| `-k, --key <key>` | API key (overrides env var) |
| `-u, --url <url>` | Custom API base URL |
| `-c, --config <path>` | Path to config file (default: `~/.iota.yaml`) |
| `--max-turns <n>` | Limit agentic tool turns in non-interactive mode (default: unlimited) |
| `--output-format <fmt>` | `-m` output: `text` (default) or `json` — one result object carrying the reply, per-round token usage and timing |

## Providers and Environment Variables

| Provider | Subcommand | Env Var | Notes |
|----------|-----------|---------|-------|
| OpenAI | `openai` | `OPENAI_API_KEY` | GPT models |
| Anthropic | `anthropic` | `ANTHROPIC_API_KEY` | Claude models |
| Gemini | `gemini` | `GOOGLE_API_KEY` | Gemini models |
| Vertex AI | `vertexai` | `GOOGLE_API_KEY` | Express (API-key) mode |
| OpenAI Responses | `openresponses` | `OPENAI_API_KEY` | OpenAI Responses API |

Custom aliases may also be configured in `~/.iota.yaml` (e.g. `deepseek`, `chatgpt`). Always run `iota -l` to see the full list.

## Config File

iota supports a YAML config file (`~/.iota.yaml`) for persistent API keys, default models, and custom provider aliases. Priority: CLI flag > env var > config file.

```yaml
providers:
  deepseek:
    type: openai
    key: sk-deepseek-xxx
    url: https://api.deepseek.com/v1
    model: deepseek-chat
    system: "You are a helpful coding assistant"
```

With a config like this, `iota deepseek -m "hello"` works as a provider alias.

## Usage Examples

### Full workflow (recommended)

```bash
# 1. Discover providers
iota -l

# 2. Pick a provider, discover its models
iota -l openai

# 3. Send the message with a real model name
iota openai -M gpt-4o -m "What is the capital of France?"
```

### With system prompt

```bash
iota anthropic -M claude-sonnet-4-20250514 -s "You are a helpful coding assistant" -m "Explain async/await in JavaScript"
```

### Pipe content via stdin

```bash
echo "Summarize this text" | iota gemini -M gemini-2.0-flash -m -
```

Note: `-m -` (dash) reads the message from stdin.

### With temperature

```bash
iota openai -M gpt-4o -t 0.7 -m "Write a haiku about programming"
```

## Important Notes

- **NEVER guess provider or model names** — always run `iota -l` and `iota -l <provider>` first
- Always use `-m` for non-interactive mode (otherwise it opens an interactive TUI)
- Use `-m -` to read the message from stdin
- If no `-M` is specified, iota will prompt for model selection interactively (avoid this in automation)
- API keys are read from environment variables by default; use `-k` only if the env var is not set
