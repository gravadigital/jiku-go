package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ServiceUserConfig configures the JWT profile grant (RFC 7523) — the flow for a SERVICE.
//
// This is what an unattended integration should use: no browser, no refresh token, no stored
// state. The private key IS the credential, and a fresh access token is minted whenever one is
// needed.
//
// # WHERE THE KEY FILE COMES FROM
//
// In Zitadel: create a machine user, set Access Token Type to JWT, grant it a role in the
// project, then add a KEY to it. Zitadel downloads a JSON file exactly once, shaped like:
//
//	{"type":"serviceaccount","keyId":"...","key":"-----BEGIN RSA PRIVATE KEY-----\n...",
//	 "userId":"...","expirationDate":"..."}
//
// That file is the credential. It cannot be re-downloaded — only replaced by a new key.
//
// # ACCESS TOKEN TYPE MUST BE JWT
//
// The auth-callout validates the token as a JWT against the issuer. A machine user left on the
// default opaque "Bearer" token type gets a token the callout cannot read, and the connection
// is refused. This is the single most common misconfiguration of this flow.
type ServiceUserConfig struct {
	// Issuer is the Zitadel instance, e.g. https://id.grava.io.
	Issuer string
	// KeyFile is the path to the service account JSON key. Either this or Key is required.
	KeyFile string
	// Key is the service account JSON key inline, for callers that hold it in a secret
	// manager rather than on disk. Takes precedence over KeyFile.
	Key []byte
	// ProjectID adds the two reserved Zitadel scopes. As with the device flow, the roles
	// claim is what the callout reads to pick a permission template, so a token minted
	// without it connects to nothing.
	ProjectID string
	// Scopes to request. Defaults to openid and profile.
	//
	// `profile` LOOKS UNNECESSARY FOR A SERVICE AND IS NOT. Jiku's auth-callout publishes an
	// authentication event that core turns into a row in `users`, and core REQUIRES a name on
	// that event. A machine user's name reaches the callout through the userinfo endpoint,
	// which only returns it when `profile` was requested — so a token minted with `openid`
	// alone produces a nameless event, core discards it, no row is created, and every
	// subsequent request is refused with `caller_not_authorized`.
	//
	// The failure is silent and lands three services away from its cause: the bus accepts the
	// connection, the callout logs a success, and only core's log says
	// `[events] descartado: "name" is required`.
	Scopes []string
	// Audience overrides the `aud` of the signed assertion. Defaults to the issuer, which
	// is what Zitadel expects.
	Audience string
	// AssertionTTL is the lifetime of the signed assertion. Defaults to one hour, the
	// maximum Zitadel accepts.
	AssertionTTL time.Duration
	// HTTPClient overrides the HTTP client.
	HTTPClient *http.Client
}

// ServiceAccountKey is the JSON key file Zitadel produces for a machine user.
type ServiceAccountKey struct {
	Type           string `json:"type"`
	KeyID          string `json:"keyId"`
	Key            string `json:"key"`
	UserID         string `json:"userId"`
	ExpirationDate string `json:"expirationDate,omitempty"`
}

// ServiceUser is a TokenSource backed by a Zitadel service account key.
//
// It caches the access token in memory and mints a new one when the cached one nears expiry,
// so a long-lived connection that reconnects always presents a live token. Nothing is written
// to disk: the key is the only durable state, and it is the caller's to manage.
type ServiceUser struct {
	cfg  ServiceUserConfig
	key  ServiceAccountKey
	priv *rsa.PrivateKey

	mu     sync.Mutex
	tokens Tokens
}

