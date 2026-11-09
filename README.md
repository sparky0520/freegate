# Freegate

A local, OpenAI-compatible proxy for free-tier LLMs and local models.

Point any editor or agent (Cline, Continue, Aider, OpenCode, or custom chat UIs) at `http://127.0.0.1:8080/v1` once with `model: "auto"`. Freegate automatically routes, manages rate limits, and fails over across your free-tier keys and Ollama models without interrupting your work.

```
editor / agent  -->  http://127.0.0.1:8080/v1/chat/completions
                           |
                     [ Freegate ]  parse session, rank eligible tiers, stream SSE
                           |
         +-----------------+-----------------+
         |                 |                 |
       Groq              Gemini          Ollama (Local)
   (fails over on 429) (proactive limits) (offline fallback)
```

---

## Quick Start (< 10 lines)

```bash
# 1. Build binary
go build -o freegate ./cmd/freegate

# 2. Initialize config and set your key
./freegate init
export GROQ_API_KEY="your-groq-key"

# 3. Start gateway (point your editor to http://127.0.0.1:8080/v1 with model: "auto")
./freegate start
```

---

## Features

- **OpenAI-Compatible Drop-In**: Serves standard `/v1/chat/completions` (streaming SSE and non-streaming) and `/v1/models`.
- **Automatic Failover**: Seamlessly switches between providers and models on `429` (respects `Retry-After`), `5xx`, connection drops, and context-length errors.
- **Privacy-First Mode**: Tag providers with `trains_on_data = false`. When `private_mode` is enabled, prompts never cross into training providers even when quota is exhausted.
- **Session Stickiness**: Pinned conversations via `X-Session-Id` or system/user hash (30-minute TTL) keep tool-using agents on the same model mid-task.
- **Constraint-Aware Routing**: Inspects requests for `tools`, vision (`image_url`), and context size, filtering out unsupported models before attempting.
- **Proactive Limits & Circuit Breakers**: Parses rate-limit headers to cooldown models before hitting limits. Opens a 60s circuit breaker after 3 consecutive failures.
- **Local Security**: Binds `127.0.0.1`, validates `Host` headers against DNS-rebinding, and never logs prompts or API keys.

---

## CLI Commands

```bash
# Initialize ~/.config/freegate/config.toml and models.toml
freegate init

# Test all configured provider keys and endpoints
freegate test

# Run the gateway proxy server
freegate start [--addr 127.0.0.1:8080] [--private] [--debug]

# Display live ASCII table of bucket states, quotas, and cooldowns
freegate status [--addr 127.0.0.1:8080]

# List active virtual aliases and concrete model names
freegate models [--addr 127.0.0.1:8080]
```

---

## Configuration

Config files are stored in `~/.config/freegate/` (or specified via `--config`):

### `config.toml`
```toml
listen = "127.0.0.1:8080"
private_mode = false
unknown_model = "auto"
max_wait_seconds = 10
# local_api_key = "secret" # optional local authentication

[[provider]]
name = "groq"
base_url = "https://api.groq.com/openai/v1"
api_key_env = "GROQ_API_KEY"
trains_on_data = false

[[provider]]
name = "gemini"
base_url = "https://generativelanguage.googleapis.com/v1beta/openai"
api_key_env = "GEMINI_API_KEY"
trains_on_data = true

[[provider]]
name = "ollama"
base_url = "http://127.0.0.1:11434/v1"
local = true
trains_on_data = false
```

### Virtual Model Aliases
- `auto`: dynamically prioritizes Tier 1 (fast/small) for simple queries, and Tier 3 (high capability) for code and tool tasks.
- `fast`: prioritizes Tier 1 and Tier 2 models.
- `best-code`: prioritizes Tier 3 models.
- `local`: routes exclusively to local models (e.g. Ollama).

---

## Cross-Compilation

Compile a single static binary for any target OS and architecture:

```bash
# macOS Apple Silicon (M1/M2/M3/M4)
GOOS=darwin GOARCH=arm64 go build -o dist/freegate-darwin-arm64 ./cmd/freegate

# macOS Intel
GOOS=darwin GOARCH=amd64 go build -o dist/freegate-darwin-amd64 ./cmd/freegate

# Linux 64-bit (x86_64)
GOOS=linux GOARCH=amd64 go build -o dist/freegate-linux-amd64 ./cmd/freegate

# Linux ARM64 (Raspberry Pi, AWS Graviton)
GOOS=linux GOARCH=arm64 go build -o dist/freegate-linux-arm64 ./cmd/freegate

# Windows 64-bit
GOOS=windows GOARCH=amd64 go build -o dist/freegate-windows-amd64.exe ./cmd/freegate
```

---

## Testing

```bash
# Run unit tests with race detection
go test -race ./...

# Run static analysis
go vet ./...
```
