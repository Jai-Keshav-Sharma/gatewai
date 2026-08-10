package middleware

import (
	"net/http"
	"strings"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/virtualkey"
)

// Auth is middleware step 4 of the chain (§4.1): validate the Bearer token.
//
// With virtual keys ENABLED the token must be a configured virtual key
// (401 otherwise), and the requested model must be allowed for that key
// (403 otherwise — §8.2).
// With virtual keys DISABLED the gateway accepts any non-empty Bearer token
// (a raw provider key, §8.2).
func Auth(enabled bool, store *virtualkey.Store) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := schema.RequestContextFrom(r.Context())
			if rc == nil || rc.ParsedRequest == nil {
				next.ServeHTTP(w, r)
				return
			}

			token, ok := bearerToken(r)
			if !ok {
				schema.NewAuthenticationError("missing bearer token: send Authorization: Bearer <key>").WriteJSON(w)
				return
			}
			rc.VirtualKey = token

			if enabled {
				key, found := store.Lookup(token)
				if !found {
					schema.NewAuthenticationError("invalid API key").WriteJSON(w)
					return
				}
				if !key.Allows(rc.ParsedRequest.Model) {
					schema.NewPermissionError(
						"key " + token + " is not allowed to use model " + rc.ParsedRequest.Model,
					).WriteJSON(w)
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the token from the Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	return token, token != ""
}
