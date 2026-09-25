package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// TestDeviceFlowNamesAMissingRefreshToken: a session stored without a refresh token expires
// into "run jiku login", and logging in again only restarts the clock. The cause is a grant
// missing on the Zitadel app, so the error has to say so — otherwise it is a login a day with
// nothing pointing at why.
func TestDeviceFlowNamesAMissingRefreshToken(t *testing.T) {
	stale := jwt(t, map[string]any{"sub": "42", "exp": time.Now().Add(-time.Minute).Unix()})
	flow, err := NewDeviceFlow(DeviceConfig{
		Issuer: "https://x", ClientID: "c", Store: &MemoryStore{Tokens: Tokens{AccessToken: stale}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = flow.Token(testContext())
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("want ErrLoginRequired, got %v", err)
	}
	if !contains(err.Error(), "Refresh Token") {
		t.Errorf("the error does not name the missing grant: %v", err)
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

// TestMemoryStoreConcurrency covers the promise a Store carries by being reachable from a token
// source: nats.go calls the token handler from its own goroutine on every reconnect, so a
// refresh that Saves here can run while another goroutine Loads. FileStore keeps that promise
// with an atomic rename; this one needs a lock, and without it the race detector fires.
//
// Worth only what `-race` makes it worth — `make ci` runs the tests under the detector for
// exactly this class of test.
func TestMemoryStoreConcurrency(t *testing.T) {
	store := &MemoryStore{}
	var wg sync.WaitGroup

	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := store.Save(Tokens{AccessToken: fmt.Sprintf("token-%d", i)}); err != nil {
				t.Errorf("Save: %v", err)
			}
		}(i)

		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.Load(); err != nil {
				t.Errorf("Load: %v", err)
			}
		}()
	}
	wg.Wait()

	// Whichever writer won, the value must be one somebody actually wrote rather than a
	// half-updated struct.
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.AccessToken, "token-") {
		t.Errorf("AccessToken = %q, want one of the written values", got.AccessToken)
	}
}

