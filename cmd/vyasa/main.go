package main

import (
	"context"
	"fmt"
	"os"
	"vyasa/src/llm"
	"vyasa/src/llm/providers/openrouter"

	"github.com/joho/godotenv"
)

func loadEnv() {
	godotenv.Load()
}

func main() {
	orConfig := llm.Config{
		APIKey:       os.Getenv("OPENROUTER_API_KEY"),
		DefaultModel: "z-ai/glm-5.3-flash",
	}
	if orConfig.APIKey == "" {
		panic("OPENROUTER_API_KEY not set")
	}
	llm.RegisterProvider("openrouter", openrouter.New)
	client, err := llm.NewClient("openrouter", orConfig)
	if err != nil {
		panic(err)
	}

	ctx := context.Background()
	req := llm.Request{
		Model: "z-ai/glm-5.3-flash",
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
