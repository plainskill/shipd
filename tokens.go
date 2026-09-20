package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"time"
)

func hashToken(plain string) string {
	h := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(h[:])
}

func newTokenPlain() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CheckToken validates a presented secret against the root token (config)
// or any active managed token. Returns (ok, matchedTokenID or "root").
func (s *State) CheckToken(rootToken, presented string) (bool, string) {
	if subtle.ConstantTimeCompare([]byte(presented), []byte(rootToken)) == 1 {
		return true, "root"
	}
	sum := hashToken(presented)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, t := range s.Tokens {
		if t == nil || !t.active() {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(sum), []byte(t.Hash)) == 1 {
			return true, id
		}
	}
	return false, ""
}

// TouchToken records usage. The marshal happens in Save; throttling is fine
// because last_used_at is cosmetic.
func (s *State) TouchToken(id string) {
	if id == "" || id == "root" {
		return
	}
	s.mu.Lock()
	if t := s.Tokens[id]; t != nil {
		now := time.Now().UTC()
		t.LastUsedAt = &now
	}
	s.mu.Unlock()
	s.Save()
}

// CreateToken mints a named token; plaintext returned exactly once.
func (s *State) CreateToken(name string) (*TokenRecord, string, error) {
	plain, err := newTokenPlain()
	if err != nil {
		return nil, "", err
	}
	idBytes := make([]byte, 4)
	_, _ = rand.Read(idBytes)
	t := &TokenRecord{
		ID:        hex.EncodeToString(idBytes),
		Name:      name,
		Hash:      hashToken(plain),
		CreatedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	if s.Tokens == nil {
		s.Tokens = map[string]*TokenRecord{}
	}
	s.Tokens[t.ID] = t
	s.mu.Unlock()
	s.Save()
	cp := *t
	return &cp, plain, nil
}

// TokenView is a token without its hash, safe to serve.
type TokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// ListTokens returns all tokens (including revoked), newest first.
func (s *State) ListTokens() []TokenView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TokenView, 0, len(s.Tokens))
	for _, t := range s.Tokens {
		out = append(out, TokenView{
			ID:         t.ID,
			Name:       t.Name,
			CreatedAt:  t.CreatedAt,
			LastUsedAt: t.LastUsedAt,
			RevokedAt:  t.RevokedAt,
		})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.After(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// RevokeToken marks a token revoked; returns false if not found.
func (s *State) RevokeToken(id string) bool {
	s.mu.Lock()
	t := s.Tokens[id]
	if t == nil {
		s.mu.Unlock()
		return false
	}
	if t.RevokedAt != nil {
		s.mu.Unlock()
		return true
	}
	now := time.Now().UTC()
	t.RevokedAt = &now
	s.mu.Unlock()
	s.Save()
	return true
}
