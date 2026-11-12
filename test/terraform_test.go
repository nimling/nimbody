package test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/nimling/nimbody/cli"
	"github.com/nimling/nimbody/state"
	"github.com/nimling/nimbody/terraform"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func demoProvider(m *memory) terraform.Provider {
	settings := &cli.Settings{Name: "demo", Prefix: "DEMO_"}
	client := &cli.Client{Settings: settings, LoginPath: "/api/auth/login", TokenPath: "/api/auth/cli/token", RevokePath: "/api/auth/cli/revoke", IdentityPath: "/api/me", AppHeader: "X-App-Slug"}
	return terraform.Provider{Name: "demo", Version: func() string { return "v1.2.3" }, Client: client, Document: state.GetDocument("demo", "v1.2.3", stateGroups(m, connectionGroup(m))...)}
}

func providerHost(t *testing.T) (*memory, tfprotov6.ProviderServer, string) {
	t.Setenv("HOME", t.TempDir())
	m := newMemory("a")
	e, _ := stateService(m, connectionGroup(m))
	host := httptest.NewServer(e)
	t.Cleanup(host.Close)
	return m, demoProvider(m).Server(), host.URL
}

func object(typ tftypes.Type, values map[string]tftypes.Value) tftypes.Value {
	attributes := typ.(tftypes.Object).AttributeTypes
	full := map[string]tftypes.Value{}
	for name, attribute := range attributes {
		if value, ok := values[name]; ok {
			full[name] = value
			continue
		}
		full[name] = tftypes.NewValue(attribute, nil)
	}
	return tftypes.NewValue(typ, full)
}

func text(value string) tftypes.Value { return tftypes.NewValue(tftypes.String, value) }

func number(value int64) tftypes.Value {
	return tftypes.NewValue(tftypes.Number, new(big.Float).SetInt64(value))
}

func encoded(t *testing.T, typ tftypes.Type, value tftypes.Value) *tfprotov6.DynamicValue {
	out, err := tfprotov6.NewDynamicValue(typ, value)
	require.NoError(t, err)
	return &out
}

func decodedValue(t *testing.T, typ tftypes.Type, value *tfprotov6.DynamicValue) tftypes.Value {
	out, err := value.Unmarshal(typ)
	require.NoError(t, err)
	return out
}

func noErrors(t *testing.T, diagnostics []*tfprotov6.Diagnostic) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		assert.NotEqual(t, tfprotov6.DiagnosticSeverityError, diagnostic.Severity, diagnostic.Summary+": "+diagnostic.Detail)
	}
}

func attribute(attributes []*tfprotov6.SchemaAttribute, name string) *tfprotov6.SchemaAttribute {
	for _, item := range attributes {
		if item.Name == name {
			return item
		}
	}
	return nil
}

type terraformRun struct {
	t      *testing.T
	ctx    context.Context
	server tfprotov6.ProviderServer
	types  map[string]tftypes.Type
}

func (r terraformRun) plan(name string, prior, config tftypes.Value) *tfprotov6.PlanResourceChangeResponse {
	typ := r.types[name]
	resp, err := r.server.PlanResourceChange(r.ctx, &tfprotov6.PlanResourceChangeRequest{TypeName: name, PriorState: encoded(r.t, typ, prior), ProposedNewState: encoded(r.t, typ, config), Config: encoded(r.t, typ, config)})
	require.NoError(r.t, err)
	noErrors(r.t, resp.Diagnostics)
	return resp
}

func (r terraformRun) apply(name string, prior, config tftypes.Value) tftypes.Value {
	typ := r.types[name]
	planned := r.plan(name, prior, config)
	resp, err := r.server.ApplyResourceChange(r.ctx, &tfprotov6.ApplyResourceChangeRequest{TypeName: name, PriorState: encoded(r.t, typ, prior), PlannedState: planned.PlannedState, Config: encoded(r.t, typ, config)})
	require.NoError(r.t, err)
	noErrors(r.t, resp.Diagnostics)
	return decodedValue(r.t, typ, resp.NewState)
}

func (r terraformRun) read(name string, current tftypes.Value) tftypes.Value {
	typ := r.types[name]
	resp, err := r.server.ReadResource(r.ctx, &tfprotov6.ReadResourceRequest{TypeName: name, CurrentState: encoded(r.t, typ, current)})
	require.NoError(r.t, err)
	noErrors(r.t, resp.Diagnostics)
	return decodedValue(r.t, typ, resp.NewState)
}

