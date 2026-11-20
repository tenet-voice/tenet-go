# Tenet Go SDK

[![Go Reference](https://pkg.go.dev/badge/github.com/tenet-voice/tenet-go/v2.svg)](https://pkg.go.dev/github.com/tenet-voice/tenet-go/v2)

Send your agent's chat completions to a [Tenet](https://trytenet.ai) Project, keep your own provider as the failover, and fetch the Project's prompts.

## Install

```bash
go get github.com/tenet-voice/tenet-go/v2
```

## Quick start

Wrap the HTTP client of your existing OpenAI client. The client keeps its provider, key and model: they are what serves a request when Tenet cannot.

```go
import (
	"net/http"
	"os"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/tenet-voice/tenet-go/v2/tenet"
)

httpClient := tenet.WrapHTTPClient(http.DefaultClient, tenet.Config{
	Key:      os.Getenv("TENET_API_KEY"), // sk-tnt-…
	Project:  "support-agent",
	Failover: true,
})
client := openai.NewClient(
	option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
	option.WithHTTPClient(httpClient),
)
```

Every chat completion, streaming included, now goes to Tenet as model `support-agent`, which serves the Project's `production` version. Your messages, tools and prompt pass through unchanged.

## Choosing a version

`Tag` picks which Project version serves:

| `Tag` | Serves |
|-------|--------|
| `""` | `production` |
| `latest` | the newest version |
| a branch name | that branch's newest version |
| a custom tag | the version you tagged |
| `v12` | exactly version 12 |

Tenet's staging endpoint (set `BaseURL`) serves every version under all of its tags. Production serves only the released version, as `production`.

## Prompts

Store your prompt in the Project's prompt store, tag a version `production`, and fetch it at the start of each call. Tenet never renders the text, so templates (Jinja or anything else) stay yours to fill:

```go
prompt, err := tenet.GetPrompt(ctx, httpClient, config, "") // "" means production
if errors.Is(err, tenet.ErrPromptNotFound) {
	// nothing is tagged production yet
}
system := render(prompt.Text, vars) // prompt.Version says which version you got
```

`GetPrompt` makes one request. If your agent must start when Tenet is unreachable, keep your own copy of the last prompt you fetched.

## Failover

Tenet answers with the Project's fallback model when the requested version fails. With `Failover: true`, the SDK also handles Tenet itself being unreachable or returning a 5xx by sending your original request, unchanged, to your provider:

```
Request → Tenet (version, or Tenet's fallback)
            ↓ network error or 5xx
       Your provider, original request
```

A 4xx (bad key, unknown Project or tag, invalid request) is returned as is. Failovers are reported in the background to `POST /v1/telemetry`.

Requests other than chat completions (embeddings, audio, …) go to your provider directly.

## Sessions

Session details are recorded with the traffic Tenet captures, so calls group into sessions in Studio. They never change which version serves.

```go
tenet.SetSessionID(httpClient, "call_123")
tenet.SetSessionTags(httpClient, []string{"beta"})
tenet.SetAgentID(httpClient, "front-desk")
```

They can also be set in `Config`.

## Attribution

`OnAttribution` is called once per chat completion:

```go
OnAttribution: func(a tenet.Attribution) {
	// a.Version:      "support-agent:v12", or "" when Tenet's fallback served
	// a.FallbackUsed: Tenet served with the Project's fallback model
	// a.ServedDirect: the SDK failed over to your provider
},
```

For streaming it fires when the response headers arrive. It runs inside the request, so make it safe for concurrent use if the client is shared.

## Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `Key` | `string` | required | Tenet key (`sk-tnt-…`) |
| `Project` | `string` | required | Tenet Project ID |
| `Tag` | `string` | `production` | Version tag to serve |
| `BaseURL` | `string` | `https://inference.trytenet.ai` | Tenet endpoint |
| `Failover` | `bool` | `false` | Send to your provider when Tenet cannot answer |
| `Timeout` | `time.Duration` | wrapped client's | HTTP client timeout |
| `AgentID`, `SessionID`, `SessionTags` | | empty | Recorded with captured traffic |
| `OnAttribution` | `func(Attribution)` | `nil` | Called once per chat completion |

## Without the SDK

Tenet speaks the OpenAI Chat Completions API. Any OpenAI-compatible client works with base URL `https://inference.trytenet.ai/v1`, your `sk-tnt-…` key as the API key, and model `<project>[:<tag>]`. The SDK adds client-side failover, prompts and attribution.

## Upgrading from v1

- Module path is `github.com/tenet-voice/tenet-go/v2`.
- `TenetKey` is `Key` and must be a `sk-tnt-…` key; `ProxyURL` is `BaseURL`.
- Add `Project` (and `Tag` if not `production`). The model your client sends is kept only for failover.
- `Attribution` has `Version`, `FallbackUsed`, `ServedDirect`; `Mode`, `ServedVariant` and `MatchedProfile` are gone.
- `Clear*` helpers are gone; call `Set*` with an empty value.

## License

MIT
