package test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nimling/nimbody/cli"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type memoryKeyring struct {
	mu    sync.Mutex
	items map[string]string
}

func (m *memoryKeyring) Get(service, account string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.items[service+"/"+account]
	if !ok {
		return "", os.ErrNotExist
	}
	return value, nil
}

func (m *memoryKeyring) Put(service, account, secret string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[service+"/"+account] = secret
	return nil
}

func (m *memoryKeyring) Delete(service, account string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, service+"/"+account)
	return nil
}

func credentialSettings(t *testing.T, keyring cli.Keyring) *cli.Settings {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	previous := cli.Keychain
	cli.Keychain = keyring
	t.Cleanup(func() { cli.Keychain = previous })
	return &cli.Settings{Name: "demo", Prefix: "DEMO_", Keys: []cli.Setting{
		{Key: "endpoint", Env: "DEMO_ENDPOINT"},
		{Key: "app_slug", Env: "DEMO_APP_SLUG"},
		{Key: "client_id", Env: "DEMO_CLIENT_ID"},
		{Key: "client_secret", Env: "DEMO_CLIENT_SECRET", Secret: true},
	}}
}

func TestCredentialFileStoreWhenNoKeychain(t *testing.T) {
	settings := credentialSettings(t, nil)
	path, err := settings.PutCredential(cli.Credential{Endpoint: "https://demo.example.test", RefreshToken: "r1", AccessToken: "a1", AccessExpiresAt: time.Now().Add(time.Hour), ObtainedAt: time.Now()})
	require.NoError(t, err)
	assert.Equal(t, "demo.example.test.cred", filepath.Base(path))
	info, _ := os.Stat(path)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	body, _ := os.ReadFile(path)
	assert.Contains(t, string(body), `DEMO_REFRESH_TOKEN="r1"`, "the file keeps the names every cli wrote, so a stored login carries over")

	cred, err := settings.GetCredential("https://demo.example.test", "")
	require.NoError(t, err)
	assert.Equal(t, "r1", cred.RefreshToken)
	assert.Equal(t, cli.StoreFile, cred.Store)
	all, _ := settings.GetCredentials()
	assert.Len(t, all, 1)
	require.NoError(t, settings.DeleteCredential(cred))
	_, err = settings.GetCredential("https://demo.example.test", "")
	assert.ErrorIs(t, err, cli.ErrNoCredential)
}

func TestCredentialKeychainKeepsTheTokens(t *testing.T) {
	keyring := &memoryKeyring{items: map[string]string{}}
	settings := credentialSettings(t, keyring)
	path, err := settings.PutCredential(cli.Credential{Endpoint: "https://demo.example.test", RefreshToken: "r1", AccessToken: "a1"})
	require.NoError(t, err)
	body, _ := os.ReadFile(path)
	assert.NotContains(t, string(body), "r1")
	assert.Contains(t, string(body), `DEMO_STORE="keychain"`)
	assert.Contains(t, keyring.items, "demo/demo.example.test.cred")

	cred, err := settings.GetCredential("https://demo.example.test", "")
	require.NoError(t, err)
	assert.Equal(t, "r1", cred.RefreshToken)
	assert.Equal(t, "a1", cred.AccessToken)
	require.NoError(t, settings.DeleteCredential(cred))
	assert.Empty(t, keyring.items)
}

func TestCredentialLockIsCrossProcess(t *testing.T) {
	settings := credentialSettings(t, nil)
	unlock, err := settings.LockCredential("https://demo.example.test", "")
	require.NoError(t, err)
	var second atomic.Bool
	done := make(chan struct{})
	go func() {
		release, err := settings.LockCredential("https://demo.example.test", "")
		assert.NoError(t, err)
		second.Store(true)
		release()
		close(done)
	}()
	time.Sleep(250 * time.Millisecond)
	assert.False(t, second.Load(), "the lock holds while the first run refreshes")
	unlock()
	<-done
	assert.True(t, second.Load())
}

type authService struct {
	server    *httptest.Server
	exchanges atomic.Int32
	refreshes atomic.Int32
	revoked   atomic.Int32
	mcp       bool
	seen      atomic.Value
}

