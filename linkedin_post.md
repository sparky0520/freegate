# Introducing freegate 🚀

Just open-sourced a project that's been a game-changer for my AI workflow:

**freegate** - A local, OpenAI-compatible proxy for free-tier LLMs and local models.

💡 **Problem it solves:** Managing rate limits across multiple free-tier AI providers is tedious. Constantly hitting quotas and dealing with different provider APIs breaks focus.

🔑 **How it works:** Point any editor or agent at `http://127.0.0.1:8080/v1` with `model: "auto"`, and freegate automatically:
- Routes requests across Groq, Gemini, and Ollama based on availability
- Handles rate-limit errors with proactive cooldowns and circuit breakers
- Sticks to the same model for session continuity (30-min TTL)
- Never sends prompts to training-data providers when private mode is on
- Binds to localhost only - privacy first

✨ **Key features:**
- OpenAI-compatible drop-in (`/v1/chat/completions` and `/v1/models`)
- Automatic failover across providers on 429/5xx errors
- Privacy-first mode - prompts never cross into training providers
- Session stickiness for tool-using agents
- Constraint-aware routing (tools, vision, context size)
- Proactive limits and circuit breakers
- Local security - binds 127.0.0.1, validates Host headers

🛠️ **Quick start:**
```bash
go build -o freegate ./cmd/freegate
./freegate init
export GROQ_API_KEY="your-groq-key"
./freegate start
```

Then point your editor/agent at `http://127.0.0.1:8080/v1` with `model: "auto"`.

🔗 **GitHub:** https://github.com/sparky0520/freegate

#gateway #AI #LLM #OpenSource #Productivity #Freegate #Go #MachineLearning