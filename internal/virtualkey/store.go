// Package virtualkey manages the gateway's virtual API keys: each key
// identifies a team/application, carries model permissions, and maps to its
// own rate limits. The gateway accepts these keys instead of exposing raw
// provider keys to clients (§8.2).
package virtualkey

import (
	"fmt"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/config"
)

// Key is a virtual API key with its permissions.
type Key struct {
	Value         string
	Description   string
	AllowedModels []string // "*" = all models
	RateLimit     config.LimitConfig
}

// Allows reports whether the key may use the given model.
func (k *Key) Allows(model string) bool {
	for _, m := range k.AllowedModels {
		if m == "*" || m == model {
			return true
		}
	}
	return false
}

// Store holds the configured virtual keys in memory (§9: "In-memory store
// loaded from config; future: persistent store"). It is read-only after
// construction, so it is safe for concurrent use.
type Store struct {
	byValue map[string]*Key
}

// NewStore builds a store from config.
func NewStore(cfg config.VirtualKeysConfig) (*Store, error) {
	s := &Store{byValue: make(map[string]*Key, len(cfg.Keys))}
	for _, kc := range cfg.Keys {
		if _, dup := s.byValue[kc.Key]; dup {
			return nil, fmt.Errorf("virtual key %q is defined twice", kc.Key)
		}
		s.byValue[kc.Key] = &Key{
			Value:         kc.Key,
			Description:   kc.Description,
			AllowedModels: kc.AllowedModels,
			RateLimit:     kc.RateLimit,
		}
	}
	return s, nil
}

// Lookup returns the key with the given value, or nil.
func (s *Store) Lookup(value string) (*Key, bool) {
	k, ok := s.byValue[value]
	return k, ok
}

// Len returns the number of configured keys.
func (s *Store) Len() int { return len(s.byValue) }