func newAuthService(t *testing.T, mcp bool) *authService {
	service := &authService{mcp: mcp}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("/api/auth/cli/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "abc" || r.Form.Get("code_verifier") == "" {
				w.WriteHeader(http.StatusBadRequest)
				write(w, map[string]string{"error": "invalid_grant"})
				return
			}
			write(w, map[string]any{"access_token": "access-1", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "refresh-1"})
		case "refresh_token":
			service.refreshes.Add(1)
			write(w, map[string]any{"access_token": "access-2", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "refresh-2"})
		}
	})
	mux.HandleFunc("/api/auth/cli/revoke", func(w http.ResponseWriter, r *http.Request) {
		service.revoked.Add(1)
	})
	mux.HandleFunc("/api/auth/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		if !ok || id != "machine" || secret != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		service.exchanges.Add(1)
		time.Sleep(50 * time.Millisecond)
		write(w, map[string]any{"access_token": "exchanged", "token_type": "Bearer", "expires_in": 3600})
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		endpoints := []string{"/api", "/status"}
		if service.mcp {
			endpoints = append(endpoints, "/mcp")
		}
		write(w, map[string]any{"data": map[string]any{"service": "demo", "version": "v9.9.9", "endpoints": endpoints}, "request_id": "r"})
	})
	mux.HandleFunc("/status/health", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"data": map[string]any{"healthy": true}, "request_id": "r"})
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		service.seen.Store(r.Header.Get("Authorization") + " " + r.Header.Get("User-Agent"))
		auth := r.Header.Get("Authorization")
		if auth == "" || auth == "Bearer stale" {
			w.WriteHeader(http.StatusUnauthorized)
			write(w, map[string]any{"error": map[string]string{"message": "no token"}, "request_id": "r"})
			return
		}
		if r.URL.Path == "/api/me" {
			write(w, map[string]any{"data": map[string]string{"user_id": "u-1", "email": "ada@example.test"}, "request_id": "r"})
			return
		}
		write(w, map[string]any{"data": []map[string]string{{"id": "1"}}, "request_id": "r"})
	})
	service.server = httptest.NewServer(mux)
	t.Cleanup(service.server.Close)
	t.Setenv("DEMO_ENDPOINT", service.server.URL)
	return service
}

func clientTree(settings *cli.Settings, client *cli.Client) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	root := &cobra.Command{Use: "demo", SilenceErrors: true, SilenceUsage: true}
	settings.Flags(root)
	root.PersistentFlags().Bool("json", false, "json")
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error { return settings.Load(cmd) }
	previous := cli.Bridge
	if err := client.Mount(root); err != nil {
		panic(err)
	}
	cli.Bridge = previous
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	return root, &stdout, &stderr
}

func TestLoginPastedCodeStoresARefreshableLogin(t *testing.T) {
	service := newAuthService(t, false)
	settings := credentialSettings(t, nil)
	client := &cli.Client{LoginPath: "/api/auth/login", TokenPath: "/api/auth/cli/token", RevokePath: "/api/auth/cli/revoke", IdentityPath: "/api/me", AppHeader: "X-App-Slug", Settings: settings, Version: func() string { return "v1.2.3" }}
	root, stdout, stderr := clientTree(settings, client)
	root.SetIn(strings.NewReader("abc\n"))
	root.SetArgs([]string{"login", "--no-browser"})
	require.NoError(t, root.Execute(), stderr.String())
	assert.Contains(t, stderr.String(), "/api/auth/login?cli_challenge=")
	assert.Contains(t, stderr.String(), "signed in as ada@example.test")
	assert.Empty(t, stdout.String(), "login prints nothing to stdout")

	cred, err := settings.GetCredential(service.server.URL, "")
	require.NoError(t, err)
	assert.Equal(t, "refresh-1", cred.RefreshToken)
	assert.Equal(t, "u-1", cred.Subject)

	cred.AccessExpiresAt = time.Now().Add(-time.Minute)
	_, err = settings.PutCredential(cred)
	require.NoError(t, err)
	fresh := &cli.Client{LoginPath: "/api/auth/login", TokenPath: "/api/auth/cli/token", RevokePath: "/api/auth/cli/revoke", IdentityPath: "/api/me", AppHeader: "X-App-Slug", Settings: settings, Version: func() string { return "v1.2.3" }}
	_, err = fresh.Send(nil, http.MethodGet, "/api/things", nil, "", nil)
	require.NoError(t, err)
	assert.Equal(t, int32(1), service.refreshes.Load())
	assert.Equal(t, "Bearer access-2 demo/v1.2.3", service.seen.Load())
	cred, _ = settings.GetCredential(service.server.URL, "")
	assert.Equal(t, "refresh-2", cred.RefreshToken, "the rotated refresh token is stored")

	root, stdout, _ = clientTree(settings, &cli.Client{LoginPath: "/api/auth/login", TokenPath: "/api/auth/cli/token", RevokePath: "/api/auth/cli/revoke", IdentityPath: "/api/me", AppHeader: "X-App-Slug", Settings: settings})
	root.SetArgs([]string{"status", "--json"})
	require.NoError(t, root.Execute())
	var status cli.Status
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &status))
	assert.Equal(t, cli.AuthLogin, status.Auth)
	assert.Equal(t, "u-1", status.Subject)
	assert.JSONEq(t, `{"healthy":true}`, string(status.Health))

	root, _, stderr = clientTree(settings, &cli.Client{LoginPath: "/api/auth/login", TokenPath: "/api/auth/cli/token", RevokePath: "/api/auth/cli/revoke", IdentityPath: "/api/me", AppHeader: "X-App-Slug", Settings: settings})
	root.SetArgs([]string{"logout"})
	require.NoError(t, root.Execute())
	assert.Contains(t, stderr.String(), "ended the session at")
	assert.Equal(t, int32(1), service.revoked.Load())
	_, err = settings.GetCredential(service.server.URL, "")
	assert.ErrorIs(t, err, cli.ErrNoCredential)
}

