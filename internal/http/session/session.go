package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

const CookieName = "session_user"

func Sign(username string, now time.Time, ttl time.Duration, secret string) string {
	expiresAt := now.Add(ttl).Unix()
	encodedUser := base64.RawURLEncoding.EncodeToString([]byte(username))
	payload := encodedUser + "." + strconv.FormatInt(expiresAt, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + signature
}

func Verify(token string, now time.Time, secret string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || secret == "" {
		return "", false
	}

	payload := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	expectedSig := mac.Sum(nil)

	gotSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(gotSig, expectedSig) {
		return "", false
	}

	expiresAt, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || now.Unix() > expiresAt {
		return "", false
	}

	usernameBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(usernameBytes) == 0 {
		return "", false
	}

	return string(usernameBytes), true
}
