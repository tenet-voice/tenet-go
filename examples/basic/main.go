package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/tenet-voice/tenet-go/tenet"
)

func main() {
	ctx := context.Background()
	config := tenet.Config{
		Key:      os.Getenv("TENET_API_KEY"),
		Agent:    "support-agent",
		Failover: true,
	}
	httpClient := tenet.WrapHTTPClient(http.DefaultClient, config)
	prompts := tenet.NewPrompts(httpClient, config, tenet.PromptsOptions{
		Refresh: 30 * time.Second,
		OnError: func(err error) { fmt.Fprintf(os.Stderr, "prompt refresh: %v\n", err) },
	})
	defer prompts.Close()

	// The production prompt from the Agent's prompt store; render it
	// however you like before sending it.
	prompt, err := prompts.Get(ctx, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "prompt: %v\n", err)
		os.Exit(1)
	}
	ctx = tenet.WithPrompt(ctx, prompt)

	// The client keeps the provider, key and model it would use without
	// Tenet: they are the failover when Tenet cannot answer.
	client := openai.NewClient(
		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
		option.WithHTTPClient(httpClient),
	)
	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: "gpt-4.1",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(prompt.Text),
			openai.UserMessage("Say hello in one word."),
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(resp.Choices[0].Message.Content)
}
