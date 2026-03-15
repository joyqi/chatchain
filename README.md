# ChatChain

A lightweight, cross-platform AI chat CLI built with Go. Supports multiple providers, streaming responses, and an interactive terminal UI.

## Features

- **Multi-provider** — OpenAI and Anthropic, with custom base URL support
- **Interactive model selection** — arrow-key navigation with filtering
- **Streaming responses** — real-time token output with loading spinner
- **Conversation history** — full context maintained within a session
- **Styled terminal output** — color-coded prompts via [Charm](https://charm.sh) ecosystem

## Install

```bash
go install github.com/joyqi/chatchain@latest
```

Or build from source:

```bash
git clone https://github.com/joyqi/chatchain.git
cd chatchain
go build -o chatchain .
```

## Usage

```bash
chatchain [openai|anthropic] [flags]
```

### Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--key` | `-k` | API key (or set via env var) |
| `--url` | `-u` | Custom base URL |
| `--model` | `-m` | Model name (skip interactive selection) |

### Environment Variables

| Variable | Provider |
|----------|----------|
| `OPENAI_API_KEY` | OpenAI |
| `ANTHROPIC_API_KEY` | Anthropic |

### Examples

```bash
# Interactive model selection
chatchain openai -k sk-xxx

# Specify model directly
chatchain openai -k sk-xxx -m gpt-4o

# Use Anthropic
chatchain anthropic -m claude-sonnet-4-20250514

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
│   └── styles.go        # lipgloss style definitions
└── provider/
    ├── provider.go      # Provider interface
    ├── openai.go        # OpenAI implementation
    └── anthropic.go     # Anthropic implementation
```

## Dependencies

- [cobra](https://github.com/spf13/cobra) — CLI framework
- [huh](https://github.com/charmbracelet/huh) — Interactive prompts & spinner
- [lipgloss](https://github.com/charmbracelet/lipgloss) — Terminal styling
- [openai-go](https://github.com/openai/openai-go) — OpenAI SDK
- [anthropic-sdk-go](https://github.com/anthropics/anthropic-sdk-go) — Anthropic SDK

## License

MIT
