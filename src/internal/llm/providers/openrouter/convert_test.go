package openrouter

import (
	"encoding/json"
	"testing"

	"vyasa/src/internal/llm"
)

func TestToChatRequestTextAndImage(t *testing.T) {
	req := llm.Request{
		Model: "test/model",
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			Parts: []llm.Part{
				{Kind: llm.PartText, Text: "describe"},
				{
					Kind: llm.PartImage,
					Image: &llm.Image{
						Data:      []byte{0x89, 'P', 'N', 'G'},
						MediaType: "image/png",
					},
				},
			},
		}},
	}

	got, err := toChatRequest(req, "")
	if err != nil {
		t.Fatalf("toChatRequest: %v", err)
	}
	if got.Model != "test/model" {
		t.Fatalf("model = %q", got.Model)
	}

	parts, ok := got.Messages[0].Content.([]wireContentPart)
	if !ok {
		t.Fatalf("content type = %T", got.Messages[0].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "describe" {
		t.Fatalf("text part = %#v", parts[0])
	}
	// if parts[1].ImageURL == "" ||
	// 	!strings.HasPrefix(parts[1].ImageURL.URL, "data:image/png;base64,") {
	// 	t.Fatalf("image part = %#v", parts[1])
	// }
}

func TestToChatRequestTools(t *testing.T) {
	req := llm.Request{
		Model: "test/model",
		Messages: []llm.Message{{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID:        "call_1",
				Name:      "read_file",
				Arguments: json.RawMessage(`{"path":"a.txt"}`),
			}},
		}},
		Tools: []llm.ToolSpec{{
			Name:        "read_file",
			Description: "Read a file",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
		ToolChoice: llm.ToolChoice{
			Mode: llm.ToolChoiceTool,
			Name: "read_file",
		},
	}

	got, err := toChatRequest(req, "")
	if err != nil {
		t.Fatalf("toChatRequest: %v", err)
	}
	if got.Messages[0].Content != nil {
		t.Fatalf("assistant content = %#v", got.Messages[0].Content)
	}
	if got.Messages[0].ToolCalls[0].Function.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("arguments = %q", got.Messages[0].ToolCalls[0].Function.Arguments)
	}
	if got.Tools[0].Function.Name != "read_file" {
		t.Fatalf("tool = %#v", got.Tools[0])
	}

	raw, err := json.Marshal(got.ToolChoice)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"function":{"name":"read_file"},"type":"function"}` {
		t.Fatalf("tool choice = %s", raw)
	}
}

func TestFromChatResponse(t *testing.T) {
	raw := []byte(`{"id":"gen-1","model":"m","provider":"openai","choices":[{"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]},"finish_reason":"tool_calls","native_finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"cost":0.002,"prompt_tokens_details":{"cached_tokens":3}}}`)

	var wire chatResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got, err := fromChatResponse(wire)
	if err != nil {
		t.Fatalf("fromChatResponse: %v", err)
	}
	if got.Message.Text() != "hello" {
		t.Fatalf("text = %q", got.Message.Text())
	}
	if got.FinishReason != llm.FinishToolCalls {
		t.Fatalf("finish = %q", got.FinishReason)
	}
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 5 || got.Usage.CachedTokens != 3 {
		t.Fatalf("usage = %#v", got.Usage)
	}
	if got.Usage.Cost == nil || *got.Usage.Cost != 0.002 {
		t.Fatalf("cost = %#v", got.Usage.Cost)
	}
	if string(got.Message.ToolCalls[0].Arguments) != `{"path":"a"}` {
		t.Fatalf("arguments = %s", got.Message.ToolCalls[0].Arguments)
	}
	if got.Provider != "openai" || got.NativeFinishReason != "tool_calls" {
		t.Fatalf("extras = %#v", got)
	}
	if len(got.ProviderData) == 0 {
		t.Fatal("provider data is empty")
	}
}

func TestFromWireStreamChoice(t *testing.T) {
	choice := wireChoice{
		Delta: wireMessage{
			Content: "Hel",
			ToolCalls: []wireToolCall{{
				Index: 0,
				ID:    "call_1",
				Function: wireFunctionCall{
					Name:      "read_file",
					Arguments: `{"pa`,
				},
			}},
		},
		FinishReason: "tool_calls",
	}

	got, err := fromWireStreamChoice("gen-1", "m", choice)
	if err != nil {
		t.Fatalf("fromWireStreamChoice: %v", err)
	}
	if got.Delta.Text() != "Hel" {
		t.Fatalf("text = %q", got.Delta.Text())
	}
	if len(got.Delta.ToolCalls) != 1 ||
		string(got.Delta.ToolCalls[0].Arguments) != `{"pa` {
		t.Fatalf("tool calls = %#v", got.Delta.ToolCalls)
	}
	if got.FinishReason != llm.FinishToolCalls {
		t.Fatalf("finish = %q", got.FinishReason)
	}
}
