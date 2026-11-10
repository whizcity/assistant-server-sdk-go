package assistantserver

import (
	"strings"
	"testing"
)

// jsonShape measures an audit payload like the server does:
// json.dumps(ensure_ascii=False).encode("utf-8") byte count.
func jsonBytes(v any) int {
	return len(marshalBytes(v))
}

func TestMergeInsertRollsUpsIntoOneCountRowsEvent(t *testing.T) {
	rows := []map[string]any{
		{"title": "订单一", "status": "draft"},
		{"title": "订单二", "status": "draft"},
	}
	events := MergeInsertEvents("parking", "audit_subjects", rows, EventProfile{OperatorID: "9"})

	if len(events) != 1 {
		t.Fatalf("expected one merged event, got %d", len(events))
	}
	e := events[0]
	if e["action"] != "create" || e["resource"] != "audit_subjects" {
		t.Errorf("wrong event identity: %+v", e)
	}
	lbl, idOk := e["event_id"].(string)
	if !idOk || len(lbl) != 26 {
		t.Errorf("event_id must be a 26-char ULID, got %v", e["event_id"])
	}
	if _, has := e["created_at"]; !has {
		t.Errorf("created_at required by AwareUTC contract")
	}
	after := e["after"].(map[string]any)
	if after["count"] != 2 {
		t.Errorf("count wrong: %+v", after)
	}
	rowsOut := after["rows"].([]any)
	first := rowsOut[0].(map[string]any)
	if len(rowsOut) != 2 || first["title"] != "订单一" {
		t.Errorf("rows payload wrong: %+v", rowsOut)
	}
}

func TestMergeInsertChunksOversizedBatches(t *testing.T) {
	rows := make([]map[string]any, 0, 400)
	for i := 0; i < 400; i++ {
		rows = append(rows, map[string]any{"title": strings.Repeat("长", 900)})
	}
	events := MergeInsertEvents("parking", "audit_subjects", rows, EventProfile{})
	if len(events) <= 1 {
		t.Fatalf("oversized batch must split, got %d events", len(events))
	}
	sum := 0
	for _, e := range events {
		if got := jsonBytes(e["after"]); got > 16384 {
			t.Errorf("chunk over server budget: %d bytes", got)
		}
		sum += e["after"].(map[string]any)["count"].(int)
	}
	if sum != 400 {
		t.Errorf("chunk counts must sum to the original rows: %d", sum)
	}
}

func TestMergeDeleteKeepsIdKeyedImages(t *testing.T) {
	images := map[string]map[string]any{
		"1": {"title": "订单一", "status": "draft"},
		"2": {"title": "订单二", "status": "draft"},
	}
	events := MergeDeleteEvents("parking", "audit_subjects", images, EventProfile{})
	if len(events) != 1 {
		t.Fatalf("expected one merged event, got %d", len(events))
	}
	after := events[0]["after"].(map[string]any)
	if events[0]["action"] != "delete" || after["count"] != 2 {
		t.Errorf("wrong delete event: %+v", events[0])
	}
	rows := after["rows"].(map[string]any)
	if rows["2"].(map[string]any)["title"] != "订单二" {
		t.Errorf("id-keyed before images lost: %+v", rows)
	}
}

func TestMergeUpdateKeepsBeforeAfterPairsIdKeyed(t *testing.T) {
	pairs := map[string]UpdatePair{
		"1": {Before: map[string]any{"status": "draft"}, After: map[string]any{"status": "paid"}},
		"2": {Before: map[string]any{"status": "draft"}, After: map[string]any{"status": "paid"}},
	}
	events := MergeUpdateEvents("parking", "audit_subjects", pairs, EventProfile{})
	if len(events) != 1 {
		t.Fatalf("expected one merged event, got %d", len(events))
	}
	after := events[0]["after"].(map[string]any)
	if events[0]["action"] != "update" {
		t.Errorf("wrong action: %+v", events[0])
	}
	rows := after["rows"].(map[string]any)
	p1 := rows["1"].(UpdatePair)
	if p1.Before["status"] != "draft" || p1.After["status"] != "paid" {
		t.Errorf("pair payload wrong: %+v", p1)
	}
}
