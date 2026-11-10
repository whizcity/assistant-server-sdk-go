package assistantserver

import "testing"

// Interop fixtures generated against the server signature rule verified in the
// PHP SDK (app/auth/signature.py). Expected values are hard-coded literals; the
// test recomputes the signature with stdlib hashing — it must never call the
// implementation being tested ("signed thing != sent thing" guard).
const (
	testApp    = "tester"
	testSecret = "s3cret-key-x" // GET-fixture identity (PHP SignerTest line 40)
	postSecret = "demo-secret"         // POST fixture's own secret (PHP SignerTest line 18)
)

func TestPostBatchSignatureMatchesServerInteropFixture(t *testing.T) {
	headers := SignHeaders(testApp, postSecret, "POST", "/api/v1/audit/logs/batch",
		`{"service":"demo","note":"中文"}`, "",
		"1700000000", "n0nce1234567890abcdefghij")

	if got, want := headers[HeaderContentSHA256], "f87fc63e6f6225e1e825d948cf1c30f27096510fac3c505828e787eff3c5aa47"; got != want {
		t.Errorf("content sha mismatch:\n got  %s\n want %s", got, want)
	}
	if got, want := headers[HeaderSignature], "1d1ed0f780f961b70593c1507b68d32e8b4ecf0455e73025265f8d809e641e95"; got != want {
		t.Errorf("signature mismatch:\n got  %s\n want %s", got, want)
	}
	if headers[HeaderAppID] != "tester" || headers[HeaderTimestamp] != "1700000000" || headers[HeaderNonce] != "n0nce1234567890abcdefghij" {
		t.Errorf("identity headers wrong: %+v", headers)
	}
}

func TestGetQuerySignatureMatchesServerInteropFixture(t *testing.T) {
	headers := SignHeaders(testApp, testSecret, "GET", "/api/v1/audit/logs",
		"", "action=update&resource=orders&size=20",
		"1893456000", "getnonce-1234567890abcdef")

	if got, want := headers[HeaderContentSHA256], "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"; got != want {
		t.Errorf("empty-body digest mismatch:\n got  %s\n want %s", got, want)
	}
	if got, want := headers[HeaderSignature], "bddc28599b00333520d4c5312c3aa8ebdd6b2b028932d629c530a9a8d1ba1503"; got != want {
		t.Errorf("signature mismatch:\n got  %s\n want %s", got, want)
	}
}