// NewServiceUser builds a service-user token source from a Zitadel service account key.
func NewServiceUser(cfg ServiceUserConfig) (*ServiceUser, error) {
	if cfg.Issuer == "" {
		return nil, fmt.Errorf("auth: service user needs an Issuer")
	}
	raw := cfg.Key
	if len(raw) == 0 {
		if cfg.KeyFile == "" {
			return nil, fmt.Errorf("auth: service user needs a KeyFile or an inline Key")
		}
		var err error
		raw, err = os.ReadFile(cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("auth: reading the service account key: %w", err)
		}
	}

	var key ServiceAccountKey
	if err := json.Unmarshal(raw, &key); err != nil {
		return nil, fmt.Errorf(
			"auth: parsing the service account key: %w — this must be the JSON file Zitadel "+
				"produced for a machine user, not a PEM on its own", err)
	}
	if key.Key == "" || key.KeyID == "" || key.UserID == "" {
		return nil, fmt.Errorf(
			"auth: the service account key is missing key, keyId or userId; it does not look " +
				"like a Zitadel machine user key")
	}
	priv, err := parseRSAPrivateKey([]byte(key.Key))
	if err != nil {
		return nil, err
	}
	if cfg.AssertionTTL <= 0 {
		cfg.AssertionTTL = time.Hour
	}
	if cfg.Audience == "" {
		cfg.Audience = strings.TrimSuffix(cfg.Issuer, "/")
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid", "profile"}
	}
	return &ServiceUser{cfg: cfg, key: key, priv: priv}, nil
}

// UserID is the machine user's id, which is also the `sub` of the tokens it mints. It is
// available without a network call, straight from the key file.
func (s *ServiceUser) UserID() string { return s.key.UserID }

// Token returns a valid access token, minting a new one when needed.
func (s *ServiceUser) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.tokens.Valid() {
		return s.tokens.AccessToken, nil
	}
	disc, err := Discover(ctx, s.cfg.HTTPClient, s.cfg.Issuer)
	if err != nil {
		return "", err
	}
	assertion, err := s.assertion()
	if err != nil {
		return "", err
	}
	tokens, err := postForm(ctx, s.cfg.HTTPClient, disc.TokenEndpoint, url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
		"scope":      {s.scopes()},
	})
	if err != nil {
		return "", err
	}
	s.tokens = tokens
	return tokens.AccessToken, nil
}

// Subject is the machine user's id. It comes from the key file rather than the token, so it
// needs no network call and works before the first Token.
func (s *ServiceUser) Subject(ctx context.Context) (string, error) {
	return s.key.UserID, nil
}

// Claims returns the current token's claims, minting one if needed.
func (s *ServiceUser) Claims(ctx context.Context) (Claims, error) {
	tok, err := s.Token(ctx)
	if err != nil {
		return Claims{}, err
	}
	return ParseClaims(tok)
}

func (s *ServiceUser) scopes() string {
	sc := append([]string(nil), s.cfg.Scopes...)
	if s.cfg.ProjectID != "" {
		sc = append(sc,
			"urn:zitadel:iam:org:projects:roles",
			"urn:zitadel:iam:org:project:id:"+s.cfg.ProjectID+":aud",
		)
	}
	return strings.Join(sc, " ")
}

// assertion builds and signs the JWT that stands in for the client secret.
//
// `iss` and `sub` are both the machine user's id: the key holder is asserting its own
// identity, not acting on somebody else's behalf.
func (s *ServiceUser) assertion() (string, error) {
	now := time.Now()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": s.key.KeyID}
	claims := map[string]any{
		"iss": s.key.UserID,
		"sub": s.key.UserID,
		"aud": s.cfg.Audience,
		"iat": now.Unix(),
		"exp": now.Add(s.cfg.AssertionTTL).Unix(),
	}

	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(hb) + "." + enc.EncodeToString(cb)

	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.priv, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("auth: signing the assertion: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// parseRSAPrivateKey accepts both PEM encodings Zitadel has used: PKCS#1 ("RSA PRIVATE KEY")
// and PKCS#8 ("PRIVATE KEY").
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("auth: the `key` field of the service account is not valid PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("auth: parsing the service account private key: %w", err)
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("auth: the service account key is %T, not RSA", parsed)
	}
	return k, nil
}
