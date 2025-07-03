# llmbench

A tiny load-tester for OpenAI and Ollama like chat-completions endpoints, with optional streaming support.

Though, better fit for benchmarking than production use, it provides a simple way to measure response times, token usage, and throughput for LLM APIs. For example, testing a third-part provider other than a trusted company such as OpenAI or Anthropic, or possibly testing your own Ollama server's performance/throughput.

## Features

- Send concurrent requests to any `/v1/chat/completions` (OpenAI) or `/chat` (Ollama) endpoint
- Measure response latency, token usage, and tokens-per-second
- Approximate token counts for Ollama responses
- Optional **streaming** mode (SSE) for real-time output
- Optionally **store** each response and per-run metrics on disk via `--store-data`
- Automatically **unload** Ollama models after the benchmark with `--unload-model`

## Installation

```bash
# Requires Go 1.18+
go install go.codycody31.dev/llmbench@latest
```

## Usage

```bash
llmbench [flags]
```

### Flags

| Flag             | Default                              | Description                                      |
|------------------|--------------------------------------|--------------------------------------------------|
| `--base-url`     | `https://api.openai.com/v1`          | API base URL                                     |
| `--key`          | (env `LLM_API_KEY`)                  | Bearer token (not used by Ollama)                |
| `--style`        | `openai`                             | API style: `openai` or `ollama`                  |
| `--stream`       | `false`                              | Enable streaming (SSE) mode                      |
| `--runs`         | `100`                                | Total requests to send                           |
| `--concurrency`  | `0`                                  | Simultaneous requests (0 = same as `--runs`)     |
| `--max-tokens`   | `4096`                               | `max_tokens` per request (OpenAI only)           |
| `--model`        | `gpt-4o-mini`                        | Model ID                                         |
| `--prompt`       | `Explain the fundamental concepts...`| The user message to send                        |
| `--timeout`      | `60s`                                | HTTP client timeout (disabled in streaming mode) |
| `--unload-model` | `false`                              | Unload model after all runs complete (Ollama only) |
| `--data-dir`     | `./runs`                             | Directory to store responses and metrics           |
| `--store-data`   | `false`                              | Store responses and per-run metrics to `--data-dir`|

## Examples

```bash
# OpenAI style (non-streaming)
export LLM_API_KEY="sk-..."
llmbench --runs 50 --concurrency 10 --model gpt-4 --prompt "Hello, world!"

# Ollama style (non-streaming)
llmbench --style ollama \
         --base-url http://localhost:11434 \
         --runs 20 --concurrency 5 --model llama2

# OpenAI style (streaming)
export LLM_API_KEY="sk-..."
llmbench --style openai --stream \
         --runs 1 --model gpt-4 --prompt "Tell me a joke"

# Ollama style (streaming)
llmbench --style ollama --stream \
         --base-url http://localhost:11434 \
         --runs 1 --model llama2 --prompt "How are you today?"
```

## License

MIT License
