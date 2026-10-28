package main

import (
	"context"
	"fmt"
	"os"
	"vyasa/src/internal/llm"
)

func main() {
	client, err := llm.NewClient("openrouter", os.Getenv("OPENROUTER_API_KEY"))
	if err != nil {
		panic(err)
	}

	ctx := context.Background()
	req := llm.Request{
		Model: "z-ai/glm-5.3-flash",
		Messages: []llm.Message{
			{Role: "user", Content: "Hello!"},
		},
	}

	resp, err := client.Complete(ctx, req)
	if err != nil {
		panic(err)
	}

	fmt.Println(resp.Content)
}
