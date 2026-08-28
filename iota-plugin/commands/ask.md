---
description: Ask another LLM a question via iota (usage: /iota:ask <provider> [model] <message>)
disable-model-invocation: true
allowed-tools:
  - Bash(iota *)
---

# iota Ask Command

Parse the user's arguments and call iota CLI.

## Arguments

`$ARGUMENTS` should be in the format: `<provider> [model] <message>`

## CRITICAL: Discover Real Providers and Models

**DO NOT guess or hardcode provider names or model names.** You MUST discover them first.

### Step 1: If provider is unclear or you're unsure it exists, list available providers:

```bash
iota -l
```

### Step 2: If no model is specified, list available models for the provider and pick a suitable one:

```bash
iota -l <provider>
```

Only use provider names and model names that appear in these outputs.

## Execution

1. Parse `$ARGUMENTS` to extract provider, optional model, and the message (everything after provider/model).
2. If the user didn't specify a model, run `iota -l <provider>` to discover available models, then pick a reasonable default from the list.
3. Run the command:

```bash
iota <provider> -M <model> -m "<message>"
```

4. Display the response to the user.

## Examples

- `/iota:ask openai What is 1+1` → first run `iota -l openai` to find models, then `iota openai -M <model-from-list> -m "What is 1+1"`
- `/iota:ask anthropic claude-sonnet-4-20250514 Explain monads` → `iota anthropic -M claude-sonnet-4-20250514 -m "Explain monads"`
- `/iota:ask deepseek Write a poem` → first run `iota -l` to verify `deepseek` exists, then `iota -l deepseek` to find models, then call with a real model name

## Input

$ARGUMENTS
