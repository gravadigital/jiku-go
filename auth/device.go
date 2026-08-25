package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// DeviceConfig configures the device authorization grant (RFC 8628) — the flow for a PERSON at
// a terminal.
//
// It suits a human because the browser does the authenticating: you get a short code, approve
// it once, and the terminal receives the tokens. It is the wrong tool for unattended work
// (cron, CI, a long-running service) because somebody has to click. Use NewServiceUser there.
type DeviceConfig struct {
	// Issuer is the Zitadel instance, e.g. https://id.grava.io.
	Issuer string
	// ClientID of a NATIVE app in Zitadel with the "Device Code" grant type enabled. Without
	// that grant the token endpoint answers unauthorized_client.
	ClientID string
	// ProjectID, when set, adds the two reserved Zitadel scopes that make the token usable
	// on this bus:
	//
	//   urn:zitadel:iam:org:projects:roles          puts the roles in the token
	//   urn:zitadel:iam:org:project:id:<id>:aud     puts the project in the `aud` claim
	//
	// The callout reads the ROLE to choose a permission template, so a token without the
	// roles claim connects to nothing. This is the field people forget.
	ProjectID string
	// Scopes to request. Defaults to openid, profile, email and offline_access.
	//
	// offline_access is what yields a refresh token; without it every expiry means another
	// trip to the browser.
	Scopes []string
	// Store persists the tokens between runs. Without one the flow is interactive on every
	// call, which is almost never what you want.
	Store Store
	// Prompt displays the verification URL and code. Defaults to writing to stderr and
	// trying to open a browser.
	Prompt func(DeviceAuth) error
	// HTTPClient overrides the HTTP client.
	HTTPClient *http.Client
	// NoBrowser skips the attempt to open a browser.
	NoBrowser bool
}

// DeviceAuth is what the device authorization endpoint answered.
type DeviceAuth struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// DeviceFlow is a TokenSource backed by the device authorization grant.
//
// It refreshes silently while a refresh token is usable and only asks for a browser when it
// genuinely has to — which for the ~20h tokens this instance issues is rarely.
type DeviceFlow struct {
	cfg DeviceConfig

	mu     sync.Mutex
	tokens Tokens
	loaded bool
}

// ErrLoginRequired means no stored token is usable and the flow needs a browser. Callers that
// must not block — a service, a request handler — should treat it as fatal and tell the
// operator to run `jiku login`.
var ErrLoginRequired = errors.New("auth: login required")

// NewDeviceFlow builds a device-flow token source.
func NewDeviceFlow(cfg DeviceConfig) (*DeviceFlow, error) {
	if cfg.Issuer == "" {
		return nil, fmt.Errorf("auth: device flow needs an Issuer")
	}
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("auth: device flow needs a ClientID")
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid", "profile", "email", "offline_access"}
	}
	if cfg.Prompt == nil {
		cfg.Prompt = defaultPrompt(cfg.NoBrowser)
	}
	return &DeviceFlow{cfg: cfg}, nil
}

// scopes assembles the requested scopes plus the two reserved Zitadel ones.
//
// They are built here rather than being asked of the caller so the project id appears exactly
// once and in the right shape. A refresh must request a SUBSET of the original scopes, and
// omitting them there can return a renewed token WITHOUT the roles claim — which would connect
// to nothing. So the same set is used for both.
func (d *DeviceFlow) scopes() string {
	s := append([]string(nil), d.cfg.Scopes...)
	if d.cfg.ProjectID != "" {
		s = append(s,
			"urn:zitadel:iam:org:projects:roles",
			"urn:zitadel:iam:org:project:id:"+d.cfg.ProjectID+":aud",
		)
	}
	return strings.Join(s, " ")
}

// Token returns a valid access token, refreshing or loading from the store as needed. It never
// starts an interactive flow — that is Login's job — and returns ErrLoginRequired instead, so
// a service can never be surprised by a call that blocks on a human.
func (d *DeviceFlow) Token(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.load(); err != nil {
		return "", err
	}
	if d.tokens.Valid() {
		return d.tokens.AccessToken, nil
	}
	if d.tokens.RefreshToken != "" {
		if err := d.refresh(ctx); err == nil {
			return d.tokens.AccessToken, nil
		} else if !errors.Is(err, ErrLoginRequired) {
			return "", err
		}
	}
	return "", fmt.Errorf("%w: run `jiku login`", ErrLoginRequired)
}

// Subject is the `sub` of the logged-in person.
func (d *DeviceFlow) Subject(ctx context.Context) (string, error) {
	tok, err := d.Token(ctx)
	if err != nil {
		return "", err
	}
	c, err := ParseClaims(tok)
	if err != nil {
		return "", err
	}
	if c.Sub == "" {
		return "", fmt.Errorf("auth: the access token carries no `sub` claim")
	}
	return c.Sub, nil
}

// Claims returns the current token's claims, for `jiku whoami`.
func (d *DeviceFlow) Claims(ctx context.Context) (Claims, error) {
	tok, err := d.Token(ctx)
	if err != nil {
		return Claims{}, err
	}
	return ParseClaims(tok)
}

