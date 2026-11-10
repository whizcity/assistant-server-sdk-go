package assistantserver

import (
	"strings"
	"testing"
)

func TestNoCodeKeyMeansSuccessAndDataIsWholeBody(t *testing.T) {
	body := []byte(`{"accepted":1}`)
	r := parseResult(200, body)
	if !r.Success() {
		t.Errorf("expected success for body without code key: %+v", r)
	}
	if r.Code != "" || r.Message != "" {
		t.Errorf("unexpected code/message on success: %+v", r)
	}
	if string(r.Data) != `{"accepted":1}` {
		t.Errorf("Data should carry the whole body, got %s", r.Data)
	}
	if r.Err() != nil {
		t.Errorf("Err() must be nil on success: %v", r.Err())
	}
}

func TestExplicitDataKeyIsThePayload(t *testing.T) {
	r := parseResult(200, []byte(`{"data":{"k":1},"extra":2}`))
	if string(r.Data) != `{"k":1}` {
		t.Errorf("Data should be the data key, got %s", r.Data)
	}
}

func TestCodeKeyMeansFailureAndErrIsSet(t *testing.T) {
	r := parseResult(422, []byte(`{"code":"VALIDATION_ERROR","message":"Invalid request parameters"}`))
	if r.Success() {
		t.Errorf("expected failure")
	}
	if r.Code != "VALIDATION_ERROR" || r.Message != "Invalid request parameters" {
		t.Errorf("wrong code/message: %+v", r)
	}
	if r.Err() == nil {
		t.Errorf("Err() must be set on failure")
	}
}

func TestNonJSONBodyFallsBackToStatusAndSnippet(t *testing.T) {
	huge := strings.Repeat("x", 600)
	r := parseResult(502, []byte(huge))
	if r.Success() {
		t.Errorf("non-JSON body should be failure")
	}
	if r.Code != "502" {
		t.Errorf("Code should be HTTP status, got %q", r.Code)
	}
	if r.Message != strings.Repeat("x", 500) {
		t.Errorf("message should be first 500 bytes, got len %d", len(r.Message))
	}
}
