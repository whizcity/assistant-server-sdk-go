package assistantserver

import (
	"regexp"
	"testing"
	"time"
)

var ulidPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TVWXYZ]{26}$`)

func TestNewULIDShape(t *testing.T) {
	id := NewULID()
	if !ulidPattern.MatchString(id) {
		t.Errorf("ULID shape wrong: %q", id)
	}
}

func TestNewULIDTimestampRoundTripsToNow(t *testing.T) {
	id := NewULID()
	var ms uint64
	for _, c := range id[:10] {
		ms = ms*32 + uint64(ulidABC.indexOf(byte(c)))
	}
	if diff := time.Now().UnixMilli() - int64(ms); diff < 0 || diff > 5000 {
		t.Errorf("ULID timestamp %d far from now (diff %dms)", ms, diff)
	}
}

func TestNewULIDUnique(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 500; i++ {
		id := NewULID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ULID within 500 draws: %s", id)
		}
		seen[id] = struct{}{}
	}
}
