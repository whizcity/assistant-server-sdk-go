// Package assistantserver is the Go client for the central assistant-server
// (Python FastAPI). Every call is signed with one-time HMAC headers using the
// shared canonical rule implemented server-side in app/auth/signature.py.
//
// Canonical string to sign (UTF-8, lines joined by "\n", fixed order):
//
//	HTTP-METHOD (uppercase)
//	PATH        (no host, no query)
//	QUERY       (raw string, no "?")
//	TIMESTAMP   (unix seconds; 1e10+ means milliseconds and gets rejected)
//	NONCE       (8..64 chars of [A-Za-z0-9_-], one distinct value per call)
//	CONTENT-SHA (lowercase hex SHA-256 of the raw request body; empty body
//	             hashed too)
//
// signature = lowercase hex of HMAC-SHA256(raw app secret, canonical).
package assistantserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	HeaderAppID         = "X-App-Id"
	HeaderTimestamp     = "X-Timestamp"
	HeaderNonce         = "X-Nonce"
	HeaderContentSHA256 = "X-Content-SHA256"
	HeaderSignature     = "X-Signature"
)

// SignHeaders builds the five one-time signed request headers. Callers pass
// explicit timestamp (unix seconds as decimal string) and nonce; the wire
// client supplies freshly generated values per call.
func SignHeaders(appID, appSecret, method, path, body, query, timestamp, nonce string) map[string]string {
	digest := SHA256Hex(body)
	canonical := strings.Join([]string{
		strings.ToUpper(method),
		path,
		query,
		timestamp,
		nonce,
		digest,
	}, "\n")

	return map[string]string{
		HeaderAppID:         appID,
		HeaderTimestamp:     timestamp,
		HeaderNonce:         nonce,
		HeaderContentSHA256: digest,
		HeaderSignature:     HMACSHA256Hex(appSecret, canonical),
	}
}

// SHA256Hex is the lowercase hex SHA-256 of the raw body.
func SHA256Hex(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// HMACSHA256Hex is the lowercase hex HMAC-SHA256 of message under key secretBytes.
func HMACSHA256Hex(secretKeyBytes, message string) string {
	mac := hmac.New(sha256.New, []byte(secretKeyBytes))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}
