package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestTokenTraceNamesWhereTheTokenCameFrom pins the one question the trace exists to answer:
// did this Token call cost nothing, a file read, or a round trip to Zitadel. The issuer is
// unroutable, so reaching the network would fail the test rather than silently pass it.
func TestTokenTraceNamesWhereTheTokenCameFrom(t *testing.T) {
	cfg := ServiceUserConfig{Issuer: "https://unroutable.invalid", Key: generateTestKey(t)}
	su, err := NewServiceUser(cfg)
	if err != nil {
		t.Fatal(err)
	}
	live := jwt(t, map[string]any{"sub": "42", "exp": time.Now().Add(time.Hour).Unix()})
	cfg.Store = &MemoryStore{Tokens: Tokens{AccessToken: live, CredentialKey: su.StoreKey()}}
	if su, err = NewServiceUser(cfg); err != nil {
		t.Fatal(err)
	}

	var got []TokenTrace
	ctx := WithTrace(context.Background(), func(tr TokenTrace) { got = append(got, tr) })
	for i := 0; i < 2; i++ {
		if _, err := su.Token(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 {
		t.Fatalf("hook called %d times for 2 Token calls", len(got))
	}
	if got[0].Origin != OriginStore || got[0].Store <= 0 {
		t.Errorf("first call: origin %q, store %v; want the store, timed", got[0].Origin, got[0].Store)
	}
	if got[1].Origin != OriginMemory || got[1].Store != 0 {
		t.Errorf("second call: origin %q, store %v; want memory and no store read",
			got[1].Origin, got[1].Store)
	}
	if len(got[0].HTTP) != 0 || got[0].DiscoveryFrom != "" {
		t.Errorf("a stored token reported HTTP or discovery: %+v", got[0])
	}
}

// TestTokenTraceBreaksDownAMint checks the expensive path is itemised: discovery from the
// network, the signing, the exchange, and both HTTP requests — the second on the connection the
// first opened, which is what makes a mint two requests and one handshake.
func TestTokenTraceBreaksDownAMint(t *testing.T) {
	isolateDiscoveryCache(t)
	token := jwt(t, map[string]any{"sub": "42", "exp": time.Now().Add(time.Hour).Unix()})
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"issuer":%q,"token_endpoint":%q}`, srv.URL, srv.URL+"/oauth/v2/token")
	})
	mux.HandleFunc("/oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"access_token":%q,"token_type":"Bearer","expires_in":3600}`, token)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	su, err := NewServiceUser(ServiceUserConfig{
		Issuer: srv.URL, Key: generateTestKey(t), HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var tr TokenTrace
	ctx := WithTrace(context.Background(), func(got TokenTrace) { tr = got })
	if _, err := su.Token(ctx); err != nil {
		t.Fatal(err)
	}

	if tr.Origin != OriginMinted {
		t.Errorf("origin %q, want minted", tr.Origin)
	}
	if tr.DiscoveryFrom != "network" || tr.Discovery <= 0 {
		t.Errorf("discovery %v from %q, want timed and from the network", tr.Discovery, tr.DiscoveryFrom)
	}
	if tr.Sign <= 0 || tr.Exchange <= 0 || tr.Total < tr.Discovery+tr.Exchange {
		t.Errorf("sign %v, exchange %v, total %v: steps missing or larger than the whole",
			tr.Sign, tr.Exchange, tr.Total)
	}
	if len(tr.HTTP) != 2 || tr.HTTP[0].Step != "discovery" || tr.HTTP[1].Step != "token" {
		t.Fatalf("HTTP = %+v, want discovery then token", tr.HTTP)
	}
	if tr.HTTP[0].Reused || !tr.HTTP[1].Reused {
		t.Errorf("reused = %v, %v; want a new connection, then that one reused",
			tr.HTTP[0].Reused, tr.HTTP[1].Reused)
	}
	if tr.HTTP[1].Status != http.StatusOK {
		t.Errorf("token status %d", tr.HTTP[1].Status)
	}

	// The second process: discovery now comes from disk, and says so.
	discoveries.mu.Lock()
	discoveries.seen = map[string]Discovery{}
	discoveries.mu.Unlock()
	su2, _ := NewServiceUser(ServiceUserConfig{Issuer: srv.URL, Key: generateTestKey(t), HTTPClient: srv.Client()})
	if _, err := su2.Token(ctx); err != nil {
		t.Fatal(err)
	}
	if tr.DiscoveryFrom != "disk" || len(tr.HTTP) != 1 {
		t.Errorf("second process: discovery from %q with %d requests, want disk and only the token",
			tr.DiscoveryFrom, len(tr.HTTP))
	}
}

// TestDeviceFlowTraceTimesTheStoreRead covers the device flow's store read, which is lazy and
// happens once: the first call reports it, the second answers from memory.
func TestDeviceFlowTraceTimesTheStoreRead(t *testing.T) {
	live := jwt(t, map[string]any{"sub": "42", "exp": time.Now().Add(time.Hour).Unix()})
	d, err := NewDeviceFlow(DeviceConfig{
		Issuer: "https://unroutable.invalid", ClientID: "c",
		Store: &MemoryStore{Tokens: Tokens{AccessToken: live}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []TokenTrace
	ctx := WithTrace(context.Background(), func(tr TokenTrace) { got = append(got, tr) })
	_, _ = d.Token(ctx)
	_, _ = d.Token(ctx)
	if len(got) != 2 {
		t.Fatalf("hook called %d times", len(got))
	}
	if got[0].Origin != OriginStore || got[0].Store <= 0 {
		t.Errorf("first: origin %q store %v, want the store, timed", got[0].Origin, got[0].Store)
	}
	if got[1].Origin != OriginMemory {
		t.Errorf("second: origin %q, want memory", got[1].Origin)
	}
}
