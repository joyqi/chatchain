---
name: chatchain
description: >
  Send questions to other LLM models — GPT-4o, GPT-4, o1, o3, Gemini, Claude (via API), etc.
  Use this skill whenever the user wants to ask, query, chat with, or get answers from another AI model,
  including: "ask GPT...", "let Gemini explain...", "what does Claude think about...",
  "compare answers from different models", "get a second opinion from another LLM",
  or any request that involves calling OpenAI, Anthropic, Gemini, or Vertex AI models.
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

## Providers and Environment Variables

| Provider | Subcommand | Env Var | Notes |
|----------|-----------|---------|-------|
| OpenAI | `openai` | `OPENAI_API_KEY` | GPT models |
| Anthropic | `anthropic` | `ANTHROPIC_API_KEY` | Claude models |
| Gemini | `gemini` | `GOOGLE_API_KEY` | Gemini models |
| Vertex AI | `vertexai` | — | Uses Google Cloud ADC |
| OpenAI Responses | `openresponses` | `OPENAI_API_KEY` | OpenAI Responses API |

## Key Flags

| Flag | Description |
|------|-------------|
| `-M, --model <model>` | Specify model (e.g., `gpt-4o`, `claude-sonnet-4-20250514`) |
| `-m, --message <msg>` | Non-interactive mode: send a single message and exit |
| `-s, --system <prompt>` | Set system prompt |
| `-t, --temperature <val>` | Set temperature (0.0–2.0) |
| `-k, --key <key>` | API key (overrides env var) |
| `-u, --url <url>` | Custom API base URL |
| `-v, --verbose` | Show raw API responses |

## Usage

### Non-interactive single question

```bash
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

### With temperature

```bash
chatchain openai -M gpt-4o -t 0.7 -m "Write a haiku about programming"
```

## Important Notes

- Always use `-m` for non-interactive mode (otherwise it opens an interactive TUI)
- The `-m` flag with value `-` reads the message from stdin
- If no `-M` is specified, ChatChain will prompt for model selection interactively (avoid this in automation)
- API keys are read from environment variables by default; use `-k` only if the env var is not set
