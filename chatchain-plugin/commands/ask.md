---
description: Ask another LLM a question via ChatChain (usage: /chatchain:ask <provider> <model> <message>)
disable-model-invocation: true
allowed-tools:
  - Bash(chatchain *)
---

# ChatChain Ask Command

Parse the user's arguments and call ChatChain CLI.

## Arguments

`$ARGUMENTS` should be in the format: `<provider> [model] <message>`

Supported providers: `openai`, `anthropic`, `gemini`, `vertexai`, `openresponses`, or any custom alias defined in `~/.chatchain.yaml`

If no model is specified, use these defaults:
- openai: `gpt-4o`
- anthropic: `claude-sonnet-4-20250514`
- gemini: `gemini-2.0-flash`
- vertexai: `gemini-2.0-flash`
- openresponses: `gpt-4o`

## Execution

1. Parse `$ARGUMENTS` to extract provider, optional model, and the message (everything after provider/model).
2. Run the command:

```bash
chatchain <provider> -M <model> -m "<message>"
```

3. Display the response to the user.

## Examples

- `/chatchain:ask openai What is 1+1` → `chatchain openai -M gpt-4o -m "What is 1+1"`
- `/chatchain:ask anthropic claude-sonnet-4-20250514 Explain monads` → `chatchain anthropic -M claude-sonnet-4-20250514 -m "Explain monads"`
- `/chatchain:ask gemini gemini-2.0-flash Write a poem` → `chatchain gemini -M gemini-2.0-flash -m "Write a poem"`

## Input

$ARGUMENTS