// TestServiceUserReusesAStoredToken is the point of the Store: a short-lived process must not
// pay a round trip to Zitadel for a token it already minted a minute ago.
//
// The token source is given a store that already holds a live token stamped with this
// credential. A mint would have to reach the network, and the issuer here is unroutable, so a
// test that passes proves the stored token was used.
func TestServiceUserReusesAStoredToken(t *testing.T) {
	key := generateTestKey(t)
	cfg := ServiceUserConfig{Issuer: "https://unroutable.invalid", Key: key}

	su, err := NewServiceUser(cfg)
	if err != nil {
		t.Fatal(err)
	}
	live := jwt(t, map[string]any{"sub": "42", "exp": time.Now().Add(time.Hour).Unix()})
	store := &MemoryStore{Tokens: Tokens{AccessToken: live, CredentialKey: su.StoreKey()}}

	cfg.Store = store
	su, err = NewServiceUser(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := su.Token(context.Background())
	if err != nil {
		t.Fatalf("Token with a valid stored token reached the network: %v", err)
	}
	if got != live {
		t.Errorf("Token = %q, want the stored one", got)
	}
}

// TestServiceUserDiscardsATokenForOtherCredentials is the safety half of the cache.
//
// Rotate the machine user's key, or change the project id, and the token on disk grants
// something other than what is being asked for now. Presenting it fails at the auth-callout,
// three services from the file that caused it — so it must be discarded instead.
func TestServiceUserDiscardsATokenForOtherCredentials(t *testing.T) {
	live := jwt(t, map[string]any{"sub": "42", "exp": time.Now().Add(time.Hour).Unix()})

	cases := []struct {
		name   string
		mutate func(*ServiceUserConfig)
	}{
		{name: "another project id", mutate: func(c *ServiceUserConfig) { c.ProjectID = "999" }},
		{name: "another issuer", mutate: func(c *ServiceUserConfig) { c.Issuer = "https://other.invalid" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := ServiceUserConfig{Issuer: "https://unroutable.invalid", Key: generateTestKey(t)}
			minted, err := NewServiceUser(base)
			if err != nil {
				t.Fatal(err)
			}

			// A token stored by the credential above, then read by a differently
			// configured one.
			now := base
			c.mutate(&now)
			now.Store = &MemoryStore{
				Tokens: Tokens{AccessToken: live, CredentialKey: minted.StoreKey()},
			}
			su, err := NewServiceUser(now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := su.Token(context.Background()); err == nil {
				t.Error("a token minted for other credentials was presented as this one's")
			}
		})
	}
}

// TestServiceUserDiscardsAnExpiredStoredToken: a cached token past its expiry is refused by the
// callout, and the symptom is an authorization violation that says nothing about time.
func TestServiceUserDiscardsAnExpiredStoredToken(t *testing.T) {
	cfg := ServiceUserConfig{Issuer: "https://unroutable.invalid", Key: generateTestKey(t)}
	su, err := NewServiceUser(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stale := jwt(t, map[string]any{"sub": "42", "exp": time.Now().Add(-time.Minute).Unix()})
	cfg.Store = &MemoryStore{Tokens: Tokens{AccessToken: stale, CredentialKey: su.StoreKey()}}

	su, err = NewServiceUser(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := su.Token(context.Background()); err == nil {
		t.Error("an expired stored token was presented instead of minting a new one")
	}
}

// TestServiceUserStoresWhatItMints checks the write half: without it every run is a mint and
// the cache never warms up. There is no network here, so this asserts the store is only
// written on a successful mint — a failed one must leave the previous entry alone.
func TestServiceUserStoresNothingOnAFailedMint(t *testing.T) {
	store := &MemoryStore{}
	su, err := NewServiceUser(ServiceUserConfig{
		Issuer: "https://unroutable.invalid", Key: generateTestKey(t), Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := su.Token(context.Background()); err == nil {
		t.Fatal("the unroutable issuer somehow answered")
	}
	if store.Tokens.AccessToken != "" {
		t.Error("a failed mint wrote to the store")
	}
}

// TestServiceUserWithNoStoreWritesNothing pins the default: a service holding a key needs no
// stored state, and writing a credential where nobody asked for one is a surprise.
func TestServiceUserWithNoStoreWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JIKU_CONFIG_DIR", dir)

	su, err := NewServiceUser(ServiceUserConfig{
		Issuer: "https://unroutable.invalid", Key: generateTestKey(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = su.Token(context.Background())

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a service user with no Store wrote %d file(s) to the config dir", len(entries))
	}
}

// discoveryServer is a stand-in issuer that counts how many times its well-known was fetched.
// Nothing leaves the machine: httptest listens on loopback.
func discoveryServer(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		*hits++
		fmt.Fprintf(w, `{"issuer":%q,"token_endpoint":%q,"userinfo_endpoint":%q}`,
			"https://issuer.example", "https://issuer.example/oauth/v2/token",
			"https://issuer.example/oidc/v1/userinfo")
	})
	return httptest.NewServer(mux)
}

// isolateDiscoveryCache points the disk cache at a temp dir and clears the in-process one, so
// each test starts cold and writes nothing to the developer's real config dir.
func isolateDiscoveryCache(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	prev := discoveryCacheDir
	discoveryCacheDir = dir
	t.Cleanup(func() { discoveryCacheDir = prev })

	discoveries.mu.Lock()
	discoveries.seen = map[string]Discovery{}
	discoveries.mu.Unlock()
	t.Cleanup(func() {
		discoveries.mu.Lock()
		discoveries.seen = map[string]Discovery{}
		discoveries.mu.Unlock()
	})
}

// TestDiscoveryIsCachedOnDisk is the whole point of the disk cache: a SECOND PROCESS must not
// re-fetch a document the first one already has. The in-process memo is cleared between the
// two calls to stand in for that second process.
func TestDiscoveryIsCachedOnDisk(t *testing.T) {
	isolateDiscoveryCache(t)
	hits := 0
	srv := discoveryServer(t, &hits)
	defer srv.Close()

	if _, err := Discover(context.Background(), srv.Client(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("the first Discover made %d request(s), want 1", hits)
	}

	// A new process: same disk, empty memory.
	discoveries.mu.Lock()
	discoveries.seen = map[string]Discovery{}
	discoveries.mu.Unlock()

	d, err := Discover(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("the second process re-fetched discovery (%d requests), want the disk cache", hits)
	}
	if d.TokenEndpoint != "https://issuer.example/oauth/v2/token" {
		t.Errorf("token_endpoint = %q, came back wrong from the cache", d.TokenEndpoint)
	}
}

// TestDiscoveryCacheExpires: the endpoints are cached, not pinned. Past the TTL the document is
// fetched again, so a deployment that moves an endpoint is picked up without anyone deleting a
// file.
func TestDiscoveryCacheExpires(t *testing.T) {
	isolateDiscoveryCache(t)
	hits := 0
	srv := discoveryServer(t, &hits)
	defer srv.Close()

	if _, err := Discover(context.Background(), srv.Client(), srv.URL); err != nil {
		t.Fatal(err)
	}

	// Backdate the cached document past the TTL, which is what a day's wait looks like.
	path := discoveryCachePath(strings.TrimSuffix(srv.URL, "/"))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c cachedDiscovery
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	c.FetchedAt = time.Now().Add(-DiscoveryTTL - time.Minute)
	if b, err = json.Marshal(c); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	discoveries.mu.Lock()
	discoveries.seen = map[string]Discovery{}
	discoveries.mu.Unlock()

	if _, err := Discover(context.Background(), srv.Client(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Errorf("an expired cache was reused (%d requests), want a re-fetch", hits)
	}
}

// TestDiscoveryCacheSurvivesGarbage: a cache file that does not parse, or that parses into a
// document with no token_endpoint, must mean "no cache" rather than a failure or — worse — a
// Discovery with empty endpoints handed to a caller that would POST to "".
//
// Each case is stamped with a FRESH FetchedAt on purpose. A zero timestamp is older than the
// TTL, so an expired-entry check would mask the corruption check and this test would pass for
// the wrong reason.
func TestDiscoveryCacheSurvivesGarbage(t *testing.T) {
	fresh := func(body string) []byte {
		return []byte(fmt.Sprintf(`{"discovery":%s,"fetched_at":%q}`,
			body, time.Now().Format(time.RFC3339Nano)))
	}
	cases := []struct {
		name    string
		content []byte
	}{
		{name: "not JSON at all", content: []byte("{not json")},
		{name: "empty file", content: nil},
		{name: "no token_endpoint", content: fresh(`{"issuer":"https://issuer.example"}`)},
		{name: "discovery is null", content: fresh(`null`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolateDiscoveryCache(t)
			hits := 0
			srv := discoveryServer(t, &hits)
			defer srv.Close()

			path := discoveryCachePath(strings.TrimSuffix(srv.URL, "/"))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, c.content, 0o600); err != nil {
				t.Fatal(err)
			}

			d, err := Discover(context.Background(), srv.Client(), srv.URL)
			if err != nil {
				t.Fatalf("a bad cache file broke discovery instead of being ignored: %v", err)
			}
			if hits != 1 {
				t.Errorf("hits = %d, want 1: the bad cache entry was used", hits)
			}
			if d.TokenEndpoint != "https://issuer.example/oauth/v2/token" {
				t.Errorf("token_endpoint = %q, want the re-fetched one", d.TokenEndpoint)
			}
		})
	}
}

// TestForgetDiscoveryClearsBothLayers: it is what lets a caller recover from endpoints that
// moved inside the TTL, so it has to clear the disk too, not just this process's memory.
func TestForgetDiscoveryClearsBothLayers(t *testing.T) {
	isolateDiscoveryCache(t)
	hits := 0
	srv := discoveryServer(t, &hits)
	defer srv.Close()

	if _, err := Discover(context.Background(), srv.Client(), srv.URL); err != nil {
		t.Fatal(err)
	}
	ForgetDiscovery(srv.URL)

	if _, err := os.Stat(discoveryCachePath(strings.TrimSuffix(srv.URL, "/"))); !os.IsNotExist(err) {
		t.Error("ForgetDiscovery left the file on disk")
	}
	if _, err := Discover(context.Background(), srv.Client(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Errorf("hits = %d after ForgetDiscovery, want a re-fetch", hits)
	}
}

// TestServiceStoreIsPrivate: the cached access token is a bearer credential until it expires.
// Anything that can read the file can act as the machine user, so the mode is part of the
// contract, not an implementation detail — the README states it.
func TestServiceStoreIsPrivate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JIKU_CONFIG_DIR", dir)

	st := DefaultServiceStore("dev")
	if err := st.Save(Tokens{AccessToken: "x", ObtainedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(st.Location())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s is mode %04o, want 0600", st.Location(), perm)
	}
}

// TestServiceStoreIsSeparateFromTheDeviceFlows: one file for both would make `jiku logout`
// choose which half of a shared file to delete, and would let a person's refresh token and a
// machine's access token overwrite each other.
func TestServiceStoreIsSeparateFromTheDeviceFlows(t *testing.T) {
	t.Setenv("JIKU_CONFIG_DIR", t.TempDir())
	if DefaultServiceStore("dev").Location() == DefaultStore("dev").Location() {
		t.Error("the service user and the device flow share a token file")
	}
}
