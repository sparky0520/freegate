# LLM Gateway: v1 Build Plan

Working name: `freegate` (rename freely). Language: **Go**. Target: a single small binary people run on their own machine.

## 0. How to use this plan (for the coding agent)

- Work **one milestone at a time** (section 10). After each milestone, stop, run the acceptance check, and wait for review. Do not jump ahead.
- Prefer the Go standard library. Allowed extra dependencies: one TOML parser (`github.com/BurntSushi/toml` or `github.com/pelletier/go-toml/v2`). Nothing else without asking.
- Do **not** use typed OpenAI SDK structs in the request path. This is a proxy: it must preserve unknown fields. Parse bodies as `map[string]json.RawMessage` and only touch the fields named in this plan.
- Never log API keys or prompt contents. Never add features from the "Non-goals" list.

## 1. What we're building

A **local, OpenAI-compatible proxy**. The user points any app that accepts a custom API base URL (Cline, Continue, Aider, OpenCode, chat UIs) at `http://127.0.0.1:8080/v1` once. The gateway then chooses which provider and model answers each request, using the user's own free-tier keys and optionally a local model (Ollama). When one provider runs out of quota, it moves to another without the app noticing.

Problem it solves: people who don't want to pay for AI either need strong local hardware or must manage rate limits across many tools.

Success looks like: an editor agent runs for an hour against `model: "auto"` and the user never sees a rate-limit error, and `freegate status` shows traffic moving between providers.

## 2. Hard constraints (read first)

1. **One API key per provider per user.** No rotating multiple accounts of the same provider. No account creation, no IP spoofing, no proxy rotation, no unofficial or scraped endpoints (no GPT4Free-style sources). Quota is per account/project, so extra keys from one account add nothing anyway. Refuse to add any of this.
2. **Legitimate axes only:** (a) different providers, (b) different models within a provider (quotas are per model), (c) a local model.
3. **Privacy is a first-class feature.** Some free tiers may train on prompts. Every provider is tagged `trains_on_data`. A **private mode** only routes to providers where this is false (local models count as false).
4. **Local only by default.** Bind `127.0.0.1`. Keys come from environment variables named in the config. No telemetry.
5. Quotas, model names, and free-tier terms **change often**. Nothing about them is hardcoded in logic; it lives in config and the embedded metadata file, and the code learns limits from response headers where possible.

## 3. Architecture

```
app/editor  ->  http://127.0.0.1:8080/v1/chat/completions
                         |
                   [ server ]  parse request, find session, estimate tokens
                         |
                   [ router ]  filter buckets -> order -> try one -> failover
                         |
             [ provider client ]  rewrite model, set auth, forward, stream back,
                         |        read x-ratelimit-* headers
        Groq / Gemini / OpenRouter / Ollama (all OpenAI-compatible)
```

All v1 providers speak the OpenAI chat-completions format, so **no request/response translation** is needed.

## 4. Core concepts

- **Provider**: `name`, `base_url`, `api_key_env` (empty for local), `trains_on_data` (bool), `local` (bool).
- **Bucket**: one `(provider, model_id)` pair. This is the unit of quota. Fields:
  - static (from metadata): `context_window`, `supports_tools`, `supports_vision`, `tier` (1 = small/fast, 2 = mid, 3 = strongest available)
  - dynamic: `cooldown_until`, `remaining_requests`, `remaining_tokens`, `consecutive_failures`, `breaker_open_until`, `last_error`
- **Alias**: a virtual model name that apps can request, mapped to an ordered list of buckets plus optional constraints. Built-ins: `auto`, `fast`, `best-code`, `local`. Users may define more in config.
- **Session**: a stable key for one conversation, so a tool-using agent isn't bounced between models mid-task. Key = `X-Session-Id` header if present, otherwise a hash of (system prompt + first user message).

## 5. Routing spec (deterministic, no LLM calls)

For each request:

1. **Parse**: `model` (alias or `provider/model`), `stream`, `tools` present, image content present, estimated prompt tokens (`len(json_body_bytes)/4` is fine for v1), and `max_tokens` if set. Unknown model names (some apps hardcode e.g. `gpt-4o`) are treated as `auto` (config: `unknown_model = "auto"`).
2. **Eligible buckets**: start from the alias's list (or all buckets for `auto`), then drop any that fail a hard filter:
   - provider has a key (or is local)
   - not in cooldown, breaker not open
   - `context_window >= estimated_tokens + max_tokens (or 1024)`
   - `supports_tools` if the request has `tools`; `supports_vision` if it has images
   - private mode on: provider must have `trains_on_data = false`
