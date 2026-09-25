package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Discovery is the subset of the OpenID provider metadata this package uses.
//
// The endpoints are READ FROM THE WELL-KNOWN, never hardcoded, so the same code works against
// any Zitadel instance and keeps working if one moves a path.
type Discovery struct {
	Issuer                      string `json:"issuer"`
	TokenEndpoint               string `json:"token_endpoint"`
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	UserinfoEndpoint            string `json:"userinfo_endpoint"`
}

type discoveryCache struct {
	mu   sync.Mutex
	seen map[string]Discovery
}

var discoveries = discoveryCache{seen: map[string]Discovery{}}

// DiscoveryTTL is how long a discovery document cached on disk is reused.
//
// The document is a set of endpoint URLs that change about as often as the deployment does, so
// the TTL is generous. It is not a security boundary: nothing here is trusted, and a stale
// entry is corrected by ForgetDiscovery and a retry — see ServiceUser.Token — rather than by
// waiting out the clock.
const DiscoveryTTL = 24 * time.Hour

// cachedDiscovery is one disk-cached discovery document with the time it was fetched.
type cachedDiscovery struct {
	Discovery Discovery `json:"discovery"`
	FetchedAt time.Time `json:"fetched_at"`
}

// discoveryCacheDir is overridden in tests. Empty means the conventional config dir.
var discoveryCacheDir string

// discoveryCachePath is the file an issuer's document is cached in. The issuer is hashed
// rather than escaped: it is a URL, and a URL does not reliably survive being a filename.
func discoveryCachePath(issuer string) string {
	dir := discoveryCacheDir
	if dir == "" {
		dir = ConfigDir()
	}
	sum := sha256.Sum256([]byte(issuer))
	name := "discovery-" + base64.RawURLEncoding.EncodeToString(sum[:12]) + ".json"
	return filepath.Join(dir, name)
}

// loadDiscoveryCache reads a non-expired document from disk. Every failure is silent and means
// "no cache": this is an optimisation, and the fallback is the fetch that would have happened.
func loadDiscoveryCache(issuer string) (Discovery, bool) {
	b, err := os.ReadFile(discoveryCachePath(issuer))
	if err != nil {
		return Discovery{}, false
	}
	var c cachedDiscovery
	if err := json.Unmarshal(b, &c); err != nil {
		return Discovery{}, false
	}
	if c.Discovery.TokenEndpoint == "" || time.Since(c.FetchedAt) > DiscoveryTTL {
		return Discovery{}, false
	}
	return c.Discovery, true
}

// saveDiscoveryCache writes the document for the next process. It holds no secret — these are
// public endpoint URLs — so it is written 0644 under a 0700 directory.
func saveDiscoveryCache(issuer string, d Discovery) {
	path := discoveryCachePath(issuer)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	b, err := json.Marshal(cachedDiscovery{Discovery: d, FetchedAt: time.Now()})
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".discovery-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), path)
}

// ForgetDiscovery drops an issuer's cached document, in memory and on disk.
//
// It is what makes the disk cache safe to keep for a day: a caller that finds the cached
// endpoints no longer working calls this and retries, which re-fetches. Without it a moved
// endpoint would be a day-long outage fixed only by deleting a file nobody knows about.
func ForgetDiscovery(issuer string) {
	issuer = strings.TrimSuffix(issuer, "/")
	discoveries.mu.Lock()
	delete(discoveries.seen, issuer)
	discoveries.mu.Unlock()
	_ = os.Remove(discoveryCachePath(issuer))
}

// Discover fetches (and memoises) the provider metadata for an issuer.
//
// There are three layers, cheapest first: this process's memory, a disk cache shared between
// runs (see DiscoveryTTL), and the issuer itself. The disk layer is what stops a one-shot
// process — the CLI — from paying a full HTTPS handshake to learn URLs that have not moved.
func Discover(ctx context.Context, hc *http.Client, issuer string) (Discovery, error) {
	d, _, err := discover(ctx, hc, issuer)
	return d, err
}

