// Package unsub signs and checks the tokens of the unsubscribe links: whoever holds the link may end one kind of mail for one address, without signing in, and nothing else.
package unsub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// What a token may end besides an event type: the info class, which is what a summary is unsubscribed from.
const InfoClass = "@info"

// Claim is what a token says: Recipient may stop mails about Type (an event type, or InfoClass).
type Claim struct {
	Recipient string `json:"r"`
	Type      string `json:"t"`
}

// Signer makes and checks tokens with one secret.
type Signer struct {
	key []byte
}

// ErrInvalid is returned for a token that is malformed or not signed with this secret.
var ErrInvalid = errors.New("invalid unsubscribe token")

// New returns a signer; the secret should be at least 32 random bytes.
func New(secret string) (*Signer, error) {
	if len(secret) < 32 {
		return nil, errors.New("the unsubscribe secret must be at least 32 characters")
	}
	return &Signer{key: []byte(secret)}, nil
}

// Sign makes the token for c. It does not expire: an old mail's link has to keep working.
func (s *Signer) Sign(c Claim) string {
	payload, _ := json.Marshal(c)
	p := base64.RawURLEncoding.EncodeToString(payload)
	return p + "." + base64.RawURLEncoding.EncodeToString(s.mac(p))
}

// Verify checks token and returns its claim.
func (s *Signer) Verify(token string) (Claim, error) {
	p, sig, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok {
		return Claim{}, ErrInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(p)) {
		return Claim{}, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return Claim{}, ErrInvalid
	}
	var c Claim
	if err := json.Unmarshal(raw, &c); err != nil || c.Recipient == "" || c.Type == "" {
		return Claim{}, ErrInvalid
	}
	return c, nil
}

func (s *Signer) mac(p string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("unsubscribe:" + p))
	return m.Sum(nil)
}
