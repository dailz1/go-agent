package glm

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// generateToken tests
// ---------------------------------------------------------------------------

// TestGenerateJWT_ValidKey verifies that a known key produces a valid JWT.
func TestGenerateJWT_ValidKey(t *testing.T) {
	token, err := generateToken("id123.secret456", 3600)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}
}

// TestGenerateJWT_InvalidKey verifies that a key without a dot returns an error.
func TestGenerateJWT_InvalidKey(t *testing.T) {
	_, err := generateToken("nodothere", 3600)
	if err == nil {
		t.Fatal("expected error for key without dot")
	}
}

// TestGenerateJWT_TokenStructure verifies 3 parts and header contains sign_type: SIGN.
func TestGenerateJWT_TokenStructure(t *testing.T) {
	token, err := generateToken("myid.mysecret", 3600)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d", len(parts))
	}

	// Decode header
	headerBytes, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(parts[0])
	if err != nil {
		t.Fatalf("failed to decode header: %v", err)
	}
	var header map[string]any
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		t.Fatalf("failed to parse header JSON: %v", err)
	}
	if header["alg"] != "HS256" {
		t.Errorf("expected alg=HS256, got %v", header["alg"])
	}
	if header["sign_type"] != "SIGN" {
		t.Errorf("expected sign_type=SIGN, got %v", header["sign_type"])
	}
}

