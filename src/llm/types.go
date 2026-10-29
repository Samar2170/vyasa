package llm

import (
	"encoding/json"
	"strings"
)

type Role string

const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
	RoleTool      = "tool"
)

type PartKind string

const (
	PartText  PartKind = "text"
	PartImage PartKind = "image"
)

type Image struct {
	URL       string
	Data      []byte
	MediaType string
}

type Part struct {
	Kind  PartKind
	Text  string
	Image *Image
}

type ToolCall struct {
	Index     int
	ID        string
	Name      string
	Arguments json.RawMessage
}

type ReasoningDetail struct {
	Type   string
	Text   string
	Format string
	Index  int
}

type Message struct {
	Role             Role
	Parts            []Part
	ToolCalls        []ToolCall
	ToolCallID       string
	Name             string
	Reasoning        string
	ReasoningDetails []ReasoningDetail
}

func (m Message) Text() string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Kind == PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceTool     ToolChoiceMode = "tool"
)

type ToolChoice struct {
	Mode ToolChoiceMode
	Name string
}

type Request struct {
	Model       string // empty -> Config.DefaultModel
	Messages    []Message
	Tools       []ToolSpec
	ToolChoice  ToolChoice
	Temperature *float64
	TopP        *float64
	MaxTokens   *int
	Stop        []string
}

type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishContentFilter FinishReason = "content_filter"
	FinishError         FinishReason = "error"
	FinishUnknown       FinishReason = "unknown"
)

type Usage struct {
	InputTokens  int
	OutputTokens int
	CachedTokens int
	Cost         *float64
}
type Response struct {
	ID, Model    string
	Message      Message // assistant
	FinishReason FinishReason
	Usage        Usage
	ProviderData json.RawMessage
}

type Chunk struct {
	ID           string
	Model        string
	Delta        Message // text deltas + partial tool calls (use ToolCall.Index)
	FinishReason FinishReason
	Usage        *Usage
}
