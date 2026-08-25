package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// jwt builds an unsigned JWT with the given claims. The signature is never verified by this
// package — see Claims — so a placeholder is enough to exercise the parsing.
func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." +
		enc.EncodeToString(b) + ".c2ln"
}

// TestParseClaimsMergesBothRoleClaimShapes is the regression test for the bug that made a
// machine user look like it had no roles at all.
//
// Zitadel emits project roles under a generic claim OR a project-scoped one, depending on the
// request. A person's device-flow token carried the first; the service user's carried the
// second, and reading only the first reported "roles: none" for a token that plainly had one.
func TestParseClaimsMergesBothRoleClaimShapes(t *testing.T) {
	cases := []struct {
		name  string
		claim string
	}{
		{name: "generic, all projects", claim: "urn:zitadel:iam:org:project:roles"},
		{name: "scoped to one project", claim: "urn:zitadel:iam:org:project:275672248377933829:roles"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			token := jwt(t, map[string]any{
				"sub": "1",
				"exp": time.Now().Add(time.Hour).Unix(),
				c.claim: map[string]any{
					"internal-app": map[string]string{"org": "example.com"},
				},
			})
			claims, err := ParseClaims(token)
			if err != nil {
				t.Fatal(err)
			}
			roles := claims.RoleNames()
			if len(roles) != 1 || roles[0] != "internal-app" {
				t.Errorf("roles = %v, want [internal-app]", roles)
			}
		})
	}
}

// TestParseClaimsMergesTwoRoleClaimsAtOnce covers a token carrying both shapes.
func TestParseClaimsMergesTwoRoleClaimsAtOnce(t *testing.T) {
	token := jwt(t, map[string]any{
		"sub": "1",
		"urn:zitadel:iam:org:project:roles": map[string]any{
			"admin": map[string]string{"o": "d"},
		},
		"urn:zitadel:iam:org:project:99:roles": map[string]any{
			"user": map[string]string{"o": "d"},
		},
	})
	claims, err := ParseClaims(token)
	if err != nil {
		t.Fatal(err)
	}
	// RoleNames is sorted, so this comparison is stable.
	if got := claims.RoleNames(); len(got) != 2 || got[0] != "admin" || got[1] != "user" {
		t.Errorf("roles = %v, want [admin user]", got)
	}
}

// TestParseClaimsIgnoresLookalikeClaims guards the pattern against matching something that
// merely resembles the roles claim.
func TestParseClaimsIgnoresLookalikeClaims(t *testing.T) {
	token := jwt(t, map[string]any{
		"sub":          "1",
		"matrix_roles": []string{"internal-app"},
		"roles":        map[string]any{"nope": map[string]string{"o": "d"}},
		"urn:zitadel:iam:org:project:roles:extra": map[string]any{
			"nope": map[string]string{"o": "d"},
		},
	})
	claims, err := ParseClaims(token)
	if err != nil {
		t.Fatal(err)
	}
	if got := claims.RoleNames(); len(got) != 0 {
		t.Errorf("roles = %v, want none", got)
	}
}