func (r terraformRun) destroy(name string, prior tftypes.Value) {
	typ := r.types[name]
	resp, err := r.server.ApplyResourceChange(r.ctx, &tfprotov6.ApplyResourceChangeRequest{TypeName: name, PriorState: encoded(r.t, typ, prior), PlannedState: encoded(r.t, typ, tftypes.NewValue(typ, nil)), Config: encoded(r.t, typ, tftypes.NewValue(typ, nil))})
	require.NoError(r.t, err)
	noErrors(r.t, resp.Diagnostics)
	assert.True(r.t, decodedValue(r.t, typ, resp.NewState).IsNull())
}

func startProvider(t *testing.T) (*memory, terraformRun, *tfprotov6.GetProviderSchemaResponse) {
	m, server, endpoint := providerHost(t)
	ctx := context.Background()
	schema, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	require.NoError(t, err)
	noErrors(t, schema.Diagnostics)
	providerType := schema.Provider.ValueType()
	configured, err := server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: encoded(t, providerType, object(providerType, map[string]tftypes.Value{"endpoint": text(endpoint), "token": text("owner")}))})
	require.NoError(t, err)
	noErrors(t, configured.Diagnostics)
	run := terraformRun{t: t, ctx: ctx, server: server, types: map[string]tftypes.Type{}}
	for name, resource := range schema.ResourceSchemas {
		run.types[name] = resource.ValueType()
	}
	return m, run, schema
}

func TestTerraformSchemaFromState(t *testing.T) {
	_, _, schema := startProvider(t)
	names := []string{}
	for name := range schema.ResourceSchemas {
		names = append(names, name)
	}
	sort.Strings(names)
	assert.Equal(t, []string{"demo_connection", "demo_location", "demo_room"}, names, "one resource per group, named <provider>_<group>")

	room := schema.ResourceSchemas["demo_room"].Block.Attributes
	assert.True(t, attribute(room, "key").Required, "the record key is the identity")
	code := attribute(room, "code")
	assert.True(t, code.WriteOnly && code.Sensitive, "a secret is write only and sensitive")
	region := attribute(schema.ResourceSchemas["demo_location"].Block.Attributes, "region")
	assert.True(t, region.Optional && region.Computed, "a server default is optional and computed")

	connection := schema.ResourceSchemas["demo_connection"].Block.Attributes
	config := attribute(connection, "config")
	require.NotNil(t, config.NestedType)
	for _, name := range []string{"client_secret", "refresh_token", "password"} {
		secret := attribute(config.NestedType.Attributes, name)
		require.NotNil(t, secret, name)
		assert.True(t, secret.WriteOnly && secret.Sensitive, "nested per pointer and per discriminator branch: "+name)
	}
	assert.False(t, attribute(config.NestedType.Attributes, "client_id").WriteOnly)
	assert.True(t, attribute(config.NestedType.Attributes, "region").Computed)
	token := attribute(attribute(connection, "auth").NestedType.Attributes, "token")
	assert.True(t, token.WriteOnly, "a secret in a discriminated branch")
}