// discover is Discover, also reporting whether the document came from a cache rather than from
// the issuer. A caller that fails against cached endpoints can retry; one that just fetched
// them has nothing fresher to try.
func discover(ctx context.Context, hc *http.Client, issuer string) (Discovery, bool, error) {
	issuer = strings.TrimSuffix(issuer, "/")
	if issuer == "" {
		return Discovery{}, false, fmt.Errorf("auth: no issuer configured")
	}

	tr := tracerFrom(ctx)
	start := time.Now()
	from := func(where string) {
		tr.set(func(t *TokenTrace) { t.Discovery += time.Since(start); t.DiscoveryFrom = where })
	}

	discoveries.mu.Lock()
	if d, ok := discoveries.seen[issuer]; ok {
		discoveries.mu.Unlock()
		from("memory")
		return d, true, nil
	}
	discoveries.mu.Unlock()

	if d, ok := loadDiscoveryCache(issuer); ok {
		discoveries.mu.Lock()
		discoveries.seen[issuer] = d
		discoveries.mu.Unlock()
		from("disk")
		return d, true, nil
	}
	defer from("network")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return Discovery{}, false, err
	}
	req, done := traceHTTP(req, "discovery")
	resp, err := client(hc).Do(req)
	if err != nil {
		done(0, err)
		return Discovery{}, false, fmt.Errorf("auth: reaching %s: %w", issuer, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	done(resp.StatusCode, err)
	if err != nil {
		return Discovery{}, false, fmt.Errorf("auth: reading discovery: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Discovery{}, false, fmt.Errorf(
			"auth: discovery of %s answered %d — is that the right issuer URL?",
			issuer, resp.StatusCode)
	}

	var d Discovery
	if err := json.Unmarshal(body, &d); err != nil {
		return Discovery{}, false, fmt.Errorf("auth: parsing discovery of %s: %w", issuer, err)
	}
	if d.TokenEndpoint == "" {
		return Discovery{}, false, fmt.Errorf("auth: %s publishes no token_endpoint", issuer)
	}

	discoveries.mu.Lock()
	discoveries.seen[issuer] = d
	discoveries.mu.Unlock()
	saveDiscoveryCache(issuer, d)
	return d, false, nil
}

// postForm sends a form-urlencoded request to a token endpoint and decodes either Tokens or
// an OAuth error, which is the one shape both flows share.
func postForm(ctx context.Context, hc *http.Client, endpoint string, form url.Values) (Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	req, done := traceHTTP(req, "token")
	resp, err := client(hc).Do(req)
	if err != nil {
		done(0, err)
		return Tokens{}, fmt.Errorf("auth: reaching the token endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	done(resp.StatusCode, err)
	if err != nil {
		return Tokens{}, fmt.Errorf("auth: reading the token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var te tokenError
		if err := json.Unmarshal(body, &te); err == nil && te.Code != "" {
			te.Status = resp.StatusCode
			return Tokens{}, &te
		}
		return Tokens{}, fmt.Errorf("auth: token endpoint answered %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var t Tokens
	if err := json.Unmarshal(body, &t); err != nil {
		return Tokens{}, fmt.Errorf("auth: parsing the token response: %w", err)
	}
	if t.AccessToken == "" {
		return Tokens{}, fmt.Errorf("auth: the token endpoint returned no access_token")
	}
	t.ObtainedAt = time.Now()
	return t, nil
}

func client(hc *http.Client) *http.Client {
	if hc != nil {
		return hc
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Claims is the part of an access token this library reads.
//
// # THIS IS NOT A VALIDATION
//
// The signature is NOT verified and cannot be trusted for any security decision. It is read
// for three local, non-security purposes: to know the caller's own `sub` (for the subject and
// the inbox prefix), to decide locally whether to refresh before expiry, and to tell a person
// which roles they hold in `jiku whoami`. Whoever validates the token is the auth-callout.
type Claims struct {
	Sub   string   `json:"sub"`
	Exp   int64    `json:"exp"`
	Iat   int64    `json:"iat"`
	Iss   string   `json:"iss"`
	Aud   Audience `json:"aud"`
	Email string   `json:"email,omitempty"`
	Name  string   `json:"name,omitempty"`

	// Roles are the Zitadel project roles: role -> org id -> org domain.
	//
	// Zitadel emits these under TWO different claim keys, and which one you get depends on
	// the request rather than on anything you control:
	//
	//	urn:zitadel:iam:org:project:roles          the roles of every project
	//	urn:zitadel:iam:org:project:<id>:roles     the roles of ONE project
	//
	// A person's token from the device flow tends to carry the first, a machine user's the
	// second. Both are merged here, because for the purpose this is read for — telling
	// somebody which roles they hold — the distinction is noise. The auth-callout reads the
	// project-scoped one when it is configured with a project id, precisely because a
	// same-named role in another project must not match a rule.
	//
	// Present only when the `urn:zitadel:iam:org:projects:roles` scope was requested.
	Roles map[string]map[string]string `json:"-"`
}

// zitadelRoleClaim matches both shapes of Zitadel's project-roles claim.
var zitadelRoleClaim = regexp.MustCompile(`^urn:zitadel:iam:org:project:(?:[^:]+:)?roles$`)

// UnmarshalJSON decodes the standard claims and then merges whichever role claims are present.
func (c *Claims) UnmarshalJSON(b []byte) error {
	type plain struct {
		Sub   string   `json:"sub"`
		Exp   int64    `json:"exp"`
		Iat   int64    `json:"iat"`
		Iss   string   `json:"iss"`
		Aud   Audience `json:"aud"`
		Email string   `json:"email,omitempty"`
		Name  string   `json:"name,omitempty"`
	}
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*c = Claims{Sub: p.Sub, Exp: p.Exp, Iat: p.Iat, Iss: p.Iss, Aud: p.Aud,
		Email: p.Email, Name: p.Name}

	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	for key, raw := range all {
		if !zitadelRoleClaim.MatchString(key) {
			continue
		}
		var roles map[string]map[string]string
		if err := json.Unmarshal(raw, &roles); err != nil {
			continue
		}
		if c.Roles == nil {
			c.Roles = map[string]map[string]string{}
		}
		for role, orgs := range roles {
			if c.Roles[role] == nil {
				c.Roles[role] = map[string]string{}
			}
			for org, domain := range orgs {
				c.Roles[role][org] = domain
			}
		}
	}
	return nil
}

// RoleNames lists the roles in the token, which is what decides bus permissions.
func (c Claims) RoleNames() []string {
	out := make([]string, 0, len(c.Roles))
	for r := range c.Roles {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Audience decodes the `aud` claim, which OIDC allows to be either a string or an array.
type Audience []string

func (a *Audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = Audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// ParseClaims decodes a JWT's payload WITHOUT verifying the signature. See Claims.
func ParseClaims(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, fmt.Errorf("auth: the access token is not a JWT (%d segments); "+
			"a machine user needs Access Token Type = JWT in Zitadel for the auth-callout "+
			"to read it", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("auth: decoding the token payload: %w", err)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, fmt.Errorf("auth: parsing the token claims: %w", err)
	}
	return c, nil
}