// TestGenerateJWT_ExpireTime verifies exp is in milliseconds and within ±1 second
// of the expected value.
func TestGenerateJWT_ExpireTime(t *testing.T) {
	before := time.Now()
	token, err := generateToken("id.testsecret", 3600)
	after := time.Now()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	parts := strings.Split(token, ".")
	claimsBytes, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(parts[1])
	if err != nil {
		t.Fatalf("failed to decode claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		t.Fatalf("failed to parse claims: %v", err)
	}

	// exp must be in milliseconds
	exp, ok := claims["exp"].(float64)
	if !ok {
		t.Fatal("exp is not a number")
	}
	expSeconds := exp / 1000
	expectedMin := float64(before.Unix()) + 3600 - 1
	expectedMax := float64(after.Unix()) + 3600 + 1
	if expSeconds < expectedMin || expSeconds > expectedMax {
		t.Errorf("exp seconds = %v, expected between %v and %v", expSeconds, expectedMin, expectedMax)
	}

	// timestamp must also be in milliseconds
	ts, ok := claims["timestamp"].(float64)
	if !ok {
		t.Fatal("timestamp is not a number")
	}
	tsSeconds := ts / 1000
	if tsSeconds < float64(before.Unix())-1 || tsSeconds > float64(after.Unix())+1 {
		t.Errorf("timestamp seconds = %v, expected between %v and %v", tsSeconds, before.Unix()-1, after.Unix()+1)
	}
}

// TestGenerateJWT_TestVector verifies key "id123.secret456" produces correct header
// and claims with api_key "id123".
func TestGenerateJWT_TestVector(t *testing.T) {
	token, err := generateToken("id123.secret456", 3600)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parts := strings.Split(token, ".")

	// Verify header
	headerBytes, _ := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(parts[0])
	var header map[string]any
	json.Unmarshal(headerBytes, &header)
	if header["alg"] != "HS256" {
		t.Errorf("expected alg=HS256, got %v", header["alg"])
	}
	if header["sign_type"] != "SIGN" {
		t.Errorf("expected sign_type=SIGN, got %v", header["sign_type"])
	}

	// Verify claims contain api_key
	claimsBytes, _ := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(parts[1])
	var claims map[string]any
	json.Unmarshal(claimsBytes, &claims)
	if claims["api_key"] != "id123" {
		t.Errorf("expected api_key=id123, got %v", claims["api_key"])
	}
}

// TestGenerateJWT_Base64URLEncoding verifies no +, /, or = in token.
func TestGenerateJWT_Base64URLEncoding(t *testing.T) {
	token, err := generateToken("id.testsecret", 3600)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.ContainsAny(token, "+/=") {
		t.Errorf("token contains invalid base64 characters (+, /, or =): %s", token)
	}
}

// TestGenerateJWT_SecretWithDots verifies key "id.sec.ret" splits as id="id",
// secret="sec.ret".
func TestGenerateJWT_SecretWithDots(t *testing.T) {
	token, err := generateToken("id.sec.ret", 3600)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parts := strings.Split(token, ".")

	claimsBytes, _ := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(parts[1])
	var claims map[string]any
	json.Unmarshal(claimsBytes, &claims)
	if claims["api_key"] != "id" {
		t.Errorf("expected api_key=id, got %v", claims["api_key"])
	}

	// Verify the token is valid by checking signature
	// The secret used for HMAC should be "sec.ret"
	headerPayload := parts[0] + "." + parts[1]
	sig := computeHMAC(headerPayload, "sec.ret")
	encodedSig := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(sig)
	if parts[2] != encodedSig {
		t.Errorf("signature mismatch: expected %s, got %s", encodedSig, parts[2])
	}
}

// TestGenerateJWT_GoldenVector verifies a known input produces expected structure
// by round-tripping the signature verification.
func TestGenerateJWT_GoldenVector(t *testing.T) {
	key := "testid.testsecret"
	token, err := generateToken(key, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d", len(parts))
	}

	// Verify signature round-trip
	headerPayload := parts[0] + "." + parts[1]
	sig := computeHMAC(headerPayload, "testsecret")
	encodedSig := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(sig)
	if parts[2] != encodedSig {
		t.Errorf("golden vector signature mismatch")
	}

	// Verify header
	headerBytes, _ := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(parts[0])
	var header map[string]any
	json.Unmarshal(headerBytes, &header)
	if header["alg"] != "HS256" || header["sign_type"] != "SIGN" {
		t.Errorf("unexpected header: %v", header)
	}

	// Verify claims
	claimsBytes, _ := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(parts[1])
	var claims map[string]any
	json.Unmarshal(claimsBytes, &claims)
	if claims["api_key"] != "testid" {
		t.Errorf("expected api_key=testid, got %v", claims["api_key"])
	}
}

// ---------------------------------------------------------------------------
// tokenCache tests
// ---------------------------------------------------------------------------

// TestTokenCache_Hit verifies second call returns cached token.
func TestTokenCache_Hit(t *testing.T) {
	cache := &tokenCache{}

	token1, err := cache.getToken("id1.secret1", 3600)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	token2, err := cache.getToken("id1.secret1", 3600)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if token1 != token2 {
		t.Errorf("expected cached token, got different values:\n  first:  %s\n  second: %s", token1, token2)
	}
}

// TestTokenCache_Expiry verifies expired token is regenerated.
func TestTokenCache_Expiry(t *testing.T) {
	cache := &tokenCache{}

	// Generate token with very short expiry (6 seconds, which is within the 5-minute safety margin)
	token1, err := cache.getToken("id1.secret1", 6)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Simulate time passing by directly manipulating the cache entry
	// Set the entry to be expired (past the 5-minute safety margin)
	cache.entries.Store("id1.secret1", cacheEntry{
		token:     token1,
		expiresAt: time.Now().Add(-6 * time.Minute), // expired + past safety margin
	})

	token2, err := cache.getToken("id1.secret1", 3600)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if token1 == token2 {
		t.Error("expected new token after expiry, got same token")
	}
}

// TestTokenCache_Concurrent verifies goroutines calling getToken don't race.
func TestTokenCache_Concurrent(t *testing.T) {
	cache := &tokenCache{}
	var wg sync.WaitGroup
	const goroutines = 50

	results := make([]string, goroutines)
	errors := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			token, err := cache.getToken("concurrent.key", 3600)
			results[idx] = token
			errors[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errors {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	// All results should be non-empty and identical
	first := results[0]
	for i, r := range results {
		if r != first {
			t.Errorf("goroutine %d: expected token %s, got %s", i, first, r)
		}
	}
}

// TestTokenCache_DifferentKeys verifies different keys produce different tokens.
func TestTokenCache_DifferentKeys(t *testing.T) {
	cache := &tokenCache{}

	token1, err := cache.getToken("id1.secret1", 3600)
	if err != nil {
		t.Fatalf("key1: %v", err)
	}
	token2, err := cache.getToken("id2.secret2", 3600)
	if err != nil {
		t.Fatalf("key2: %v", err)
	}
	if token1 == token2 {
		t.Error("different keys should produce different tokens")
	}

	// Verify each key is cached independently
	token1Again, _ := cache.getToken("id1.secret1", 3600)
	token2Again, _ := cache.getToken("id2.secret2", 3600)
	if token1 != token1Again {
		t.Error("key1 should still be cached")
	}
	if token2 != token2Again {
		t.Error("key2 should still be cached")
	}
}