3. **Order** the eligible buckets:
   - If a sticky bucket for this session is still eligible, put it first.
   - For `auto`: short and simple (below ~2k estimated tokens, no tools, no code fences) prefers tier 1 then up; long, code-heavy, or tool-using prefers tier 3 then down. For other aliases, use the alias's own order.
   - Tie-break by most `remaining_requests`/`remaining_tokens`, then by name for determinism.
4. **Attempt loop** (max 4 attempts, overall deadline configurable, default 120s):
   - Rewrite `model` to the bucket's real model ID; replace `Authorization` with the provider's key; forward.
   - **429**: set `cooldown_until` from `Retry-After` (seconds or HTTP date) or the reset header, else 30s. Try the next bucket.
   - **5xx or connection error/timeout before any response byte**: increment `consecutive_failures`; 3 in a row opens a circuit breaker for 60s. Try the next bucket.
   - **401/403**: mark the *provider* disabled with a clear reason; try the next bucket.
   - **404 / model not found**: mark the bucket disabled; trigger a model refresh; try the next bucket.
   - **400 with a context-length error**: try a bucket with a larger `context_window`; any other 400 is returned to the client as is (it's the request's fault).
   - **Success**: record the sticky bucket for the session (TTL 30 min) and update dynamic fields from rate-limit headers.
5. **Wait vs switch**: if no bucket is eligible only because of cooldowns and the soonest `cooldown_until` is within `max_wait` (default 10s), wait (respect client cancellation) instead of failing. Otherwise return `429` in OpenAI error format with a `Retry-After` header.
6. **Proactive limits**: after each response, parse `x-ratelimit-remaining-requests`, `x-ratelimit-remaining-tokens`, and `x-ratelimit-reset-*` if present (formats vary; parse as a Go duration string and ignore on failure). If remaining hits 0, set the cooldown to the reset time before ever receiving a 429.
7. **Private-mode failover rule**: failover must never cross into a provider the mode forbids. If nothing eligible remains, fail rather than leak.

### Streaming rules

- For `stream: true`, copy SSE chunks to the client and call `Flush()` after each chunk. Use `http.NewResponseController(w)` or `http.Flusher`.
- Failover is only allowed **before the first byte reaches the client**. After that, if upstream fails, send one final SSE event `data: {"error": {...}}` then close; do not splice another model's output into the stream.
- A client disconnect must cancel the upstream request via the request context.
- Do not buffer tool-call streams in v1. (Possible later option: buffer until the tool call is complete.)

## 6. HTTP API

- `POST /v1/chat/completions`: streaming and non-streaming, as above.
- `GET /v1/models`: the alias names plus the concrete `provider/model` names currently usable.
- `GET /healthz`: `ok`.
- `GET /status`: JSON snapshot of all buckets (state, remaining, cooldown, last error, disabled reasons). Used by the CLI.
- Optional local auth: if `local_api_key` is set in config, require `Authorization: Bearer <it>`; otherwise accept any value (apps insist on sending a key).
- Security: reject requests whose `Host` header is not `localhost`, `127.0.0.1`, or `[::1]` (DNS-rebinding protection). Limit request bodies to 20 MB.
- Timeouts: upstream connect 10s, response-header timeout 60s, streaming idle timeout 120s.
- Errors returned to clients use the OpenAI error shape (`{"error": {"message", "type", "code"}}`) plus an `X-Freegate-Attempts` header such as `groq/llama-3.3-70b:429,gemini/flash:ok`.

## 7. Config and model metadata

Config file: `~/.config/freegate/config.toml` (use `os.UserConfigDir()`).

```toml
listen = "127.0.0.1:8080"
private_mode = false
unknown_model = "auto"
max_wait_seconds = 10

[[provider]]
name = "groq"
base_url = "https://api.groq.com/openai/v1"
api_key_env = "GROQ_API_KEY"
trains_on_data = false

[[provider]]
name = "gemini"
base_url = "https://generativelanguage.googleapis.com/v1beta/openai"
api_key_env = "GEMINI_API_KEY"
trains_on_data = true   # free tier may use prompts; verify current terms

[[provider]]
name = "openrouter"
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_API_KEY"
trains_on_data = true   # varies by model; verify

[[provider]]
name = "ollama"
base_url = "http://127.0.0.1:11434/v1"
local = true
trains_on_data = false
```

Base URLs above are **examples to verify against each provider's docs**.

