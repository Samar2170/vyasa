package openrouter

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"vyasa/src/llm"
)

type Response struct {
	llm.Response
	Provider           string
	NativeFinishReason string
}

func toChatRequest(req llm.Request, defaultModel string) (chatRequest, error) {
	model := req.Model
	if model == "" {
		model = defaultModel
	}
	if model == "" {
		return chatRequest{}, errors.New("openrouter: model is required")
	}

	out := chatRequest{
		Model:       model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
	}

	for _, msg := range req.Messages {
		wireMsg, err := toWireMessage(msg)
		if err != nil {
			return chatRequest{}, err
		}
		out.Messages = append(out.Messages, wireMsg)
	}

	if len(req.Tools) > 0 {
		out.Tools = make([]wireTool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			if tool.Name == "" {
				return chatRequest{}, errors.New("openrouter: tool name is required")
			}

			params := tool.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			if !json.Valid(params) {
				return chatRequest{}, fmt.Errorf("openrouter: tool %q has invalid parameter schema", tool.Name)
			}

			out.Tools = append(out.Tools, wireTool{
				Type: "function",
				Function: wireFunction{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  params,
				},
			})
		}
	}

	toolChoice, ok, err := toWireToolChoice(req.ToolChoice)
	if err != nil {
		return chatRequest{}, err
	}
	if ok {
		out.ToolChoice = toolChoice
	}

	return out, nil
}

func toWireToolChoice(choice llm.ToolChoice) (any, bool, error) {
	switch choice.Mode {
	case "":
		return nil, false, nil
	case llm.ToolChoiceAuto, llm.ToolChoiceNone, llm.ToolChoiceRequired:
		return string(choice.Mode), true, nil
	case llm.ToolChoiceTool:
		if choice.Name == "" {
			return nil, false, errors.New("openrouter: tool choice name is required")
		}
		return map[string]any{
			"type": "function",
			"function": map[string]string{
				"name": choice.Name,
			},
		}, true, nil
	default:
		return nil, false, fmt.Errorf("openrouter: unknown tool choice mode %q", choice.Mode)
	}
}

func toWireMessage(msg llm.Message) (wireMessage, error) {
	if msg.Role == "" {
		return wireMessage{}, errors.New("openrouter: message role is required")
	}

	content, err := toWireContent(msg)
	if err != nil {
		return wireMessage{}, err
	}

	out := wireMessage{
		Role:       string(msg.Role),
		Content:    content,
		Name:       msg.Name,
		ToolCallID: msg.ToolCallID,
	}

	if len(msg.ToolCalls) > 0 {
		out.ToolCalls = make([]wireToolCall, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			if call.Name == "" {
				return wireMessage{}, errors.New("openrouter: tool call name is required")
			}

			args := "{}"
			if len(call.Arguments) > 0 {
				if !json.Valid(call.Arguments) {
					return wireMessage{}, fmt.Errorf("openrouter: invalid arguments for tool call %q", call.Name)
				}
				args = string(call.Arguments)
			}

			out.ToolCalls = append(out.ToolCalls, wireToolCall{
				ID:   call.ID,
				Type: "function",
				Function: wireFunctionCall{
					Name:      call.Name,
					Arguments: args,
				},
			})
		}
	}

	return out, nil
}

func toWireContent(msg llm.Message) (any, error) {
	if len(msg.Parts) == 0 {
		return nil, nil
	}

	hasImage := false
	for _, part := range msg.Parts {
		if part.Kind == llm.PartImage {
			hasImage = true
			break
		}
	}

	if !hasImage {
		texts := make([]string, 0, len(msg.Parts))
		for _, part := range msg.Parts {
			if part.Kind != llm.PartText {
				return nil, fmt.Errorf("openrouter: unsupported content part %q", part.Kind)
			}
			texts = append(texts, part.Text)
		}
		return strings.Join(texts, "\n"), nil
	}

	parts := make([]wireContentPart, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		switch part.Kind {
		case llm.PartText:
			parts = append(parts, wireContentPart{
				Type: "text",
				Text: part.Text,
			})
		case llm.PartImage:
			url, err := imageURL(part.Image)
			if err != nil {
				return nil, err
			}
			parts = append(parts, wireContentPart{
				Type: "image_url",
				ImageURL: wireImageURL{
					URL: url,
				},
			})
		default:
			return nil, fmt.Errorf("openrouter: unsupported content part %q", part.Kind)
		}
	}

	return parts, nil
}

func imageURL(img *llm.Image) (string, error) {
	if img == nil {
		return "", errors.New("openrouter: image part is missing image data")
	}
	if img.URL != "" {
		return img.URL, nil
	}
	if len(img.Data) == 0 {
		return "", errors.New("openrouter: image requires URL or data")
	}

	mediaType := img.MediaType
	if mediaType == "" {
		mediaType = http.DetectContentType(img.Data)
	}

	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data), nil
}