func TestParseClaimsAudienceBothShapes(t *testing.T) {
	one, err := ParseClaims(jwt(t, map[string]any{"sub": "1", "aud": "a"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Aud) != 1 || one.Aud[0] != "a" {
		t.Errorf("string aud = %v", one.Aud)
	}
	many, err := ParseClaims(jwt(t, map[string]any{"sub": "1", "aud": []string{"a", "b"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(many.Aud) != 2 {
		t.Errorf("array aud = %v", many.Aud)
	}
}

// TestParseClaimsRejectsAnOpaqueToken covers the misconfiguration that produces the least
// helpful symptom: a machine user left on the default Bearer token type.
func TestParseClaimsRejectsAnOpaqueToken(t *testing.T) {
	_, err := ParseClaims("not-a-jwt")
	if err == nil {
		t.Fatal("an opaque token was accepted as a JWT")
	}
	if !contains(err.Error(), "Access Token Type = JWT") {
		t.Errorf("the error does not name the fix: %v", err)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestTokensExpiryPrefersTheClaim: expires_in is meaningless after a file round-trip unless
// ObtainedAt came with it, while the `exp` claim is self-contained AND is what the callout
// reads.
func TestTokensExpiry(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	withClaim := Tokens{AccessToken: jwt(t, map[string]any{"sub": "1", "exp": exp.Unix()})}
	if got := withClaim.Expiry(); !got.Equal(exp) {
		t.Errorf("expiry from the claim = %v, want %v", got, exp)
	}
	if !withClaim.Valid() {
		t.Error("a token with two hours left is not valid")
	}

	// An opaque token falls back to expires_in + ObtainedAt.
	opaque := Tokens{AccessToken: "opaque", ExpiresIn: 3600, ObtainedAt: time.Now()}
	if opaque.Expiry().IsZero() {
		t.Error("expires_in + ObtainedAt produced no expiry")
	}
	if !opaque.Valid() {
		t.Error("an opaque token with an hour left is not valid")
	}

	// An unknown expiry counts as valid: the authority is the callout, not us.
	unknown := Tokens{AccessToken: "opaque"}
	if !unknown.Valid() {
		t.Error("a token with an unknown expiry should be treated as valid")
	}

	// And no token is never valid.
	if (Tokens{}).Valid() {
		t.Error("an empty token is valid")
	}
}

// TestTokensValidHonoursTheSkew: a token expiring inside the skew window is already unusable,
// because it can expire while the callout is validating it.
func TestTokensValidHonoursTheSkew(t *testing.T) {
	soon := time.Now().Add(refreshSkew / 2)
	tok := Tokens{AccessToken: jwt(t, map[string]any{"sub": "1", "exp": soon.Unix()})}
	if tok.Valid() {
		t.Error("a token expiring within the skew window was treated as valid")
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := &FileStore{Path: filepath.Join(dir, "tokens.json")}

	// A missing file is not an error: nobody has logged in yet.
	got, err := store.Load()
	if err != nil {
		t.Fatalf("loading a missing file: %v", err)
	}
	if got.AccessToken != "" {
		t.Error("a missing file produced a token")
	}

	want := Tokens{AccessToken: "a", RefreshToken: "r", ExpiresIn: 60, ObtainedAt: time.Now()}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "a" || got.RefreshToken != "r" {
		t.Errorf("round trip lost data: %+v", got)
	}

	// The file holds a refresh token, so the permissions matter.
	info, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %04o, want 0600", perm)
	}
}

// TestFileStoreSaveIsAtomic: a save must not leave a truncated file behind, because that would
// cost the user another trip to the browser.
func TestFileStoreSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	store := &FileStore{Path: filepath.Join(dir, "tokens.json")}
	if err := store.Save(Tokens{AccessToken: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Tokens{AccessToken: "second"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "second" {
		t.Errorf("token = %q, want the second write", got.AccessToken)
	}
	// No temp files left over.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("the directory holds %v, want only the tokens file", names)
	}
}

func TestFileStoreRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&FileStore{Path: path}).Load(); err == nil {
		t.Fatal("garbage was accepted")
	} else if !contains(err.Error(), "jiku login") {
		t.Errorf("the error does not say how to recover: %v", err)
	}
}

// TestServiceUserRejectsBadKeys covers the messages a first-time integrator will actually see.
func TestServiceUserRejectsBadKeys(t *testing.T) {
	cases := []struct {
		name string
		cfg  ServiceUserConfig
	}{
		{name: "no issuer", cfg: ServiceUserConfig{Key: []byte(`{}`)}},
		{name: "no key at all", cfg: ServiceUserConfig{Issuer: "https://x"}},
		{name: "not JSON", cfg: ServiceUserConfig{Issuer: "https://x", Key: []byte(`nope`)}},
		{name: "JSON without the fields", cfg: ServiceUserConfig{Issuer: "https://x", Key: []byte(`{"a":1}`)}},
		{name: "a key that is not PEM", cfg: ServiceUserConfig{Issuer: "https://x",
			Key: []byte(`{"keyId":"1","userId":"2","key":"nope"}`)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewServiceUser(c.cfg); err == nil {
				t.Error("a bad config was accepted")
			}
		})
	}
}

// TestServiceUserDefaultsIncludeProfile pins the scope default that makes identity sync work.
//
// With `openid` alone, userinfo returns no name, the auth-callout publishes a nameless event,
// core discards it, no row is created in `users`, and every request is refused with
// caller_not_authorized — three services away from the cause.
func TestServiceUserDefaultsIncludeProfile(t *testing.T) {
	key := generateTestKey(t)
	su, err := NewServiceUser(ServiceUserConfig{Issuer: "https://x", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(su.scopes(), "profile") {
		t.Errorf("default scopes = %q, want profile included", su.scopes())
	}
	if su.UserID() != "42" {
		t.Errorf("UserID = %q, want 42", su.UserID())
	}
}

// TestServiceUserScopesAddTheReservedZitadelOnes: without them the token carries no roles and
// the callout has no rule to match.
func TestServiceUserScopesAddTheReservedZitadelOnes(t *testing.T) {
	su, err := NewServiceUser(ServiceUserConfig{
		Issuer: "https://x", Key: generateTestKey(t), ProjectID: "123",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"urn:zitadel:iam:org:projects:roles",
		"urn:zitadel:iam:org:project:id:123:aud",
	} {
		if !contains(su.scopes(), want) {
			t.Errorf("scopes %q are missing %q", su.scopes(), want)
		}
	}
}

// TestServiceUserAssertion checks the JWT profile assertion: iss and sub are both the machine
// user (it asserts its own identity), and the audience defaults to the issuer.
func TestServiceUserAssertion(t *testing.T) {
	su, err := NewServiceUser(ServiceUserConfig{Issuer: "https://x/", Key: generateTestKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := su.assertion()
	if err != nil {
		t.Fatal(err)
	}
	claims, err := ParseClaims(assertion)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Sub != "42" || claims.Iss != "42" {
		t.Errorf("iss=%q sub=%q, want both 42", claims.Iss, claims.Sub)
	}
	// The trailing slash of the issuer must not end up in the audience.
	if len(claims.Aud) != 1 || claims.Aud[0] != "https://x" {
		t.Errorf("aud = %v, want [https://x]", claims.Aud)
	}
	if claims.Exp <= claims.Iat {
		t.Error("the assertion does not expire after it was issued")
	}
}

func TestDeviceFlowRequiresIssuerAndClient(t *testing.T) {
	if _, err := NewDeviceFlow(DeviceConfig{ClientID: "c"}); err == nil {
		t.Error("a device flow with no issuer was accepted")
	}
	if _, err := NewDeviceFlow(DeviceConfig{Issuer: "https://x"}); err == nil {
		t.Error("a device flow with no client id was accepted")
	}
}

// TestDeviceFlowTokenNeverBlocksOnAHuman: Token must return ErrLoginRequired rather than
// starting an interactive flow, so a service can never be surprised by a call that waits for a
// browser.
func TestDeviceFlowTokenNeverBlocksOnAHuman(t *testing.T) {
	flow, err := NewDeviceFlow(DeviceConfig{
		Issuer: "https://x", ClientID: "c", Store: &MemoryStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = flow.Token(testContext())
	if err == nil {
		t.Fatal("a token appeared from an empty store")
	}
	if !contains(err.Error(), "jiku login") {
		t.Errorf("the error does not say what to run: %v", err)
	}
}

// TestDeviceFlowScopesIncludeOfflineAccess: without it there is no refresh token and every
// expiry means another trip to the browser.
func TestDeviceFlowScopesIncludeOfflineAccess(t *testing.T) {
	flow, err := NewDeviceFlow(DeviceConfig{Issuer: "https://x", ClientID: "c", ProjectID: "9"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"offline_access",
		"urn:zitadel:iam:org:projects:roles",
		"urn:zitadel:iam:org:project:id:9:aud",
	} {
		if !contains(flow.scopes(), want) {
			t.Errorf("scopes %q are missing %q", flow.scopes(), want)
		}
	}
}

func testContext() context.Context { return context.Background() }
