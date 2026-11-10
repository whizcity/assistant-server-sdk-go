package assistantserver

import (
	"encoding/json"
	"fmt"
)

// Result mirrors the PHP ResultCase semantics: a response body without a
// "code" key means success; Data carries the explicit "data" key when present
// and the whole body otherwise. Errors never panic — non-JSON bodies fall
// back to Code=<HTTP status>, Message=<first 500 bytes>.
type Result struct {
	Code    string
	Message string
	Data    json.RawMessage
}

func (r *Result) Success() bool { return r.Code == "" }

// Err is a convenience for callers that prefer error flows.
func (r *Result) Err() error {
	if r.Success() {
		return nil
	}
	return fmt.Errorf("assistant-server call failed: %s: %s", r.Code, r.Message)
}

// parseResult converts one wire response into a Result, applying all the
// PHP-parity fallback rules.
func parseResult(status int, body []byte) *Result {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err == nil && payload != nil {
		if raw, ok := payload["code"]; ok {
			var code, message string
			_ = json.Unmarshal(raw, &code)
			if rawMsg, ok := payload["message"]; ok {
				_ = json.Unmarshal(rawMsg, &message)
			}
			return &Result{Code: code, Message: message}
		}
		data, ok := payload["data"]
		if !ok {
			data = json.RawMessage(body)
		}
		return &Result{Data: data}
	}
	// Non-JSON body: PHP parity — code = HTTP status, message = first 500 bytes.
	snippet := body
	if len(snippet) > 500 {
		snippet = snippet[:500]
	}
	return &Result{
		Code:    fmt.Sprintf("%d", status),
		Message: string(snippet),
	}
}