func TestTerraformProviderRoundTrip(t *testing.T) {
	m, run, _ := startProvider(t)
	locationType, roomType := run.types["demo_location"], run.types["demo_room"]
	null := func(typ tftypes.Type) tftypes.Value { return tftypes.NewValue(typ, nil) }

	hqConfig := object(locationType, map[string]tftypes.Value{"key": text("hq"), "name": text("Head office")})
	planned := run.plan("demo_location", null(locationType), hqConfig)
	assert.False(t, decodedValue(t, locationType, planned.PlannedState).IsFullyKnown(), "a server default is unknown until applied")
	hq := run.apply("demo_location", null(locationType), hqConfig)
	assert.True(t, hq.Equal(object(locationType, map[string]tftypes.Value{"key": text("hq"), "name": text("Head office"), "region": text("north")})), hq.String())

	roomConfig := object(roomType, map[string]tftypes.Value{"key": text("north"), "name": text("North"), "location_id": text("hq"), "seats": number(8), "kind": text("meeting"), "code": text("1234")})
	plan := run.plan("demo_room", null(roomType), roomConfig)
	assert.Empty(t, plan.Diagnostics, "the dry run accepts the room")
	assert.True(t, decodedValue(t, roomType, plan.PlannedState).Equal(object(roomType, map[string]tftypes.Value{"key": text("north"), "name": text("North"), "location_id": text("hq"), "seats": number(8), "kind": text("meeting")})), "a write only value never enters the plan")
	room := run.apply("demo_room", null(roomType), roomConfig)
	assert.Equal(t, 1, m.count("room"))
	assert.Equal(t, "1234", m.rows["room"][0].data["code"], "the secret reaches the service from the config")
	assert.Equal(t, "a-1", m.rows["room"][0].data["location_id"], "the reference by key resolves to the location's id")
	assert.True(t, at(room, "code").IsNull(), "a write only value never enters the state")

	assert.True(t, run.read("demo_room", room).Equal(room), "a refresh finds no drift")
	assert.True(t, run.read("demo_location", hq).Equal(hq), "a refresh finds no drift")

	moved := object(roomType, map[string]tftypes.Value{"key": text("north"), "name": text("North"), "location_id": text("hq"), "seats": number(10), "kind": text("meeting"), "code": text("1234")})
	update := run.plan("demo_room", room, moved)
	assert.Empty(t, update.RequiresReplace)
	room = run.apply("demo_room", room, moved)
	assert.EqualValues(t, 10, m.rows["room"][0].data["seats"])
	assert.Equal(t, "a-2", m.rows["room"][0].id, "an update writes in place")

	retyped := object(roomType, map[string]tftypes.Value{"key": text("north"), "name": text("North"), "location_id": text("hq"), "seats": number(10), "kind": text("desk"), "code": text("1234")})
	replace := run.plan("demo_room", room, retyped)
	require.Len(t, replace.RequiresReplace, 1, "an immutable change forces replace")
	assert.True(t, replace.RequiresReplace[0].Equal(tftypes.NewAttributePath().WithAttributeName("kind")))

	resp, err := run.server.ImportResourceState(run.ctx, &tfprotov6.ImportResourceStateRequest{TypeName: "demo_location", ID: "hq"})
	require.NoError(t, err)
	noErrors(t, resp.Diagnostics)
	imported := decodedValue(t, locationType, resp.ImportedResources[0].State)
	assert.True(t, run.read("demo_location", imported).Equal(hq), "an import by key reads the record")

	run.destroy("demo_room", room)
	assert.Equal(t, 0, m.count("room"))
	run.destroy("demo_room", room)
	assert.True(t, run.read("demo_room", room).IsNull(), "a deleted record leaves the state")
}

func TestTerraformNestedSecretsByBranch(t *testing.T) {
	m, run, _ := startProvider(t)
	typ := run.types["demo_connection"]
	attributes := typ.(tftypes.Object).AttributeTypes
	config := object(typ, map[string]tftypes.Value{
		"key":  text("calendar"),
		"name": text("Calendar"),
		"type": text("oauth"),
		"config": object(attributes["config"], map[string]tftypes.Value{
			"client_id": text("public-id"), "client_secret": text("s3cret"), "refresh_token": text("r3fresh"), "mode": text("push"),
		}),
		"auth": object(attributes["auth"], map[string]tftypes.Value{"kind": text("token"), "token": text("t0ken")}),
	})
	applied := run.apply("demo_connection", tftypes.NewValue(typ, nil), config)
	stored := m.rows["connection"][0].data
	assert.Equal(t, map[string]any{"client_id": "public-id", "client_secret": "s3cret", "refresh_token": "r3fresh", "mode": "push"}, stored["config"], "every nested secret reaches the service")
	assert.Equal(t, "t0ken", stored["auth"].(map[string]any)["token"])
	assert.True(t, at(at(applied, "config"), "client_secret").IsNull())
	assert.True(t, at(at(applied, "config"), "refresh_token").IsNull())
	assert.Equal(t, "public-id", stringOf(at(at(applied, "config"), "client_id")))
	assert.True(t, run.read("demo_connection", applied).Equal(applied), "a refresh finds no drift in nested values")
}

func at(value tftypes.Value, name string) tftypes.Value {
	var fields map[string]tftypes.Value
	if err := value.As(&fields); err != nil {
		return tftypes.Value{}
	}
	return fields[name]
}

func stringOf(value tftypes.Value) string {
	var out string
	_ = value.As(&out)
	return out
}

func TestTerraformInstall(t *testing.T) {
	dir := t.TempDir()
	p := terraform.Provider{Name: "demo", Version: func() string { return "v1.2.3" }}
	path, err := p.Install(dir)
	require.NoError(t, err)
	file := "terraform-provider-demo_v1.2.3"
	if runtime.GOOS == "windows" {
		file += ".exe"
	}
	assert.Equal(t, filepath.Join(dir, "registry.terraform.io", "nimling", "demo", "1.2.3", runtime.GOOS+"_"+runtime.GOARCH, file), path, "the provider version follows the cli version")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0o100)

	_, err = terraform.Provider{Name: "demo", Version: func() string { return "dev" }}.Install(dir)
	assert.Equal(t, cli.ExitRejected, cli.ExitCode(err), "only a release version is a provider version")

	root := &cobra.Command{Use: "demo"}
	terraform.Register(root, p)
	t.Cleanup(func() { cli.Provider = nil })
	served := false
	cli.Provider = func() error { served = true; return nil }
	t.Setenv(cli.EnvTerraform, "cookie")
	assert.Equal(t, cli.ExitOK, cli.Execute(root))
	assert.True(t, served, "the binary serves the provider when terraform starts it")
	assert.Empty(t, cli.Tools(root, "/bin/demo"), "the terraform commands are tooling, not mcp tools")
}