func TestLoginLoopbackTakesTheCodeBack(t *testing.T) {
	service := newAuthService(t, false)
	settings := credentialSettings(t, nil)
	previous := cli.OpenBrowser
	t.Cleanup(func() { cli.OpenBrowser = previous })
	cli.OpenBrowser = func(target string) error {
		login, _ := url.Parse(target)
		redirect := login.Query().Get("cli_redirect") + "?code=abc&state=" + url.QueryEscape(login.Query().Get("cli_state"))
		go func() {
			res, err := http.Get(redirect)
			if err == nil {
				res.Body.Close()
			}
		}()
		return nil
	}
	root, _, stderr := clientTree(settings, &cli.Client{LoginPath: "/api/auth/login", TokenPath: "/api/auth/cli/token", RevokePath: "/api/auth/cli/revoke", IdentityPath: "/api/me", AppHeader: "X-App-Slug", Settings: settings})
	root.SetArgs([]string{"login"})
	require.NoError(t, root.Execute(), stderr.String())
	_, err := settings.GetCredential(service.server.URL, "")
	assert.NoError(t, err)
}

func TestClientTokenFromTheEnvironmentForCI(t *testing.T) {
	service := newAuthService(t, false)
	settings := credentialSettings(t, nil)
	t.Setenv("DEMO_ACCESS_TOKEN", "from-ci")
	client := &cli.Client{LoginPath: "/api/auth/login", TokenPath: "/api/auth/cli/token", RevokePath: "/api/auth/cli/revoke", IdentityPath: "/api/me", AppHeader: "X-App-Slug", Settings: settings}
	assert.Equal(t, cli.AuthToken, client.Mode())
	_, err := client.Send(nil, http.MethodGet, "/api/things", nil, "", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(service.seen.Load().(string), "Bearer from-ci "))
	t.Setenv("DEMO_TOKEN", "preferred")
	assert.Equal(t, "preferred", (&cli.Client{LoginPath: "/api/auth/login", TokenPath: "/api/auth/cli/token", RevokePath: "/api/auth/cli/revoke", IdentityPath: "/api/me", AppHeader: "X-App-Slug", Settings: settings}).EnvToken())
}

func TestClientMachinePairIsBasicToTheService(t *testing.T) {
	service := newAuthService(t, false)
	settings := credentialSettings(t, nil)
	t.Setenv("DEMO_CLIENT_ID", "machine")
	t.Setenv("DEMO_CLIENT_SECRET", "s3cret")
	client := &cli.Client{LoginPath: "/api/auth/login", TokenPath: "/api/auth/cli/token", RevokePath: "/api/auth/cli/revoke", IdentityPath: "/api/me", AppHeader: "X-App-Slug", Settings: settings}
	assert.Equal(t, cli.AuthClient, client.Mode())
	_, err := client.Send(nil, http.MethodGet, "/api/things", nil, "", nil)
	require.NoError(t, err)
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("machine:s3cret"))
	assert.True(t, strings.HasPrefix(service.seen.Load().(string), basic))
	assert.Equal(t, int32(0), service.exchanges.Load(), "a disciple's cli never fetches a token")
	_, err = client.GetToken(context.Background())
	assert.Equal(t, cli.ExitRejected, cli.ExitCode(err))
	endpoint, token, err := client.Remote(context.Background())
	assert.NoError(t, err)
	assert.Empty(t, endpoint+token, "a Basic pair serves the tools locally")
}

