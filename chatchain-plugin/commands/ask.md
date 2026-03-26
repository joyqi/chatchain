---
description: Ask another LLM a question via ChatChain (usage: /chatchain:ask <provider> [model] <message>)
disable-model-invocation: true
allowed-tools:
  - Bash(chatchain *)
---

# ChatChain Ask Command

Parse the user's arguments and call ChatChain CLI.

## Arguments

`$ARGUMENTS` should be in the format: `<provider> [model] <message>`

## CRITICAL: Discover Real Providers and Models

**DO NOT guess or hardcode provider names or model names.** You MUST discover them first.

### Step 1: If provider is unclear or you're unsure it exists, list available providers:

```bash
chatchain -l
```

### Step 2: If no model is specified, list available models for the provider and pick a suitable one:

```bash
chatchain -l <provider>
```

Only use provider names and model names that appear in these outputs.

## Execution

1. Parse `$ARGUMENTS` to extract provider, optional model, and the message (everything after provider/model).
2. If the user didn't specify a model, run `chatchain -l <provider>` to discover available models, then pick a reasonable default from the list.
3. Run the command:

```bash
chatchain <provider> -M <model> -m "<message>"
```

4. Display the response to the user.

## Examples

- `/chatchain:ask openai What is 1+1` → first run `chatchain -l openai` to find models, then `chatchain openai -M <model-from-list> -m "What is 1+1"`
- `/chatchain:ask anthropic claude-sonnet-4-20250514 Explain monads` → `chatchain anthropic -M claude-sonnet-4-20250514 -m "Explain monads"`
- `/chatchain:ask deepseek Write a poem` → first run `chatchain -l` to verify `deepseek` exists, then `chatchain -l deepseek` to find models, then call with a real model name

## Input

$ARGUMENTS
