package glm

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type cacheEntry struct {
	token     string
	expiresAt time.Time
}

type tokenCache struct {
	entries sync.Map
}

func generateToken(apiKey string, expSeconds int) (string, error) {
	parts := splitAPIKey(apiKey)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid API key format: expected 'id.secret'")
	}
	id, secret := parts[0], parts[1]

	now := time.Now()
	nowMs := now.UnixMilli()
	expMs := now.Add(time.Duration(expSeconds) * time.Second).UnixMilli()

	header := map[string]string{
		"alg":       "HS256",
		"sign_type": "SIGN",
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshal header: %w", err)
	}

	claims := map[string]any{
		"api_key":   id,
		"exp":       expMs,
		"timestamp": nowMs,
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}

	encoder := base64.URLEncoding.WithPadding(base64.NoPadding)
	headerB64 := encoder.EncodeToString(headerJSON)
	claimsB64 := encoder.EncodeToString(claimsJSON)

	headerPayload := headerB64 + "." + claimsB64
	sig := computeHMAC(headerPayload, secret)
	sigB64 := encoder.EncodeToString(sig)

	return headerPayload + "." + sigB64, nil
}

func computeHMAC(message, secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	return mac.Sum(nil)
}

func splitAPIKey(key string) []string {
	parts := make([]string, 0, 2)
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			parts = append(parts, key[:i])
			parts = append(parts, key[i+1:])
			return parts
		}
	}
	return nil
}

func (tc *tokenCache) getToken(apiKey string, expSeconds int) (string, error) {
	if cached, ok := tc.entries.Load(apiKey); ok {
		entry := cached.(cacheEntry)
		if time.Now().Before(entry.expiresAt.Add(-5 * time.Minute)) {
			return entry.token, nil
		}
	}

	token, err := generateToken(apiKey, expSeconds)
	if err != nil {
		return "", err
	}

	tc.entries.Store(apiKey, cacheEntry{
		token:     token,
		expiresAt: time.Now().Add(time.Duration(expSeconds) * time.Second),
	})

	return token, nil
}
