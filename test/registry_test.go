package test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody/endpoint"
	"github.com/nimling/nimbody/mcp"
	"github.com/nimling/nimbody/state"
	"github.com/nimling/nimbody/terraform"
	"github.com/nimling/nimbody/terraform/registry"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
)

const releaseToken = "test-read-token"

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

type releaseFixture struct {
	Assets []releaseAsset `json:"assets"`
}

type stateStore struct {
	mu     sync.Mutex
	states map[string][]byte
	locks  map[string]registry.Lock
}

func (s *stateStore) backend(access echo.MiddlewareFunc) *registry.Backend {
	return &registry.Backend{
		Access: access,
		GetState: func(ctx echo.Context, workspace string) ([]byte, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.states[workspace], nil
		},
		PutState: func(ctx echo.Context, workspace string, body []byte) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.states[workspace] = body
			return nil
		},
		DeleteState: func(ctx echo.Context, workspace string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.states, workspace)
			return nil
		},
		GetLock: func(ctx echo.Context, workspace string) (*registry.Lock, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if lock, ok := s.locks[workspace]; ok {
				return &lock, nil
			}
			return nil, nil
		},
		PostLock: func(ctx echo.Context, workspace string, lock registry.Lock) (registry.Lock, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if held, ok := s.locks[workspace]; ok {
				return held, nil
			}
			s.locks[workspace] = lock
			return lock, nil
		},
		DeleteLock: func(ctx echo.Context, workspace string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.locks, workspace)
			return nil
		},
	}
}

func signingKey(t *testing.T) (*openpgp.Entity, string) {
	entity, err := openpgp.NewEntity("Release", "test", "release@example.com", nil)
	require.NoError(t, err)
	var public bytes.Buffer
	w, err := armor.Encode(&public, openpgp.PublicKeyType, nil)
	require.NoError(t, err)
	require.NoError(t, entity.Serialize(w))
	require.NoError(t, w.Close())
	return entity, public.String()
}

func packRelease(t *testing.T, entity *openpgp.Entity) string {
	dir := t.TempDir()
	build := filepath.Join(dir, "build")
	require.NoError(t, os.MkdirAll(build, 0o755))
	for _, platform := range []string{"linux_amd64", "darwin_arm64"} {
		require.NoError(t, os.WriteFile(filepath.Join(build, "demo_"+platform), []byte("provider "+platform), 0o755))
	}
	module := filepath.Join(dir, "deploy", "terraform", "demo", "azurerm")
	require.NoError(t, os.MkdirAll(filepath.Join(module, ".terraform"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(module, "main.tf"), []byte(`resource "demo_location" "hq" {}`+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(module, "terraform.tfstate"), []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(module, ".terraform", "cache"), []byte("x"), 0o644))

	dist := filepath.Join(dir, "dist")
	root := &cobra.Command{Use: "demo", SilenceUsage: true, SilenceErrors: true}
	terraform.Register(root, terraform.Provider{Name: "demo", Version: func() string { return "v1.2.3" }})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"terraform", "package", "--dir", dist, "--modules", filepath.Join(dir, "deploy", "terraform"),
		"linux_amd64=" + filepath.Join(build, "demo_linux_amd64"), "darwin_arm64=" + filepath.Join(build, "demo_darwin_arm64")})
	require.NoError(t, root.Execute())
	files := registry.GetFiles("demo", "1.2.3", "", "")
	assert.Contains(t, out.String(), files.Sums)
	assert.Contains(t, out.String(), registry.GetModuleArchive("demo", "azurerm", "1.2.3"))

	root.SetArgs([]string{"terraform", "package", "--dir", dist, "linux"})
	assert.Error(t, root.Execute(), "a build is named <os>_<arch>=<binary>")

	sums, err := os.ReadFile(filepath.Join(dist, files.Sums))
	require.NoError(t, err)
	var signature bytes.Buffer
	require.NoError(t, openpgp.DetachSign(&signature, entity, bytes.NewReader(sums), nil))
	require.NoError(t, os.WriteFile(filepath.Join(dist, files.Signature), signature.Bytes(), 0o644))
	return dist
}

func githubFixture(t *testing.T, dist string, calls *int) *httptest.Server {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	authorized := func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer "+releaseToken
	}
	mux.HandleFunc("/repos/nimling/demo/releases/tags/v1.2.3", func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if !authorized(r) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		entries, _ := os.ReadDir(dist)
		var fixture releaseFixture
		for _, entry := range entries {
			info, _ := entry.Info()
			fixture.Assets = append(fixture.Assets, releaseAsset{Name: entry.Name(), URL: server.URL + "/assets/" + entry.Name(), Size: info.Size()})
		}
		_ = json.NewEncoder(w).Encode(fixture)
	})
	mux.HandleFunc("/assets/", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) || r.Header.Get("Accept") != "application/octet-stream" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.ServeFile(w, r, filepath.Join(dist, strings.TrimPrefix(r.URL.Path, "/assets/")))
	})
	return server
}

