# ChatChain

A lightweight, cross-platform AI chat CLI built with Go. Supports multiple providers, streaming responses, and an interactive terminal UI.

## Features

- **Multi-provider** — OpenAI, OpenAI Responses API, Anthropic, Gemini and Vertex AI, with custom base URL support
- **Interactive model selection** — arrow-key navigation with filtering
- **Streaming responses** — real-time token output with loading spinner
- **Non-interactive mode** — single message in, response out, pipe-friendly
- **Conversation history** — full context maintained within a session
- **System prompt** — set via flag or interactive input
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

## Usage

```bash
chatchain [openai|anthropic|gemini|vertexai|openresponses] [flags]
```

### Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--key` | `-k` | API key (or set via env var) |
| `--url` | `-u` | Custom base URL |
| `--model` | `-m` | Model name (skip interactive selection) |
| `--temperature` | `-t` | Sampling temperature, 0.0-2.0 (omit to use provider default) |
| `--chat` | `-c` | Send a single message and print the response (non-interactive) |
| `--system` | `-s` | System prompt (omit value for interactive input) |

### Environment Variables

| Variable | Provider |
|----------|----------|
| `OPENAI_API_KEY` | OpenAI / OpenResponses |
| `ANTHROPIC_API_KEY` | Anthropic |
| `GOOGLE_API_KEY` | Gemini / Vertex AI |

### Examples

```bash
# Interactive model selection
chatchain openai -k sk-xxx

# Specify model directly
chatchain openai -k sk-xxx -m gpt-4o

# Use Anthropic
chatchain anthropic -m claude-sonnet-4-20250514

# Use Gemini
chatchain gemini -m gemini-2.5-flash

# Use Vertex AI (with custom endpoint)
chatchain vertexai -u https://your-proxy.com/api/vertex-ai -m gemini-2.5-flash -c "Hello"

# Use OpenAI Responses API
chatchain openresponses -m gpt-4o -c "Hello"

# With system prompt
chatchain openai -m gpt-4o -s 'You are a helpful translator' -c "Translate to French: hello"

# Interactive system prompt input (prompts System> after model selection)
chatchain openai -m gpt-4o -s

# Non-interactive mode (requires -m)
chatchain openai -m gpt-4o -c "Explain quicksort in one paragraph"

# Adjust temperature
chatchain anthropic -m claude-sonnet-4-20250514 -t 0.5 -c "Write a haiku"

# Custom API endpoint
chatchain openai -u https://your-proxy.com/v1 -k sk-xxx
```

## Project Structure

```
chatchain/
├── main.go              # Entry point
├── cmd/
│   └── root.go          # CLI definition (cobra)
├── chat/
│   ├── chat.go          # Chat loop, model selection, spinner
│   └── styles.go        # Terminal style definitions
└── provider/
    ├── provider.go      # Provider interface
    ├── openai.go          # OpenAI Chat Completions implementation
    ├── openresponses.go   # OpenAI Responses API implementation
    ├── anthropic.go       # Anthropic implementation
    ├── gemini.go          # Gemini implementation
    └── vertexai.go        # Vertex AI implementation
```

## Dependencies

- [cobra](https://github.com/spf13/cobra) — CLI framework
- [fatih/color](https://github.com/fatih/color) — Terminal styling
- [promptui](https://github.com/manifoldco/promptui) — Interactive prompts
- [spinner](https://github.com/briandowns/spinner) — Loading spinners
- [openai-go](https://github.com/openai/openai-go) — OpenAI SDK
- [anthropic-sdk-go](https://github.com/anthropics/anthropic-sdk-go) — Anthropic SDK
- [go-genai](https://github.com/googleapis/go-genai) — Google Gemini SDK

## License

MIT
