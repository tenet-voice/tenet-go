# Tenet Go SDK

[![Go Reference](https://pkg.go.dev/badge/github.com/tenet-voice/tenet-go.svg)](https://pkg.go.dev/github.com/tenet-voice/tenet-go)

Route OpenAI-compatible LLM calls through [Tenet](https://trytenet.ai) for production observability, A/B model routing, and automatic failover — in one line.

## Install

```bash
go get github.com/tenet-voice/tenet-go
```

## Quick start

Wrap your existing HTTP client. Everything else stays the same.

```go
import (
	"net/http"
	"os"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/tenet-voice/tenet-go/tenet"
)

client := openai.NewClient(
	option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
	option.WithHTTPClient(tenet.WrapHTTPClient(http.DefaultClient, tenet.Config{
		TenetKey: os.Getenv("TENET_API_KEY"),
		Failover: true,
	})),
)
```

That's it. All requests — including streaming — are transparently proxied through Tenet. Your code, types, and error handling don't change.

## A/B routing

Tenet supports per-session sticky model routing for A/B tests. Attach a session ID and the proxy consistently routes that session to the same model variant:

```go
// Sticky routing — same session always hits the same variant
tenet.SetSessionID(httpClient, "caller_123")

// Back to weighted-random
tenet.ClearSessionID(httpClient)
```

Session IDs are hashed (FNV-1a) against configured variant weights. Without a session ID, each request is independently routed by weight.

You can also attach cohort tags to a session (e.g. for cohort-based analysis or routing):

```go
tenet.SetSessionTags(httpClient, []string{"beta", "internal"})

tenet.ClearSessionTags(httpClient)
```

Both `SessionID` and `SessionTags` can also be set at construction time via `Config`.

## Failover

With `Failover: true`, the SDK automatically falls back to calling your provider directly if the proxy is unreachable or returns a 5xx:

```
Request → Tenet Proxy → Provider
              ↓ (5xx or network error)
         Direct to Provider
```

4xx errors (auth failures, bad requests) are returned as-is — they indicate a real problem, not a proxy issue.

Failover events are reported asynchronously to `POST /v1/telemetry` for monitoring.

## Attribution

Set `OnAttribution` to learn how each request was served — without coupling your code to Tenet's response-header names. The callback fires once per request:

```go
client := tenet.WrapHTTPClient(http.DefaultClient, tenet.Config{
	TenetKey: os.Getenv("TENET_API_KEY"),
	Failover: true,
	OnAttribution: func(a tenet.Attribution) {
		// a.Mode:           "passthrough" | "replacement" | "managed"
		// a.ServedVariant:  which variant served the request
		// a.MatchedProfile: which model profile matched (empty if none)
		// a.FallbackUsed:   the proxy fell through to a backup variant
		// a.ServedDirect:   the SDK bypassed Tenet (client-side failover)
		log.Printf("served by %s (%s)", a.ServedVariant, a.Mode)
	},
})
```

The proxy reports `Mode`, `ServedVariant`, `MatchedProfile`, and `FallbackUsed` via `X-Tenet-*` response headers; the SDK parses them for you. On **client-side failover** (proxy unreachable or 5xx with `Failover: true`), the SDK didn't reach Tenet at all, so it reports `Attribution{ServedDirect: true}` with the other fields empty.

For **streaming**, the callback fires when the response headers arrive (stream start), before the first token.

Notes:

- The callback runs **synchronously inside the request**. If you share one client across goroutines, make your callback concurrency-safe.
- The callback carries no request context — scope **one wrapped client per caller** (each with its own closure) when you need to correlate attribution back to a specific call.
- A `4xx`, or a `5xx` without failover, reaches the callback with whatever headers are present — usually an empty `Mode`. Treat empty `Mode` as "unknown / proxy error," not a served mode.

## Streaming

Streaming works transparently. The proxy forwards SSE chunks as they arrive from the upstream provider — no buffering, no re-encoding:

```go
stream := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
	Model:    "gpt-4o",
	Messages: []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage("Hello"),
	},
})
for stream.Next() {
	chunk := stream.Current()
	// chunks arrive in real-time, same as without Tenet
}
```

## Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `TenetKey` | `string` | required | API key for the Tenet proxy |
| `SessionID` | `string` | `""` | Session identifier for sticky A/B routing |
| `SessionTags` | `[]string` | `nil` | Cohort tags attached to the session |
| `ProxyURL` | `string` | `https://inference.trytenet.ai` | Proxy endpoint (override for self-hosted or staging) |
| `Failover` | `bool` | `false` | Fall back to direct provider on proxy failure |
| `Timeout` | `time.Duration` | `0` (no timeout) | HTTP client timeout |
| `OnAttribution` | `func(Attribution)` | `nil` | Called once per request with how the proxy served it |

## How it works

`WrapHTTPClient` replaces the transport on your `*http.Client`. On each request, the transport:

1. Buffers the request body (for potential failover replay)
2. Rewrites the URL to point at the Tenet proxy
3. Injects `X-Tenet-Key` (auth) and `X-Provider-URL` (original destination)
4. Optionally injects `X-Tenet-Session-Id` and `X-Tenet-Session-Tags` for sticky routing
5. Sends the request through the proxy
6. On 5xx or network error (with failover enabled), replays to the original URL
7. Parses the proxy's `X-Tenet-*` response headers into an `Attribution` and invokes `OnAttribution` (if set)

Your provider credentials (`Authorization` header) pass through untouched.

## Works with any OpenAI-compatible provider

The SDK proxies raw HTTP — it doesn't parse or transform requests. Any provider that speaks the OpenAI API format works:

- OpenAI
- Azure OpenAI
- Groq
- Together AI
- Fireworks
- Any OpenAI-compatible endpoint

## License

MIT
