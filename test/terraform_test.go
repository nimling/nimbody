package test

import (
	"bytes"
	"context"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody/cli"
	"github.com/nimling/nimbody/terraform"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const envTestCLI = "NIMBODY_TEST_CLI"

func TestMain(m *testing.M) {
	if endpoint := os.Getenv(envTestCLI); endpoint != "" {
		os.Exit(cli.Execute(testCLI(endpoint)))
	}
	os.Exit(m.Run())
}

func testCLI(endpoint string) *cobra.Command {
	root := &cobra.Command{Use: "demo"}
	send := func(cmd *cobra.Command, method, path string, query url.Values, contentType string, body []byte) ([]byte, error) {
		target := endpoint + "/api" + path
		if len(query) > 0 {
			target += "?" + query.Encode()
		}
		req, err := http.NewRequestWithContext(cmd.Context(), method, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set(echo.HeaderAuthorization, "Bearer owner")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		answer, err := io.ReadAll(res.Body)
		if err != nil {
			return nil, err
		}
		if res.StatusCode >= http.StatusBadRequest {
			return nil, cli.Rejected(string(answer), "")
		}
		return answer, nil
	}
	root.AddCommand(cli.StateCommand(send, nil))
	return root
}

func providerHost(t *testing.T) (*memory, tfprotov6.ProviderServer) {
	m := newMemory("a")
	e, _ := stateService(m, connectionGroup(m))
	host := httptest.NewServer(e)
	t.Cleanup(host.Close)
	t.Setenv(envTestCLI, host.URL)
	executable, err := os.Executable()
	require.NoError(t, err)
	p := terraform.Provider{Name: "demo", Version: func() string { return "v1.2.3" }, Executable: executable}
	return m, p.Server()
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
	m, server := providerHost(t)
	ctx := context.Background()
	schema, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	require.NoError(t, err)
	noErrors(t, schema.Diagnostics)
	providerType := schema.Provider.ValueType()
	configured, err := server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: encoded(t, providerType, object(providerType, nil))})
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
	assert.Equal(t, filepath.Join(dir, "registry.terraform.io", "samna", "demo", "1.2.3", runtime.GOOS+"_"+runtime.GOARCH, file), path, "the provider version follows the cli version")
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
