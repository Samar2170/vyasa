package main

import (
	"context"
	"fmt"

	"vyasa/src/config"
	"vyasa/src/llm"

	_ "vyasa/src/llm/providers/openrouter"
)

func main() {
	if err := config.LoadDotEnv(".env"); err != nil {
		panic(err)
	}

	settings, err := config.Load("config.json")
	if err != nil {
		panic(err)
	}

	apiKey, err := config.RequireSecret("OPENROUTER_API_KEY")
	if err != nil {
		panic(err)
	}

	client, err := llm.NewClient(settings.Provider, llm.Config{
		APIKey:       apiKey,
		DefaultModel: settings.Model,
		MaxRetries:   settings.MaxRetries,
		Timeout:      settings.Timeout(),
	})
	if err != nil {
		panic(err)
	}

	ctx := context.Background()
	req := llm.Request{
		Model: settings.Model,
		Messages: []llm.Message{
			{Role: "user",
				Parts: []llm.Part{
					{Kind: llm.PartText, Text: "Hello"},
				},
			},
		},
	}

	resp, err := client.Complete(ctx, req)
	if err != nil {
		panic(err)
	}

	fmt.Println(resp.Message)
}
