package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/tenet-voice/tenet-go/tenet"
)

func main() {
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
}