func TestClientProphetExchangeIsSharedAcrossRuns(t *testing.T) {
	service := newAuthService(t, true)
	settings := credentialSettings(t, nil)
	t.Setenv("DEMO_CLIENT_ID", "machine")
	t.Setenv("DEMO_CLIENT_SECRET", "s3cret")
	var group sync.WaitGroup
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			client := &cli.Client{LoginPath: "/api/auth/login", TokenPath: "/api/auth/cli/token", RevokePath: "/api/auth/cli/revoke", IdentityPath: "/api/me", AppHeader: "X-App-Slug", Settings: settings, Exchange: "/api/auth/oauth/token"}
			_, err := client.Send(nil, http.MethodGet, "/api/things", nil, "", nil)
			assert.NoError(t, err)
		}()
	}
	group.Wait()
	assert.Equal(t, int32(1), service.exchanges.Load(), "parallel runs share one token under the lock")
	cached, err := settings.GetCredential(service.server.URL, "machine")
	require.NoError(t, err)
	assert.Equal(t, "exchanged", cached.AccessToken)

	cached.AccessToken = "stale"
	_, err = settings.PutCredential(cached)
	require.NoError(t, err)
	_, err = (&cli.Client{LoginPath: "/api/auth/login", TokenPath: "/api/auth/cli/token", RevokePath: "/api/auth/cli/revoke", IdentityPath: "/api/me", AppHeader: "X-App-Slug", Settings: settings, Exchange: "/api/auth/oauth/token"}).Send(nil, http.MethodGet, "/api/things", nil, "", nil)
	require.NoError(t, err, "a cached token the service refuses is exchanged again once")
	assert.Equal(t, int32(2), service.exchanges.Load())

	endpoint, token, err := (&cli.Client{LoginPath: "/api/auth/login", TokenPath: "/api/auth/cli/token", RevokePath: "/api/auth/cli/revoke", IdentityPath: "/api/me", AppHeader: "X-App-Slug", Settings: settings, Exchange: "/api/auth/oauth/token"}).Remote(context.Background())
	require.NoError(t, err)
	assert.Equal(t, service.server.URL+"/mcp", endpoint)
	assert.Equal(t, "exchanged", token)
}

func TestClientRemoteBridgesOnlyWhenTheServiceServesMCP(t *testing.T) {
	newAuthService(t, false)
	settings := credentialSettings(t, nil)
	t.Setenv("DEMO_TOKEN", "from-ci")
	endpoint, _, err := (&cli.Client{LoginPath: "/api/auth/login", TokenPath: "/api/auth/cli/token", RevokePath: "/api/auth/cli/revoke", IdentityPath: "/api/me", AppHeader: "X-App-Slug", Settings: settings}).Remote(context.Background())
	assert.NoError(t, err)
	assert.Empty(t, endpoint, "no /mcp on the service, the tools stay local")

	service := newAuthService(t, true)
	endpoint, token, err := (&cli.Client{LoginPath: "/api/auth/login", TokenPath: "/api/auth/cli/token", RevokePath: "/api/auth/cli/revoke", IdentityPath: "/api/me", AppHeader: "X-App-Slug", Settings: settings}).Remote(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, service.server.URL+"/mcp", endpoint)
	assert.Equal(t, "from-ci", token)
}

func TestClientWithoutCredentialIsNotAuthenticated(t *testing.T) {
	newAuthService(t, false)
	settings := credentialSettings(t, nil)
	_, err := (&cli.Client{LoginPath: "/api/auth/login", TokenPath: "/api/auth/cli/token", RevokePath: "/api/auth/cli/revoke", IdentityPath: "/api/me", AppHeader: "X-App-Slug", Settings: settings}).Send(nil, http.MethodGet, "/api/things", nil, "", nil)
	assert.Equal(t, cli.ExitUnauthenticated, cli.ExitCode(err))
	assert.Contains(t, err.(*cli.Error).Hint, "demo login")
}

func TestClientMountNamesTheMissingPaths(t *testing.T) {
	err := (&cli.Client{Settings: &cli.Settings{Name: "demo"}, LoginPath: "/login"}).Mount(&cobra.Command{Use: "demo"})
	assert.EqualError(t, err, "cli.Client needs TokenPath, RevokePath, IdentityPath, AppHeader: the cli names its own login, token, revoke and identity paths and its app header")
	err = (&cli.Client{Settings: &cli.Settings{Name: "demo"}, LoginPath: "login", TokenPath: "/t", RevokePath: "/r", IdentityPath: "/me", AppHeader: "X-App"}).Mount(&cobra.Command{Use: "demo"})
	assert.EqualError(t, err, "cli.Client paths start with a slash, got login")
}