func providerSchemaOf(t *testing.T, p terraform.Provider) *tfprotov6.GetProviderSchemaResponse {
	schema, err := p.Server().GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	require.NoError(t, err)
	noErrors(t, schema.Diagnostics)
	return schema
}

func TestTerraformSchemaOfflineEqualsLive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newMemory("a")
	offline := demoProvider(m)
	assert.Empty(t, offline.Client.Endpoint(), "the offline provider has no endpoint to reach")
	fromCode := providerSchemaOf(t, offline)

	e, _ := stateService(m, connectionGroup(m))
	rec := call(e, http.MethodGet, "/api/state", "", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var wrapped struct {
		Data state.Document `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &wrapped))
	coded, err := json.Marshal(offline.Document)
	require.NoError(t, err)
	served, err := json.Marshal(wrapped.Data)
	require.NoError(t, err)
	assert.JSONEq(t, string(served), string(coded), "the document in code is the document the service answers")

	live := offline
	live.Document = wrapped.Data
	fromService := providerSchemaOf(t, live)
	assert.Equal(t, fromService.ResourceSchemas, fromCode.ResourceSchemas)
	assert.Equal(t, fromService.Provider, fromCode.Provider)
	assert.Len(t, fromCode.ResourceSchemas, 3)
}

func TestTerraformSchemaCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := &cobra.Command{Use: "demo", SilenceUsage: true, SilenceErrors: true}
	terraform.Register(root, demoProvider(newMemory("a")))
	t.Cleanup(func() { cli.Provider = nil })
	file := filepath.Join(t.TempDir(), "schema.json")
	root.SetArgs([]string{"terraform", "schema", "-o", file})
	require.NoError(t, root.Execute())
	body, err := os.ReadFile(file)
	require.NoError(t, err)

	type attribute struct {
		Type       json.RawMessage `json:"type"`
		NestedType *struct {
			NestingMode string                     `json:"nesting_mode"`
			Attributes  map[string]json.RawMessage `json:"attributes"`
		} `json:"nested_type"`
		Required  bool `json:"required"`
		Optional  bool `json:"optional"`
		Computed  bool `json:"computed"`
		Sensitive bool `json:"sensitive"`
		WriteOnly bool `json:"write_only"`
	}
	type block struct {
		Block struct {
			Attributes map[string]attribute `json:"attributes"`
		} `json:"block"`
	}
	var schemas struct {
		FormatVersion string `json:"format_version"`
		Schemas       map[string]struct {
			Provider  block            `json:"provider"`
			Resources map[string]block `json:"resource_schemas"`
		} `json:"provider_schemas"`
	}
	require.NoError(t, json.Unmarshal(body, &schemas))
	assert.Equal(t, "1.0", schemas.FormatVersion)
	demo, ok := schemas.Schemas["registry.terraform.io/nimling/demo"]
	require.True(t, ok, "keyed by the provider source, as terraform providers schema -json keys it")
	names := []string{}
	for name := range demo.Resources {
		names = append(names, name)
	}
	sort.Strings(names)
	assert.Equal(t, []string{"demo_connection", "demo_location", "demo_room"}, names)

	room := demo.Resources["demo_room"].Block.Attributes
	assert.JSONEq(t, `"string"`, string(room["code"].Type))
	assert.True(t, room["code"].WriteOnly && room["code"].Sensitive)
	assert.True(t, room["key"].Required)
	assert.JSONEq(t, `"number"`, string(room["seats"].Type))
	assert.True(t, demo.Resources["demo_location"].Block.Attributes["region"].Computed)
	config := demo.Resources["demo_connection"].Block.Attributes["config"]
	require.NotNil(t, config.NestedType)
	assert.Equal(t, "single", config.NestedType.NestingMode)
	assert.Contains(t, config.NestedType.Attributes, "client_secret")

	provider := demo.Provider.Block.Attributes
	for _, name := range []string{"endpoint", "client_id", "client_secret", "token", "app_slug"} {
		assert.True(t, provider[name].Optional, name)
	}
	assert.True(t, provider["client_secret"].Sensitive && provider["token"].Sensitive)
}

func TestTerraformProviderFromTheEnvironment(t *testing.T) {
	m, server, endpoint := providerHost(t)
	ctx := context.Background()
	providerType := providerSchemaOf(t, demoProvider(m)).Provider.ValueType()
	types := providerSchemaOf(t, demoProvider(m)).ResourceSchemas

	read, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{TypeName: "demo_location", CurrentState: encoded(t, types["demo_location"].ValueType(), object(types["demo_location"].ValueType(), map[string]tftypes.Value{"key": text("hq")}))})
	require.NoError(t, err)
	require.NotEmpty(t, read.Diagnostics)
	assert.Contains(t, read.Diagnostics[0].Summary, "not configured")

	missing, err := server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: encoded(t, providerType, object(providerType, nil))})
	require.NoError(t, err)
	require.NotEmpty(t, missing.Diagnostics)
	assert.Contains(t, missing.Diagnostics[0].Detail, "DEMO_ENDPOINT")

	t.Setenv("DEMO_ENDPOINT", endpoint)
	t.Setenv("DEMO_TOKEN", "stranger")
	configured, err := server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: encoded(t, providerType, object(providerType, nil))})
	require.NoError(t, err)
	noErrors(t, configured.Diagnostics)
	refused, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{TypeName: "demo_location", CurrentState: read.NewState})
	require.NoError(t, err)
	require.NotEmpty(t, refused.Diagnostics, "the service chain refuses the token from the environment")
	assert.Contains(t, refused.Diagnostics[0].Detail, "403")

	t.Setenv("DEMO_TOKEN", "owner")
	_, err = server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: encoded(t, providerType, object(providerType, nil))})
	require.NoError(t, err)
	m.add("location", "loc-1", map[string]any{"name": "Head office", "region": "north"})
	locationType := types["demo_location"].ValueType()
	found, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{TypeName: "demo_location", CurrentState: encoded(t, locationType, object(locationType, map[string]tftypes.Value{"key": text("loc-1")}))})
	require.NoError(t, err)
	noErrors(t, found.Diagnostics)
	assert.Equal(t, "Head office", stringOf(at(decodedValue(t, locationType, found.NewState), "name")))
}

func TestTerraformPackage(t *testing.T) {
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

	dist := filepath.Join(dir, "dist")
	root := &cobra.Command{Use: "demo", SilenceUsage: true, SilenceErrors: true}
	terraform.Register(root, terraform.Provider{Name: "demo", Version: func() string { return "v1.2.3" }})
	t.Cleanup(func() { cli.Provider = nil })
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"terraform", "package", "--dir", dist, "--modules", filepath.Join(dir, "deploy", "terraform"),
		"linux_amd64=" + filepath.Join(build, "demo_linux_amd64"), "darwin_arm64=" + filepath.Join(build, "demo_darwin_arm64")})
	require.NoError(t, root.Execute())

	files := terraform.GetFiles("demo", "1.2.3", "", "")
	assert.Equal(t, "terraform-provider-demo_1.2.3_manifest.json", files.Manifest)
	manifest, err := os.ReadFile(filepath.Join(dist, files.Manifest))
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":1,"metadata":{"protocol_versions":["6.0"]}}`, string(manifest))

	sums, err := os.ReadFile(filepath.Join(dist, files.Sums))
	require.NoError(t, err)
	var listed []string
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		hash, name, ok := strings.Cut(line, "  ")
		require.True(t, ok, line)
		body, err := os.ReadFile(filepath.Join(dist, name))
		require.NoError(t, err)
		sum := sha256.Sum256(body)
		assert.Equal(t, hex.EncodeToString(sum[:]), hash, name)
		listed = append(listed, name)
	}
	assert.Equal(t, []string{"terraform-provider-demo_1.2.3_darwin_arm64.zip", "terraform-provider-demo_1.2.3_linux_amd64.zip", "terraform-provider-demo_1.2.3_manifest.json"}, listed, "the public registry reads the manifest from the signed sums")

	archive, err := zip.OpenReader(filepath.Join(dist, terraform.GetFiles("demo", "1.2.3", "linux", "amd64").Archive))
	require.NoError(t, err)
	defer archive.Close()
	require.Len(t, archive.File, 1)
	assert.Equal(t, "terraform-provider-demo_v1.2.3", archive.File[0].Name)
	assert.Contains(t, out.String(), terraform.GetModuleArchive("demo", "azurerm", "1.2.3"))

	root.SetArgs([]string{"terraform", "package", "--dir", dist, "linux"})
	assert.Error(t, root.Execute(), "a build is named <os>_<arch>=<binary>")
}