// Login runs the interactive flow: it prints a code, waits for the authorization, and stores
// the tokens. It is what `jiku login` calls.
func (d *DeviceFlow) Login(ctx context.Context) (Tokens, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	disc, err := Discover(ctx, d.cfg.HTTPClient, d.cfg.Issuer)
	if err != nil {
		return Tokens{}, err
	}
	if disc.DeviceAuthorizationEndpoint == "" {
		return Tokens{}, fmt.Errorf(
			"auth: %s publishes no device_authorization_endpoint, so the device flow is not "+
				"available on this instance", d.cfg.Issuer)
	}

	auth, err := d.authorize(ctx, disc)
	if err != nil {
		return Tokens{}, err
	}
	if err := d.cfg.Prompt(auth); err != nil {
		return Tokens{}, err
	}

	tokens, err := d.poll(ctx, disc, auth)
	if err != nil {
		return Tokens{}, err
	}
	d.tokens, d.loaded = tokens, true
	if d.cfg.Store != nil {
		if err := d.cfg.Store.Save(tokens); err != nil {
			return tokens, fmt.Errorf("auth: the login succeeded but storing it failed: %w", err)
		}
	}
	return tokens, nil
}

// authorize is the POST that starts the flow and yields the user code.
func (d *DeviceFlow) authorize(ctx context.Context, disc Discovery) (DeviceAuth, error) {
	form := url.Values{
		"client_id": {d.cfg.ClientID},
		"scope":     {d.scopes()},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		disc.DeviceAuthorizationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return DeviceAuth{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client(d.cfg.HTTPClient).Do(req)
	if err != nil {
		return DeviceAuth{}, fmt.Errorf("auth: starting the device flow: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		var te tokenError
		if err := json.Unmarshal(body, &te); err == nil && te.Code != "" {
			te.Status = resp.StatusCode
			return DeviceAuth{}, &te
		}
		return DeviceAuth{}, fmt.Errorf("auth: device authorization answered %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var da DeviceAuth
	if err := json.Unmarshal(body, &da); err != nil {
		return DeviceAuth{}, fmt.Errorf("auth: parsing the device authorization: %w", err)
	}
	if da.Interval <= 0 {
		da.Interval = 5
	}
	if da.ExpiresIn <= 0 {
		da.ExpiresIn = 300
	}
	return da, nil
}

// poll waits for the person to authorise, honouring the two codes RFC 8628 defines for it:
// authorization_pending means keep going, slow_down means back off permanently.
func (d *DeviceFlow) poll(ctx context.Context, disc Discovery, auth DeviceAuth) (Tokens, error) {
	interval := time.Duration(auth.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(auth.ExpiresIn) * time.Second)

	for {
		select {
		case <-ctx.Done():
			return Tokens{}, ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return Tokens{}, fmt.Errorf(
				"auth: the device code expired after %ds without being authorized", auth.ExpiresIn)
		}

		tokens, err := postForm(ctx, d.cfg.HTTPClient, disc.TokenEndpoint, url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {auth.DeviceCode},
			"client_id":   {d.cfg.ClientID},
		})
		if err == nil {
			return tokens, nil
		}

		var te *tokenError
		if !errors.As(err, &te) {
			return Tokens{}, err
		}
		switch te.Code {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
		default:
			return Tokens{}, err
		}
	}
}

// refresh renews the access token. Zitadel ROTATES the refresh token on every use, so the new
// one is kept; when a response carries none, the previous one is preserved rather than dropped.
func (d *DeviceFlow) refresh(ctx context.Context) error {
	disc, err := Discover(ctx, d.cfg.HTTPClient, d.cfg.Issuer)
	if err != nil {
		return err
	}
	tokens, err := postForm(ctx, d.cfg.HTTPClient, disc.TokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {d.tokens.RefreshToken},
		"client_id":     {d.cfg.ClientID},
		"scope":         {d.scopes()},
	})
	if err != nil {
		var te *tokenError
		if errors.As(err, &te) && (te.Code == "invalid_grant" || te.Code == "invalid_token") {
			return fmt.Errorf("%w: the refresh token was rejected (%s)", ErrLoginRequired, te.Code)
		}
		return err
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = d.tokens.RefreshToken
	}
	d.tokens = tokens
	if d.cfg.Store != nil {
		return d.cfg.Store.Save(tokens)
	}
	return nil
}

// load reads the store once, lazily. A store that holds nothing is not an error: it just means
// nobody has logged in yet.
func (d *DeviceFlow) load() error {
	if d.loaded {
		return nil
	}
	d.loaded = true
	if d.cfg.Store == nil {
		return nil
	}
	tokens, err := d.cfg.Store.Load()
	if err != nil {
		return err
	}
	d.tokens = tokens
	return nil
}

// defaultPrompt writes the code to stderr — never stdout, which belongs to the command's
// output — and tries to open a browser.
func defaultPrompt(noBrowser bool) func(DeviceAuth) error {
	return func(a DeviceAuth) error {
		target := a.VerificationURIComplete
		if target == "" {
			target = a.VerificationURI
		}
		fmt.Fprintf(stderr, "\n  Open:  %s\n", target)
		if a.VerificationURIComplete == "" {
			fmt.Fprintf(stderr, "  Code:  %s\n", a.UserCode)
		} else {
			fmt.Fprintf(stderr, "  Code:  %s  (already in the link)\n", a.UserCode)
		}
		fmt.Fprintf(stderr, "\n  Waiting for authorization (expires in %ds)...\n", a.ExpiresIn)
		if !noBrowser {
			openBrowser(target)
		}
		return nil
	}
}

// openBrowser is best-effort: a headless machine is a normal place to run this, and the code
// is on screen either way.
func openBrowser(u string) {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "rundll32"
	default:
		cmd = "xdg-open"
	}
	if cmd == "rundll32" {
		_ = exec.Command(cmd, "url.dll,FileProtocolHandler", u).Start()
		return
	}
	_ = exec.Command(cmd, u).Start()
}