func basicAccess(next echo.HandlerFunc) echo.HandlerFunc {
	return func(ctx echo.Context) error {
		id, secret, ok := ctx.Request().BasicAuth()
		if !ok {
			return ctx.NoContent(http.StatusUnauthorized)
		}
		if id != "ci" || secret != "secret" {
			return ctx.NoContent(http.StatusForbidden)
		}
		return next(ctx)
	}
}

func fetch(t *testing.T, method, url string, body []byte, auth bool) (int, http.Header, []byte) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	require.NoError(t, err)
	if auth {
		req.SetBasicAuth("ci", "secret")
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, res.Header, out
}

func registryHost(t *testing.T) (*httptest.Server, string, *int) {
	entity, public := signingKey(t)
	dist := packRelease(t, entity)
	calls := 0
	github := githubFixture(t, dist, &calls)
	store := &stateStore{states: map[string][]byte{}, locks: map[string]registry.Lock{}}
	e := echo.New()
	endpoint.Mount(e, endpoint.Set{
		Service: "demo",
		Version: "v1.2.3",
		MCP:     &mcp.Server{Name: "demo", Version: "v1.2.3"},
		State:   &state.Context{},
		Terraform: &registry.Registry{
			Type: "demo",
			Release: registry.Release{
				Repository: "nimling/demo",
				API:        github.URL,
				Token:      func(ctx context.Context) (string, error) { return releaseToken, nil },
			},
			SigningKey: registry.SigningKey{KeyID: entity.PrimaryKey.KeyIdString(), ASCIIArmor: public},
			State:      store.backend(basicAccess),
		},
	})
	host := httptest.NewServer(e)
	t.Cleanup(host.Close)
	return host, public, &calls
}

