package cmd

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// verifyJWTCapability validates a compact HS256 JWT. The configured secret is
// supplied per tunnel; audience is optional but, when set, must match aud.
func verifyJWTCapability(token, secret, audience string, now time.Time) bool {
	p := strings.Split(token, ".")
	if len(p) != 3 || secret == "" {
		return false
	}
	dec := func(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
	header, err := dec(p[0])
	if err != nil {
		return false
	}
	var h struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if json.Unmarshal(header, &h) != nil || h.Alg != "HS256" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(p[0] + "." + p[1]))
	sig, err := dec(p[2])
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	payload, err := dec(p[1])
	if err != nil {
		return false
	}
	var c struct {
		Exp int64       `json:"exp"`
		Nbf int64       `json:"nbf"`
		Aud interface{} `json:"aud"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return false
	}
	ts := now.Unix()
	if c.Exp == 0 || ts >= c.Exp || (c.Nbf > 0 && ts < c.Nbf) {
		return false
	}
	if audience != "" {
		switch a := c.Aud.(type) {
		case string:
			if a != audience {
				return false
			}
		case []interface{}:
			ok := false
			for _, v := range a {
				if v == audience {
					ok = true
				}
			}
			if !ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

var errInvalidJWT = errors.New("invalid jwt capability")
