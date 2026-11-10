package assistantserver

import (
	"bytes"
	"encoding/json"
	"time"
)

// Audit merge helpers — consumer-side ports of the PHP AuditableQueryBuilder
// roll-up semantics. A mass write becomes ONE merged event shaped
// {"count":N,"rows":...}; an oversized batch splits into several events at
// PAYLOAD_LIMIT_BYTES (PHP parity: +16-byte overhead per row), so every event
// stays under the server's 16KB per-side validation.

// PAYLOAD_LIMIT_BYTES is 16000 — headroom under the server's 16384-byte
// AuditEvent per-side JSON budget (json.dumps(..., ensure_ascii=False) UTF-8
// bytes == these measures).
const PAYLOAD_LIMIT_BYTES = 16000

// Audit actions as the server contract spells them.
const (
	ActionCreate = "create"
	ActionUpdate = "update"
	ActionDelete = "delete"
)

// UpdatePair is one id's before/after image for update roll-ups.
type UpdatePair struct {
	Before map[string]any `json:"before"`
	After  map[string]any `json:"after"`
}

// EventProfile fills the shared identity fields of generated events.
type EventProfile struct {
	Service      string
	OperatorID   string
	OperatorName string
	OperatorIP   string
	TraceID      string
	UserAgent    string
}

// budgetItem is one row for the chunk walk; key "" marks list rows (insert).
type budgetItem struct {
	key     string
	payload any
}

// MergeInsertEvents rolls a bulk insert up into merged create events with
// after = {"count":N,"rows":[payload...]}.
func MergeInsertEvents(service, resource string, rows []map[string]any, profile EventProfile) []map[string]any {
	items := make([]budgetItem, len(rows))
	for i := range rows {
		items[i] = budgetItem{payload: rows[i]}
	}
	var events []map[string]any
	for _, chunk := range chunkByBudget(items) {
		list := make([]any, 0, len(chunk))
		for _, it := range chunk {
			list = append(list, it.payload)
		}
		events = append(events, baseEvent(ActionCreate, service, resource, "",
			nil, map[string]any{"count": len(chunk), "rows": list}, profile))
	}
	return events
}

// MergeDeleteEvents rolls a bulk delete up into merged delete events with
// after = {"count":N,"rows":{id: before-image...}}.
func MergeDeleteEvents(service, resource string, images map[string]map[string]any, profile EventProfile) []map[string]any {
	payloads := make(map[string]any, len(images))
	for id := range images {
		payloads[id] = images[id]
	}
	return mergeKeyed(ActionDelete, service, resource, payloads, profile)
}

// MergeUpdateEvents rolls a bulk update up into merged update events with
// after = {"count":N,"rows":{id: {"before":...,"after":...}}}.
func MergeUpdateEvents(service, resource string, pairs map[string]UpdatePair, profile EventProfile) []map[string]any {
	payloads := make(map[string]any, len(pairs))
	for id := range pairs {
		payloads[id] = pairs[id]
	}
	return mergeKeyed(ActionUpdate, service, resource, payloads, profile)
}

func mergeKeyed(action, service, resource string, payloads map[string]any, profile EventProfile) []map[string]any {
	items := make([]budgetItem, 0, len(payloads))
	for id := range payloads {
		items = append(items, budgetItem{key: id, payload: payloads[id]})
	}
	var events []map[string]any
	for _, chunk := range chunkByBudget(items) {
		rows := make(map[string]any, len(chunk))
		for _, it := range chunk {
			rows[it.key] = it.payload
		}
		events = append(events, baseEvent(action, service, resource, "",
			nil, map[string]any{"count": len(chunk), "rows": rows}, profile))
	}
	return events
}

// baseEvent builds one full wire event (the server AuditEvent contract).
func baseEvent(action, service, resource, resourceID string, before, after map[string]any, profile EventProfile) map[string]any {
	e := map[string]any{
		"event_id":    NewULID(),
		"service":     service,
		"action":      action,
		"resource":    resource,
		"resource_id": resourceID,
		"before":      before,
		"after":       after,
		"created_at":  time.Now().UTC(),
	}
	if profile.OperatorID != "" {
		e["operator_id"] = profile.OperatorID
	}
	if profile.OperatorName != "" {
		e["operator_name"] = profile.OperatorName
	}
	if profile.OperatorIP != "" {
		e["operator_ip"] = profile.OperatorIP
	}
	if profile.TraceID != "" {
		e["trace_id"] = profile.TraceID
	}
	if profile.UserAgent != "" {
		e["user_agent"] = profile.UserAgent
	}
	return e
}

// chunkByBudget walks rows in order, flushing a chunk when adding the next row
// would exceed the per-event budget (+16 bytes row overhead, PHP parity).
// Key-preserving: callers rebuild list- or map-shaped "rows" as needed.
func chunkByBudget(items []budgetItem) [][]budgetItem {
	var chunks [][]budgetItem
	var current []budgetItem
	currentSize := 0
	for _, it := range items {
		size := len(marshalBytes(map[string]any{it.key: it.payload})) + 16
		if current != nil && currentSize+size > PAYLOAD_LIMIT_BYTES {
			chunks = append(chunks, current)
			current = nil
			currentSize = 0
		}
		current = append(current, it)
		currentSize += size
	}
	if current != nil {
		chunks = append(chunks, current)
	}
	return chunks
}

// marshalBytes JSON-encodes with HTML escaping off and UTF-8 raw (parity with
// PHP json_encode JSON_UNESCAPED_UNICODE / server ensure_ascii=False).
func marshalBytes(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