**Model metadata** (`models.default.toml`, embedded with `go:embed`, user-overridable at `~/.config/freegate/models.toml`): one entry per chat model the router may use, with `provider`, `id`, `tier`, `supports_tools`, `supports_vision`, `context_window` (fallback if the API doesn't report it). Only models with an entry are routable (this also keeps speech, embeddings, and safety models out). Seed it with a few entries per provider and mark them `# verify`; model names change.

**Discovery**: at startup and every 6 hours, call `GET {base_url}/models` for each configured provider. Use it to (a) drop metadata entries whose model no longer exists, (b) fill in `context_window` when the API reports it, and (c) log new unclassified models at info level. Discovery failure must never stop the gateway.

## 8. CLI (standard library `flag`, subcommands)

- `freegate init`: write an example config and models file.
- `freegate start [--addr] [--private] [--debug]`: run the proxy in the foreground.
- `freegate status`: print a table from `/status` (bucket, state, remaining, cooldown, last error).
- `freegate models`: list aliases and routable buckets.
- `freegate test`: send a tiny request to each configured provider and report which keys work.

Logging: `log/slog`, JSON or text. At info level log one line per request (alias, chosen bucket, attempts, latency, status), never bodies or keys. `--debug` may log headers with `Authorization` redacted.

## 9. Project layout and ownership

```
cmd/freegate/main.go          agent
internal/config/              agent   (load config, embedded metadata, validation)
internal/provider/            agent   (HTTP forwarding, header rewrite, rate-limit header parsing, discovery)
internal/registry/            human   (Bucket state machine: cooldowns, breaker, remaining) + agent tests
internal/router/              human   (eligibility, ordering, attempt loop, stickiness) + agent tests
internal/server/              agent   (handlers, SSE streaming, Host check, status)
internal/cli/                 agent
models.default.toml           agent (seed) / human (curate)
testdata/                     agent
```

## 10. Milestones and acceptance checks

**M1: Passthrough (non-streaming).** One hardcoded provider; forward `POST /v1/chat/completions`, rewrite auth, copy status, headers, and body back.
*Check:* a `curl` to localhost returns a real model reply; removing the Authorization header upstream returns the provider's 401 unchanged.

**M2: Streaming passthrough.** `stream: true` flows chunk by chunk.
*Check:* `curl -N` shows chunks arriving incrementally; Ctrl-C cancels the upstream request (verify with a slow fake upstream in a test).

**M3: Config, providers, buckets, discovery.** Load config and metadata, build buckets, expose `/v1/models`, `/status`, `freegate status`.
*Check:* with two providers configured, `status` lists their buckets; an unreachable provider is reported, not fatal.

**M4: Router v1.** Eligibility filter, ordering, attempt loop, 429/5xx/401/404/400 handling, cooldowns from `Retry-After` and rate-limit headers, wait-vs-switch.
*Check (unit, with `httptest` fakes):* first fake returns 429 with `Retry-After: 5`, second succeeds; the client sees one success; the first bucket shows a cooldown of about 5s.

**M5: Sessions and constraints.** Stickiness, tools/vision/context filters, private mode, unknown-model fallback to `auto`.
*Check:* a tool-using request never routes to a bucket with `supports_tools = false`; private mode never reaches a `trains_on_data = true` provider even when everything else is exhausted.

**M6: Polish.** `init`, `start`, `models`, `test`, README (quick start in under 10 lines), and cross-compile instructions for macOS, Linux, and Windows (`GOOS`/`GOARCH`).
*Check:* a fresh machine can go from download to a working request in under 5 minutes.

## 11. Testing

- Table-driven unit tests for routing decisions (pure functions where possible).
- A fake-provider test harness using `httptest.Server` that can return: success, SSE stream, 429 with headers, 5xx, slow response, mid-stream failure, 401, 404, context-length 400.
- Run `go test -race ./...` and `go vet ./...` in every milestone.
- No test may call a real provider; one optional script (`scripts/smoke.sh`) may, using the user's own keys.

## 12. Definition of done for v1

A real agent (for example Cline or OpenCode) is pointed at `http://127.0.0.1:8080/v1` with `model: auto` and used for a 30+ minute session across at least two providers. Rate limits are hit and handled; there are no client-visible failures except the documented ones (all buckets exhausted beyond `max_wait`; mid-stream upstream failure). `freegate status` shows what happened.

## 13. Non-goals for v1

Multiple accounts per provider, any evasion of provider limits, a learned or LLM-based router, a web dashboard, response caching, embeddings and audio endpoints, non-OpenAI-format providers (native Anthropic, etc.), persistent state across restarts, multi-user auth, a hosted service.

## 14. Open items to verify before or during M3

- Exact current base URLs and OpenAI-compatibility behavior for each provider.
- Which models each provider currently offers for free, and their tool-calling support.
- Current free-tier data-use terms (for the `trains_on_data` flags).
- A final project name.
