---
name: chatchain
description: >
  Send questions to other LLM models — GPT-4o, GPT-4, o1, o3, Gemini, Claude (via API), etc.
  Use this skill whenever the user wants to ask, query, chat with, or get answers from another AI model,
  including: "ask GPT...", "let Gemini explain...", "what does Claude think about...",
  "compare answers from different models", "get a second opinion from another LLM",
  or any request that involves calling OpenAI, Anthropic, Gemini, Vertex AI, or OpenClaw models.
allowed-tools:
  - Bash(chatchain *)
---

# ChatChain CLI — Agent Skill

ChatChain is a CLI tool for chatting with multiple LLM providers.

## Prerequisites

First check if ChatChain is installed:

```bash
command -v chatchain
```

If not installed, install via Homebrew:

```bash
brew tap joyqi/tap && brew install chatchain
```

Or via Go:

```bash
go install github.com/joyqi/chatchain@latest
```

## CRITICAL: Discover Providers and Models Before Calling

**DO NOT guess or hardcode provider names or model names.** Always discover them first using `chatchain -l`.

### Step 1: List available providers

```bash
chatchain -l
```

This shows all built-in providers and any custom aliases configured in `~/.chatchain.yaml`. Only use providers that appear in this list.

### Step 2: List available models for the chosen provider

```bash
chatchain -l <provider>
```

This queries the provider's API and returns the actual available models. Only use model names that appear in this list. If the user asks for a specific model (e.g. "ask GPT-4o"), find the closest match from the list.

### Step 3: Send the message

```bash
chatchain <provider> -M <model> -m "<message>"
```

## Key Flags

| Flag | Description |
|------|-------------|
| `-l, --list` | List configured providers (no arg), or models for a provider (with arg) |
| `-M, --model <model>` | Specify model — **must be a real model from `chatchain -l <provider>`** |
| `-m, --message <msg>` | Non-interactive mode: send a single message and exit (use `-` to read from stdin) |
| `-s, --system <prompt>` | Set system prompt |
| `-t, --temperature <val>` | Set temperature (0.0–2.0) |
| `-k, --key <key>` | API key (overrides env var) |
| `-u, --url <url>` | Custom API base URL |
| `-c, --config <path>` | Path to config file (default: `~/.chatchain.yaml`) |
| `-v, --verbose` | Show raw API responses |

## Providers and Environment Variables

| Provider | Subcommand | Env Var | Notes |
|----------|-----------|---------|-------|
| OpenAI | `openai` | `OPENAI_API_KEY` | GPT models |
| Anthropic | `anthropic` | `ANTHROPIC_API_KEY` | Claude models |
| Gemini | `gemini` | `GOOGLE_API_KEY` | Gemini models |
| Vertex AI | `vertexai` | — | Uses Google Cloud ADC |
| OpenAI Responses | `openresponses` | `OPENAI_API_KEY` | OpenAI Responses API |
| OpenClaw | `openclaw` | `OPENCLAW_GATEWAY_TOKEN` | OpenClaw Gateway via WebSocket (requires `-u` for gateway URL) |

Custom aliases may also be configured in `~/.chatchain.yaml` (e.g. `deepseek`, `chatgpt`). Always run `chatchain -l` to see the full list.

## Config File

ChatChain supports a YAML config file (`~/.chatchain.yaml`) for persistent API keys, default models, and custom provider aliases. Priority: CLI flag > env var > config file.

```yaml
providers:
  deepseek:
    type: openai
    key: sk-deepseek-xxx
    url: https://api.deepseek.com/v1
    model: deepseek-chat
    system: "You are a helpful coding assistant"
```

With a config like this, `chatchain deepseek -m "hello"` works as a provider alias.

## Usage Examples

### Full workflow (recommended)

```bash
# 1. Discover providers
chatchain -l

# 2. Pick a provider, discover its models
chatchain -l openai

# 3. Send the message with a real model name
chatchain openai -M gpt-4o -m "What is the capital of France?"
```

### With system prompt

```bash
chatchain anthropic -M claude-sonnet-4-20250514 -s "You are a helpful coding assistant" -m "Explain async/await in JavaScript"
```

### Pipe content via stdin

```bash
echo "Summarize this text" | chatchain gemini -M gemini-2.0-flash -m -
```

Note: `-m -` (dash) reads the message from stdin.

### With temperature

```bash
chatchain openai -M gpt-4o -t 0.7 -m "Write a haiku about programming"
```

## Important Notes

- **NEVER guess provider or model names** — always run `chatchain -l` and `chatchain -l <provider>` first
- Always use `-m` for non-interactive mode (otherwise it opens an interactive TUI)
- Use `-m -` to read the message from stdin
- If no `-M` is specified, ChatChain will prompt for model selection interactively (avoid this in automation)
- API keys are read from environment variables by default; use `-k` only if the env var is not set