func fromWireContent(value any) ([]llm.Part, error) {
	switch content := value.(type) {
	case nil:
		return nil, nil
	case string:
		if content == "" {
			return nil, nil
		}
		return []llm.Part{{Kind: llm.PartText, Text: content}}, nil
	case []any:
		parts := make([]llm.Part, 0, len(content))
		for _, item := range content {
			obj, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("openrouter: invalid content part")
			}

			partType, _ := obj["type"].(string)
			switch partType {
			case "text":
				text, _ := obj["text"].(string)
				if text != "" {
					parts = append(parts, llm.Part{Kind: llm.PartText, Text: text})
				}
			case "image_url":
				image, ok := obj["image_url"].(map[string]any)
				if !ok {
					continue
				}
				url, _ := image["url"].(string)
				if url != "" {
					parts = append(parts, llm.Part{
						Kind:  llm.PartImage,
						Image: &llm.Image{URL: url},
					})
				}
			default:
				return nil, fmt.Errorf("openrouter: unsupported response content part %q", partType)
			}
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("openrouter: unsupported content type %T", value)
	}
}

func fromWireMessage(msg wireMessage) (llm.Message, error) {
	parts, err := fromWireContent(msg.Content)
	if err != nil {
		return llm.Message{}, err
	}

	role := llm.Role(msg.Role)
	if role == "" {
		role = llm.RoleAssistant
	}

	out := llm.Message{
		Role:       role,
		Parts:      parts,
		ToolCallID: msg.ToolCallID,
		Name:       msg.Name,
	}

	for i, call := range msg.ToolCalls {
		args := json.RawMessage(call.Function.Arguments)
		switch {
		case len(args) == 0:
			args = json.RawMessage(`{}`)
		case !json.Valid(args):
			return llm.Message{}, fmt.Errorf("openrouter: invalid tool arguments for %q", call.Function.Name)
		}

		out.ToolCalls = append(out.ToolCalls, llm.ToolCall{
			Index:     i,
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: args,
		})
	}

	return out, nil
}

func fromChatResponse(resp chatResponse) (*Response, error) {
	if resp.Error != nil {
		return nil, &llm.ProviderError{
			Provider:   "openrouter",
			StatusCode: http.StatusOK,
			Code:       codeString(resp.Error.Code),
			Message:    resp.Error.Message,
		}
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("openrouter: response contained no choices")
	}

	choice := resp.Choices[0]
	message, err := fromWireMessage(choice.Message)
	if err != nil {
		return nil, err
	}

	finish := finishReason(choice.FinishReason)
	if finish == "" {
		finish = llm.FinishUnknown
	}

	canonical := llm.Response{
		ID:           resp.ID,
		Model:        resp.Model,
		Message:      message,
		FinishReason: finish,
		Usage:        usageFromWire(resp.Usage),
	}

	if data, err := json.Marshal(map[string]string{
		"provider":             resp.Provider,
		"native_finish_reason": choice.NativeFinishReason,
	}); err == nil {
		canonical.ProviderData = data
	}

	return &Response{
		Response:           canonical,
		Provider:           resp.Provider,
		NativeFinishReason: choice.NativeFinishReason,
	}, nil
}

func fromWireStreamChoice(id, model string, choice wireChoice) (llm.Chunk, error) {
	parts, err := fromWireContent(choice.Delta.Content)
	if err != nil {
		return llm.Chunk{}, err
	}

	delta := llm.Message{
		Role:  llm.RoleAssistant,
		Parts: parts,
	}

	for _, call := range choice.Delta.ToolCalls {
		delta.ToolCalls = append(delta.ToolCalls, llm.ToolCall{
			Index:     call.Index,
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: json.RawMessage(call.Function.Arguments),
		})
	}

	return llm.Chunk{
		ID:           id,
		Model:        model,
		Delta:        delta,
		FinishReason: finishReason(choice.FinishReason),
	}, nil
}

func usageFromWire(usage *wireUsage) llm.Usage {
	if usage == nil {
		return llm.Usage{}
	}
	return llm.Usage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
		CachedTokens: usage.PromptTokensDetails.CachedTokens,
		Cost:         usage.Cost,
	}
}

func usagePtr(usage *wireUsage) *llm.Usage {
	if usage == nil {
		return nil
	}
	u := usageFromWire(usage)
	return &u
}

func finishReason(raw string) llm.FinishReason {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "stop", "end_turn", "stop_sequence":
		return llm.FinishStop
	case "length", "max_tokens":
		return llm.FinishLength
	case "tool_calls", "tool_use", "function_call":
		return llm.FinishToolCalls
	case "content_filter", "safety":
		return llm.FinishContentFilter
	case "":
		return ""
	default:
		return llm.FinishUnknown
	}
}

func codeString(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}