func TestRegistryDiscovery(t *testing.T) {
	host, _, _ := registryHost(t)

	code, _, body := fetch(t, http.MethodGet, host.URL+registry.DiscoveryPath, nil, false)
	require.Equal(t, http.StatusOK, code)
	var discovery registry.Discovery
	require.NoError(t, json.Unmarshal(body, &discovery))
	assert.Equal(t, registry.Discovery{Providers: "/terraform/providers/v1/", Modules: "/terraform/modules/v1/"}, discovery)

	code, _, body = fetch(t, http.MethodGet, host.URL+endpoint.StatusPath, nil, false)
	require.Equal(t, http.StatusOK, code)
	var status struct {
		Data endpoint.Status `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &status))
	assert.Equal(t, []string{"/api", "/status", "/state", "/mcp", "/.well-known/terraform.json", "/terraform/providers/v1/", "/terraform/modules/v1/", "/terraform/state/"}, status.Data.Endpoints)
	assert.Equal(t, "v1.2.3", status.Data.Version)

	code, _, _ = fetch(t, http.MethodGet, host.URL+endpoint.HealthPath, nil, false)
	assert.Equal(t, http.StatusOK, code)
	code, _, _ = fetch(t, http.MethodGet, host.URL+"/state", nil, false)
	assert.Equal(t, http.StatusOK, code, "the set mounts /state")
	code, _, _ = fetch(t, http.MethodPost, host.URL+mcp.MountPath, nil, false)
	assert.Equal(t, http.StatusUnauthorized, code, "the set mounts /mcp behind its bearer challenge")

	sick := echo.New()
	endpoint.Mount(sick, endpoint.Set{Service: "demo", Version: "v1.2.3", Health: func(ctx context.Context) error { return errors.New("database down") }})
	rec := httptest.NewRecorder()
	sick.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, endpoint.HealthPath, nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestRegistryProvider(t *testing.T) {
	host, public, calls := registryHost(t)
	at := host.URL + registry.ProvidersPath + "samna/demo"

	code, _, body := fetch(t, http.MethodGet, at+"/versions", nil, false)
	require.Equal(t, http.StatusOK, code, string(body))
	var versions registry.ProviderVersions
	require.NoError(t, json.Unmarshal(body, &versions))
	require.Len(t, versions.Versions, 1, "the instance serves exactly the version it runs")
	version := versions.Versions[0]
	assert.Equal(t, "1.2.3", version.Version)
	assert.Equal(t, []string{"6.0"}, version.Protocols)
	assert.Equal(t, []registry.Platform{{OS: "darwin", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}}, version.Platforms)
	require.Len(t, version.SigningKeys.GPGPublicKeys, 1)
	assert.Equal(t, public, version.SigningKeys.GPGPublicKeys[0].ASCIIArmor, "the versions response carries the org public key")

	code, _, body = fetch(t, http.MethodGet, at+"/1.2.3/download/linux/amd64", nil, false)
	require.Equal(t, http.StatusOK, code, string(body))
	var download registry.ProviderDownload
	require.NoError(t, json.Unmarshal(body, &download))
	assert.Equal(t, "terraform-provider-demo_1.2.3_linux_amd64.zip", download.Filename)
	assert.Equal(t, []string{"6.0"}, download.Protocols)
	require.Len(t, download.SigningKeys.GPGPublicKeys, 1)

	code, _, archive := fetch(t, http.MethodGet, download.DownloadURL, nil, false)
	require.Equal(t, http.StatusOK, code)
	sum := sha256.Sum256(archive)
	assert.Equal(t, hex.EncodeToString(sum[:]), download.Shasum, "the zip streamed through the instance matches its shasum")
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)
	require.Len(t, reader.File, 1)
	assert.Equal(t, "terraform-provider-demo_v1.2.3", reader.File[0].Name)

	code, _, sums := fetch(t, http.MethodGet, download.ShasumsURL, nil, false)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, string(sums), download.Shasum+"  "+download.Filename)
	code, _, signature := fetch(t, http.MethodGet, download.ShasumsSignatureURL, nil, false)
	require.Equal(t, http.StatusOK, code)
	keyring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(download.SigningKeys.GPGPublicKeys[0].ASCIIArmor))
	require.NoError(t, err)
	_, err = openpgp.CheckDetachedSignature(keyring, bytes.NewReader(sums), bytes.NewReader(signature))
	assert.NoError(t, err, "SHA256SUMS verifies against the public key the registry publishes")
	_, err = openpgp.CheckDetachedSignature(keyring, bytes.NewReader(append(sums, '\n')), bytes.NewReader(signature))
	assert.Error(t, err, "a changed SHA256SUMS does not verify")

	code, _, _ = fetch(t, http.MethodGet, at+"/1.2.3/download/windows/amd64", nil, false)
	assert.Equal(t, http.StatusNotFound, code, "a platform the release lacks")
	code, _, _ = fetch(t, http.MethodGet, at+"/1.2.2/download/linux/amd64", nil, false)
	assert.Equal(t, http.StatusNotFound, code, "a version the instance does not run")
	code, _, _ = fetch(t, http.MethodGet, at+"/1.2.3/files/other.zip", nil, false)
	assert.Equal(t, http.StatusNotFound, code, "only the provider's own files stream")
	code, _, _ = fetch(t, http.MethodGet, host.URL+registry.ProvidersPath+"samna/other/versions", nil, false)
	assert.Equal(t, http.StatusNotFound, code, "only the instance's own provider")
	assert.Equal(t, 1, *calls, "the release is read once and kept")
}

func TestRegistryUnreachable(t *testing.T) {
	e := echo.New()
	registry.Mount(e, registry.Registry{Type: "demo", Version: "v1.2.3", Release: registry.Release{
		Repository: "nimling/demo",
		Token:      func(ctx context.Context) (string, error) { return "", errors.New("no token") },
	}})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, registry.ProvidersPath+"samna/demo/versions", nil))
	assert.Equal(t, http.StatusBadGateway, rec.Code, "a release that cannot be read says so and is never an empty list")
	assert.NotContains(t, rec.Body.String(), "no token", "the cause stays in the log")
}

func TestRegistryModule(t *testing.T) {
	host, _, _ := registryHost(t)
	at := host.URL + registry.ModulesPath + "samna/demo/azurerm"

	code, _, body := fetch(t, http.MethodGet, at+"/versions", nil, false)
	require.Equal(t, http.StatusOK, code)
	var versions registry.ModuleVersions
	require.NoError(t, json.Unmarshal(body, &versions))
	assert.Equal(t, registry.ModuleVersions{Modules: []registry.ModuleVersionList{{Versions: []registry.ModuleVersion{{Version: "1.2.3"}}}}}, versions)

	code, header, _ := fetch(t, http.MethodGet, at+"/1.2.3/download", nil, false)
	require.Equal(t, http.StatusNoContent, code)
	location := header.Get("X-Terraform-Get")
	assert.True(t, strings.HasSuffix(location, "/terraform-module-demo-azurerm_1.2.3.tar.gz"), location)

	code, _, archive := fetch(t, http.MethodGet, location, nil, false)
	require.Equal(t, http.StatusOK, code)
	unzipped, err := gzip.NewReader(bytes.NewReader(archive))
	require.NoError(t, err)
	var names []string
	files := tar.NewReader(unzipped)
	for {
		entry, err := files.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		names = append(names, entry.Name)
	}
	assert.Equal(t, []string{"main.tf"}, names, "the module ships without local state or the .terraform directory")

	code, _, body = fetch(t, http.MethodGet, host.URL+registry.ModulesPath+"samna/demo/aws/versions", nil, false)
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, json.Unmarshal(body, &versions))
	assert.Empty(t, versions.Modules[0].Versions, "a module the release lacks has no versions")
	code, _, _ = fetch(t, http.MethodGet, host.URL+registry.ModulesPath+"samna/demo/aws/1.2.3/download", nil, false)
	assert.Equal(t, http.StatusNotFound, code)
}

func TestRegistryState(t *testing.T) {
	host, _, _ := registryHost(t)
	at := host.URL + registry.StatePath + "prd"
	lockA, _ := json.Marshal(registry.Lock{ID: "a", Operation: "OperationTypeApply", Who: "ci"})
	lockB, _ := json.Marshal(registry.Lock{ID: "b", Operation: "OperationTypePlan", Who: "laptop"})
	tfstate := []byte(`{"version":4,"serial":1}`)

	code, _, _ := fetch(t, http.MethodGet, at, nil, false)
	assert.Equal(t, http.StatusUnauthorized, code, "the backend is behind the host's access")
	code, _, _ = fetch(t, registry.MethodLock, at, lockA, false)
	assert.Equal(t, http.StatusUnauthorized, code)
	req, _ := http.NewRequest(http.MethodGet, at, nil)
	req.SetBasicAuth("ci", "wrong")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	res.Body.Close()
	assert.Equal(t, http.StatusForbidden, res.StatusCode)

	code, _, _ = fetch(t, http.MethodGet, at, nil, true)
	assert.Equal(t, http.StatusNotFound, code, "no state yet")

	code, _, _ = fetch(t, registry.MethodLock, at, lockA, true)
	assert.Equal(t, http.StatusOK, code)
	code, _, body := fetch(t, registry.MethodLock, at, lockB, true)
	assert.Equal(t, http.StatusLocked, code, "a second lock conflicts")
	var held registry.Lock
	require.NoError(t, json.Unmarshal(body, &held))
	assert.Equal(t, "a", held.ID, "the conflict answers the lock that is held")

	code, _, _ = fetch(t, http.MethodPost, at+"?ID=b", tfstate, true)
	assert.Equal(t, http.StatusLocked, code, "a write under another lock is refused")
	code, _, _ = fetch(t, http.MethodPost, at, tfstate, true)
	assert.Equal(t, http.StatusLocked, code, "a write without the lock is refused while it is held")
	code, _, _ = fetch(t, http.MethodPost, at+"?ID=a", tfstate, true)
	assert.Equal(t, http.StatusOK, code)
	code, _, body = fetch(t, http.MethodGet, at, nil, true)
	assert.Equal(t, http.StatusOK, code)
	assert.JSONEq(t, string(tfstate), string(body))

	code, _, _ = fetch(t, registry.MethodUnlock, at, lockB, true)
	assert.Equal(t, http.StatusLocked, code, "another holder cannot unlock")
	code, _, _ = fetch(t, registry.MethodUnlock, at, lockA, true)
	assert.Equal(t, http.StatusOK, code)
	code, _, _ = fetch(t, registry.MethodLock, at, lockB, true)
	assert.Equal(t, http.StatusOK, code, "a released lock is free")
	code, _, _ = fetch(t, registry.MethodUnlock, at, nil, true)
	assert.Equal(t, http.StatusOK, code, "an unlock with no body forces it")
	code, _, _ = fetch(t, http.MethodPost, at, []byte("not json"), true)
	assert.Equal(t, http.StatusBadRequest, code)
	code, _, _ = fetch(t, http.MethodDelete, at, nil, true)
	assert.Equal(t, http.StatusOK, code)
	code, _, _ = fetch(t, http.MethodGet, at, nil, true)
	assert.Equal(t, http.StatusNotFound, code)
	code, _, _ = fetch(t, http.MethodGet, host.URL+registry.StatePath+"..", nil, true)
	assert.NotEqual(t, http.StatusOK, code)
}
