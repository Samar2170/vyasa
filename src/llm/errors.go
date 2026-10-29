package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type ProviderError struct {
	Provider   string
	StatusCode int
	Code       string
	Message    string
	Retryable  bool
	Raw        []byte
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "<nil>"
	}

	parts := make([]string, 0, 3)
	if e.Provider != "" {
		parts = append(parts, e.Provider)
	}
	if e.StatusCode > 0 {
		parts = append(parts, fmt.Sprintf("status %d", e.StatusCode))
	}
	if e.Code != "" {
		parts = append(parts, "code "+e.Code)
	}

	prefix := strings.Join(parts, ", ")
	switch {
	case prefix == "":
		return e.Message
	case e.Message == "":
		return prefix
	default:
		return prefix + ": " + e.Message
	}
}

func isRetryableStatus(code int) bool {
	return code == http.StatusRequestTimeout ||
		code == http.StatusTooManyRequests ||
		code >= http.StatusInternalServerError
}

func newProviderError(provider string, statusCode int, body []byte) *ProviderError {
	pe := &ProviderError{
		Provider:   provider,
		StatusCode: statusCode,
		Message:    strings.TrimSpace(string(body)),
		Retryable:  isRetryableStatus(statusCode),
		Raw:        body,
	}

	var envelope struct {
		Error *struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
		Code    any    `json:"code"`
	}

	if json.Unmarshal(body, &envelope) == nil {
		switch {
		case envelope.Error != nil && envelope.Error.Message != "":
			pe.Message = envelope.Error.Message
			pe.Code = codeString(envelope.Error.Code)
		case envelope.Message != "":
			pe.Message = envelope.Message
			pe.Code = codeString(envelope.Code)
		}
	}

	if pe.Message == "" {
		pe.Message = http.StatusText(statusCode)
	}

	return pe
}

func codeString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
