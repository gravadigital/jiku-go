// Package auth obtains Zitadel access tokens for a Jiku bus connection.
//
// # WHY A TOKEN IS WHAT MATTERS
//
// Connecting to Jiku's NATS needs two things, and only one of them is a secret worth guarding:
//
//  1. The sentinel creds — a NATS user JWT that grants NOTHING. Its own permissions are
//     `pub.deny: [">"]` and `sub.deny: [">"]`. It exists to let the connection reach the
//     auth-callout, nothing more.
//  2. A Zitadel access token — THIS is what mints permissions. The callout validates it,
//     reads the role, picks a template and returns a user JWT with real subject permissions
//     for that connection.
//
// So the interesting work of authenticating to Jiku is entirely the work of getting a Zitadel
// token, which is what this package does.
//
// # THE TWO MODES
//
// Device flow (a person, interactively) — RFC 8628. You authorise once in a browser and the
// tokens land in a file:
//
//	src, err := auth.NewDeviceFlow(auth.DeviceConfig{
//	    Issuer:   "https://id.grava.io",
//	    ClientID: "385696162499330050@gestor_de_proyectos",
//	    Store:    auth.DefaultStore("dev"),
//	})
//
// Service user (a service, unattended) — RFC 7523 JWT profile, from the JSON key Zitadel hands
// you when you add a key to a machine user:
//
//	src, err := auth.NewServiceUser(auth.ServiceUserConfig{
//	    Issuer:  "https://id.grava.io",
//	    KeyFile: "/etc/jiku/service-account.json",
//	})
//
// # TOKENS ARE FETCHED PER CONNECTION ATTEMPT, NOT ONCE
//
// The callout evaluates the token at CONNECT time and the resulting permissions live for the
// life of the connection — NATS does not re-check. That is fine until the connection drops:
// a reconnect re-runs the callout, and a token that has since expired means the reconnect is
// refused. So a TokenSource is asked for a token on every (re)connect and is responsible for
// returning a fresh one, which is why this is an interface and not a string.
package auth

import (
	"context"
	"fmt"
	"time"
)

// TokenSource yields a currently-valid access token.
//
// Token is called on every connect AND on every reconnect, from nats.go's token handler, so
// implementations must be safe for concurrent use and should refresh rather than return
// something expired. They should also be fast in the common case: cache, and only talk to the
// identity provider when the cached token is close to expiry.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	// Subject is the `sub` of the identity behind the token. It is the caller's identity in
	// every subject and the seed of its inbox prefix, so the client needs it before it can
	// build either.
	Subject(ctx context.Context) (string, error)
}

// refreshSkew is how long before real expiry a token is treated as expired.
//
// It absorbs clock skew between us and Zitadel plus the flight time of the request that is
// about to use the token. A token that expires while the callout is validating it is refused,
// and the symptom is an authorization violation on connect rather than anything about time.
const refreshSkew = 60 * time.Second

// Tokens is what an OIDC token endpoint answered, plus when we learned it.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	Scope        string `json:"scope,omitempty"`

	// ObtainedAt is set by this package, not by the provider, so expiry survives being
	// written to a file and read back tomorrow. Without it a stored `expires_in` is
	// meaningless.
	ObtainedAt time.Time `json:"obtained_at,omitempty"`
}

// Expiry is when the access token stops being accepted.
//
// It prefers the JWT's own `exp` claim over `expires_in`, because the claim is what the
// callout actually reads and it survives a file round-trip without needing ObtainedAt.
func (t Tokens) Expiry() time.Time {
	if claims, err := ParseClaims(t.AccessToken); err == nil && claims.Exp > 0 {
		return time.Unix(claims.Exp, 0)
	}
	if t.ExpiresIn > 0 && !t.ObtainedAt.IsZero() {
		return t.ObtainedAt.Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	return time.Time{}
}

// Valid reports whether the access token is present and not within refreshSkew of expiry.
// An unknown expiry counts as valid: the token may be an opaque one, and the authority on
// whether it is accepted is the callout, not us.
func (t Tokens) Valid() bool {
	if t.AccessToken == "" {
		return false
	}
	exp := t.Expiry()
	if exp.IsZero() {
		return true
	}
	return time.Now().Add(refreshSkew).Before(exp)
}

// tokenError is an OAuth error response, which carries the field that says what to do next.
type tokenError struct {
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
	Status      int    `json:"-"`
}

func (e *tokenError) Error() string {
	msg := fmt.Sprintf("zitadel: %s", e.Code)
	if e.Description != "" {
		msg += ": " + e.Description
	}
	if hint := oauthHint(e.Code); hint != "" {
		msg += "\n  hint: " + hint
	}
	return msg
}

// oauthHint maps the OAuth error codes that actually happen here to their real cause. Every
// one of these was a line in the troubleshooting table of the shell scripts this replaces.
func oauthHint(code string) string {
	switch code {
	case "unauthorized_client":
		return "the app does not have the grant type enabled, or the client id is wrong. " +
			"For the device flow, enable \"Device Code\" on a Native app in Zitadel."
	case "expired_token":
		return "the device code expired (300s). Run login again."
	case "access_denied":
		return "the authorization was rejected in the browser."
	case "invalid_client":
		return "the client id, or the service account key, was not accepted. Check that the " +
			"key still exists on the machine user and has not been revoked."
	case "invalid_grant":
		return "the assertion was rejected. The usual causes are a clock skew of more than a " +
			"minute, a wrong audience, or a key that was deleted in Zitadel."
	case "invalid_scope":
		return "a requested scope is not allowed for this client. Check the project id in the " +
			"reserved zitadel scopes."
	}
	return ""
}
