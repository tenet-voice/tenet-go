# Tenet Go SDK

Route OpenAI-compatible LLM calls through [Tenet](https://trytenet.ai) for production voice agent observability.

## Install

```bash
go get github.com/tenet-ai/go-sdk
```

## Quick Start

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/tenet-ai/go-sdk/tenet"
)

client := openai.NewClient(
	option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
	option.WithHTTPClient(tenet.WrapHTTPClient(
		http.DefaultClient,
		tenet.Config{
			TenetKey: os.Getenv("TENET_API_KEY"),
			Failover: true,
		},
	)),
)

resp, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
	Model: "gpt-4o",
	Messages: []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage("Say hello in one word."),
	},
})
if err != nil {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
fmt.Println(resp.Choices[0].Message.Content)
```

## Configuration

| Option | Default | Description |
|---|---|---|
| `TenetKey` | required | Tenet API key |
| `Failover` | `true` | Fall back to direct provider on proxy failure |
| `ProxyURL` | `https://inference.trytenet.ai` | Custom proxy URL (self-hosted) |
| `Timeout` | `5s` | Connection timeout |

## How It Works

`WrapHTTPClient()` wraps your HTTP client transport. Requests are routed through Tenet's inference proxy, which captures them for scoring and analysis. If the proxy is unreachable, the SDK falls back to calling your provider directly — your agent never goes silent.

## License

MIT
