package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody/cli"
	"github.com/nimling/nimbody/mcp"
	"github.com/nimling/nimbody/respond"
	"github.com/nimling/nimbody/state"
	"github.com/nimling/nimbody/stream"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

type locationBody struct {
	Name   string `json:"name"`
	Region string `json:"region,omitempty"`
}

type roomBody struct {
	Name       string `json:"name"`
	LocationID string `json:"location_id"`
	Seats      int    `json:"seats"`
	Kind       string `json:"kind"`
	Code       string `json:"code,omitempty"`
}

type stored struct {
	id       string
	external string
	data     map[string]any
	blobs    map[string]state.Blob
}

type memory struct {
	mu     sync.Mutex
	prefix string
	next   int
	writes int
	rows   map[string][]*stored
	assets map[string][]byte
}

func newMemory(prefix string) *memory {
	return &memory{prefix: prefix, rows: map[string][]*stored{}, assets: map[string][]byte{}}
}

func (m *memory) add(group, id string, data map[string]any) *stored {
	row := &stored{id: id, data: data, blobs: map[string]state.Blob{}}
	m.rows[group] = append(m.rows[group], row)
	return row
}

func (m *memory) key(row *stored) string {
	if row.external != "" {
		return row.external
	}
	return row.id
}

func (m *memory) GetID(ctx context.Context, group, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.rows[group] {
		if row.external == key || row.id == key {
			return row.id, nil
		}
	}
	return "", nil
}

func (m *memory) GetKey(ctx context.Context, group, id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.rows[group] {
		if row.id == id {
			return m.key(row), nil
		}
	}
	return "", nil
}

func (m *memory) count(group string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows[group])
}

func memoryGroup[T any](m *memory, group string) state.Group[T] {
	return state.Group[T]{
		Name: group,
		Export: func(ctx *state.Context, query state.Query) ([]state.Record[T], error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			var out []state.Record[T]
			for _, row := range m.rows[group] {
				if len(query.Keys) > 0 && !containsString(query.Keys, m.key(row)) {
					continue
				}
				matched := true
				for field, value := range query.Filter {
					if fmt.Sprint(row.data[field]) != value {
						matched = false
					}
				}
				if !matched {
					continue
				}
				body, _ := json.Marshal(row.data)
				var data T
				if err := json.Unmarshal(body, &data); err != nil {
					return nil, err
				}
				out = append(out, state.Record[T]{ID: row.id, Key: row.external, Data: data})
			}
			return out, nil
		},
		Import: func(ctx *state.Context, record state.Record[T]) (string, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.writes++
			body, _ := json.Marshal(record.Data)
			data := map[string]any{}
			_ = json.Unmarshal(body, &data)
			if group == "location" && data["region"] == nil {
				data["region"] = "north"
			}
			if record.ID == "" {
				m.next++
				row := &stored{id: fmt.Sprintf("%s-%d", m.prefix, m.next), external: record.Key, data: data, blobs: map[string]state.Blob{}}
				m.rows[group] = append(m.rows[group], row)
				return row.id, nil
			}
			for _, row := range m.rows[group] {
				if row.id == record.ID {
					if !record.Body.HasKey("code") && row.data["code"] != nil {
						data["code"] = row.data["code"]
					}
					row.data = data
					return row.id, nil
				}
			}
			return "", echo.NewHTTPError(http.StatusNotFound, "no "+group+" "+record.ID)
		},
		Delete: func(ctx *state.Context, id string) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.writes++
			kept := m.rows[group][:0]
			for _, row := range m.rows[group] {
				if row.id != id {
					kept = append(kept, row)
				}
			}
			m.rows[group] = kept
			return nil
		},
	}
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func stateService(m *memory, extra ...state.Definition) (*echo.Echo, []mcp.Operation) {
	location := memoryGroup[locationBody](m, "location")
	location.Schema = map[string]any{"type": "object", "properties": map[string]any{
		"name":   map[string]any{"type": "string"},
		"region": map[string]any{"type": "string", "x-server-default": true},
	}}
	room := memoryGroup[roomBody](m, "room")
	room.Schema = map[string]any{"type": "object", "properties": map[string]any{
		"name":        map[string]any{"type": "string"},
		"location_id": map[string]any{"type": "string"},
		"seats":       map[string]any{"type": "integer"},
		"kind":        map[string]any{"type": "string", "x-immutable": true},
		"code":        map[string]any{"type": "string", "writeOnly": true},
	}}
	room.DependsOn = []string{"location"}
	room.Refs = map[string]string{"location_id": "location"}
	room.Filters = []string{"kind"}
	room.Blobs = state.Blobs[roomBody]{
		Get: func(ctx *state.Context, record state.Record[roomBody]) ([]state.Blob, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			var out []state.Blob
			for _, row := range m.rows["room"] {
				if row.id == record.ID {
					for _, blob := range row.blobs {
						out = append(out, blob)
					}
				}
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
			return out, nil
		},
		Put: func(ctx *state.Context, record state.Record[roomBody], blob state.Blob) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.writes++
			for _, row := range m.rows["room"] {
				if row.id == record.ID {
					row.blobs[blob.Name] = state.Blob{Name: blob.Name, MIME: blob.MIME, Data: blob.Data}
				}
			}
			return nil
		},
	}
	access := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(ctx echo.Context) error {
			if ctx.Request().Header.Get(echo.HeaderAuthorization) != "Bearer owner" {
				return respond.Error(ctx, http.StatusForbidden, "the caller owns no root organization")
			}
			return next(ctx)
		}
	}
	host := state.Context{
		Service: "demo",
		Version: "v1.2.3",
		Access:  access,
		Keys:    m,
		Transaction: func(ctx context.Context, run func(ctx context.Context) error) error {
			return run(ctx)
		},
		PostAsset: func(ctx echo.Context, name string, body []byte) (string, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			id := fmt.Sprintf("asset-%d", len(m.assets)+1)
			m.assets[id] = body
			return id, nil
		},
		GetAsset: func(ctx echo.Context, assetID string) ([]byte, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			body, ok := m.assets[assetID]
			if !ok {
				return nil, echo.NewHTTPError(http.StatusNotFound, "no asset "+assetID)
			}
			return body, nil
		},
	}
	e := echo.New()
	operations := state.Register(e.Group("/api"), host, append([]state.Definition{room, location}, extra...)...)
	return e, operations
}

func seeded() *memory {
	m := newMemory("a")
	m.add("location", "loc-1", map[string]any{"name": "Head office", "region": "north"})
	m.add("location", "loc-2", map[string]any{"name": "Annex", "region": "west"})
	north := m.add("room", "room-1", map[string]any{"name": "North", "location_id": "loc-1", "seats": 8, "kind": "meeting", "code": "1234"})
	north.blobs["photo"] = state.Blob{Name: "photo", MIME: "image/png", Data: []byte("north photo")}
	m.add("room", "room-2", map[string]any{"name": "Desk 4", "location_id": "loc-2", "seats": 1, "kind": "desk", "code": "5678"})
	return m
}

func call(e *echo.Echo, method, target, contentType, accept string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.Header.Set(echo.HeaderAuthorization, "Bearer owner")
	if contentType != "" {
		req.Header.Set(echo.HeaderContentType, contentType)
	}
	if accept != "" {
		req.Header.Set(echo.HeaderAccept, accept)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func exportState(t *testing.T, e *echo.Echo, body string) []byte {
	rec := call(e, http.MethodPost, "/api/state/export", echo.MIMEApplicationJSON, "", []byte(body))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, state.MIMENDJSON, rec.Header().Get(echo.HeaderContentType))
	return rec.Body.Bytes()
}

func importState(t *testing.T, e *echo.Echo, query string, file []byte) state.ImportResponse {
	rec := call(e, http.MethodPost, "/api/state/import?"+query, state.MIMENDJSON, "", file)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var wrapped struct {
		Data state.ImportResponse `json:"data"`
	}
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &wrapped))
	return wrapped.Data
}

func outcomes(out state.ImportResponse) map[string]string {
	byKey := map[string]string{}
	for _, result := range out.Results {
		byKey[result.Group+":"+result.Key] = result.Outcome
	}
	return byKey
}

func TestStateSchemaDocument(t *testing.T) {
	e, _ := stateService(newMemory("a"))
	rec := call(e, http.MethodGet, "/api/state", "", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var wrapped struct {
		Data state.Document `json:"data"`
	}
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &wrapped))
	doc := wrapped.Data
	assert.Equal(t, state.FormatVersion, doc.Format)
	assert.Equal(t, "demo", doc.Service)
	assert.Equal(t, []string{"location", "room"}, []string{doc.Groups[0].Name, doc.Groups[1].Name}, "dependencies first, whatever the registration order")
	room := doc.Groups[1]
	assert.Equal(t, []string{"location"}, room.DependsOn)
	assert.Equal(t, map[string]string{"location_id": "location"}, room.Refs)
	assert.Equal(t, []string{"kind"}, room.Filters)
	assert.Equal(t, []string{"/code"}, room.Fields.Sensitive)
	assert.Equal(t, []string{"/kind"}, room.Fields.Immutable)
	assert.Equal(t, []string{"/region"}, doc.Groups[0].Fields.ServerDefault)
	assert.Equal(t, map[string][]string{}, room.Fields.SensitiveBy)
	assert.Contains(t, room.Schema["properties"], "code")

	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	refused := httptest.NewRecorder()
	e.ServeHTTP(refused, req)
	assert.Equal(t, http.StatusForbidden, refused.Code, "the host's access hook guards every route")
}

func TestStateRoundTrip(t *testing.T) {
	source, _ := stateService(seeded())
	file := exportState(t, source, `{"blobs":true}`)

	lines := strings.Split(strings.TrimSpace(string(file)), "\n")
	assert.Len(t, lines, 6, "a manifest, two locations, two rooms and one blob")
	var manifest state.Manifest
	assert.NoError(t, json.Unmarshal([]byte(lines[0]), &manifest))
	assert.Equal(t, []string{"location", "room"}, []string{manifest.Groups[0].Name, manifest.Groups[1].Name})
	assert.Equal(t, 2, manifest.Groups[1].Count)
	assert.Equal(t, 1, manifest.Groups[1].Blobs)
	assert.NotEmpty(t, manifest.Groups[0].Hash)
	assert.JSONEq(t, `{"group":"room","key":"room-1","data":{"name":"North","seats":8,"kind":"meeting"},"refs":{"location_id":"loc-1"}}`, lines[3])

	target := newMemory("b")
	empty, _ := stateService(target)
	first := importState(t, empty, "", file)
	assert.Equal(t, map[string]int{state.OutcomeCreated: 4}, first.Counts)
	for _, result := range first.Results {
		assert.True(t, strings.HasPrefix(result.ID, "b-"), "a created record answers its new id")
	}
	assert.Equal(t, file, exportState(t, empty, `{"blobs":true}`), "the import holds the same records, refs and blobs under new ids")

	writes := target.writes
	second := importState(t, empty, "", file)
	assert.Equal(t, map[string]int{state.OutcomeUnchanged: 4}, second.Counts, "the key is kept as the external id, so a second run converges")
	assert.Equal(t, writes, target.writes, "an unchanged record is never written")
}

func TestStateNeverExportsWriteOnlyFields(t *testing.T) {
	e, _ := stateService(seeded())
	file := string(exportState(t, e, `{"blobs":true}`))
	assert.NotContains(t, file, `"code"`)
	assert.NotContains(t, file, "1234")
	assert.NotContains(t, file, "5678")
}

func TestStateSyncPrunesInsideTheScope(t *testing.T) {
	source, _ := stateService(seeded())
	file := exportState(t, source, ``)

	target := newMemory("b")
	e, _ := stateService(target)
	importState(t, e, "", file)
	target.add("room", "stray-desk", map[string]any{"name": "Stray", "location_id": "b-1", "seats": 1, "kind": "desk"})
	target.add("room", "stray-meeting", map[string]any{"name": "Stray", "location_id": "b-1", "seats": 4, "kind": "meeting"})

	upsert := importState(t, e, "", file)
	assert.Equal(t, map[string]int{state.OutcomeUnchanged: 4}, upsert.Counts, "upsert never deletes")

	synced := importState(t, e, "mode=sync&target=room:kind=desk", file)
	assert.Equal(t, state.OutcomeDeleted, outcomes(synced)["room:stray-desk"])
	assert.NotContains(t, outcomes(synced), "room:stray-meeting", "a sync prunes only what its targets reach")
	assert.NotContains(t, outcomes(synced), "location:loc-1", "a group outside the targets is neither imported nor pruned")
	assert.Equal(t, 3, target.count("room"))

	synced = importState(t, e, "mode=sync", file)
	assert.Equal(t, state.OutcomeDeleted, outcomes(synced)["room:stray-meeting"])
	assert.Equal(t, 2, target.count("room"))

	deleted := importState(t, e, "delete=room:room-2&target=location", file)
	assert.Equal(t, state.OutcomeDeleted, outcomes(deleted)["room:room-2"])
	assert.Equal(t, 1, target.count("room"))

	rec := call(e, http.MethodPost, "/api/state/import?delete=room:room-1", state.MIMENDJSON, "", file)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a key both imported and deleted is rejected before anything is written")
}

func TestStateDryRunWritesNothing(t *testing.T) {
	source, _ := stateService(seeded())
	file := exportState(t, source, `{"blobs":true}`)

	target := newMemory("b")
	e, _ := stateService(target)
	planned := importState(t, e, "dry_run=true", file)
	assert.True(t, planned.DryRun)
	assert.Equal(t, map[string]int{state.OutcomeCreated: 4}, planned.Counts, "a room referencing a location created in the same run resolves")
	assert.Equal(t, 0, target.writes)
	assert.Equal(t, 0, target.count("location"))

	importState(t, e, "", file)
	target.add("room", "stray", map[string]any{"name": "Stray", "location_id": "b-1", "seats": 1, "kind": "desk"})
	writes := target.writes
	planned = importState(t, e, "dry_run=true&mode=sync", file)
	assert.Equal(t, state.OutcomeDeleted, outcomes(planned)["room:stray"], "a dry run lists every delete")
	assert.Equal(t, writes, target.writes)
	assert.Equal(t, 3, target.count("room"))
}

func TestStateFieldFlagsDecideDrift(t *testing.T) {
	source, _ := stateService(seeded())
	file := exportState(t, source, ``)
	target := newMemory("b")
	e, _ := stateService(target)
	importState(t, e, "", file)

	edited := strings.Replace(string(file), `"region":"north",`, ``, 1)
	edited = strings.Replace(edited, `"kind":"desk"`, `"kind":"meeting"`, 1)
	edited = strings.Replace(edited, `"seats":8`, `"seats":10`, 1)
	read, err := state.Read(strings.NewReader(edited))
	assert.Error(t, err, "an edited group no longer matches its hash")
	assert.Empty(t, read.Records)

	var unhashed bytes.Buffer
	read, err = state.Read(strings.NewReader(string(file)))
	assert.NoError(t, err)
	for i := range read.Manifest.Groups {
		read.Manifest.Groups[i].Hash = ""
	}
	assert.NoError(t, read.Write(&unhashed))
	edited = strings.Replace(unhashed.String(), `"region":"north",`, ``, 1)
	edited = strings.Replace(edited, `"kind":"desk"`, `"kind":"meeting"`, 1)
	edited = strings.Replace(edited, `"seats":8`, `"seats":10`, 1)

	out := importState(t, e, "", []byte(edited))
	byKey := outcomes(out)
	assert.Equal(t, state.OutcomeUnchanged, byKey["location:loc-1"], "an omitted server default is not drift")
	assert.Equal(t, state.OutcomeUpdated, byKey["room:room-1"])
	assert.Equal(t, state.OutcomeRefused, byKey["room:room-2"], "an immutable field cannot change in place")
	for _, result := range out.Results {
		if result.Key == "room-2" {
			assert.Contains(t, result.Error, "room.kind cannot change")
		}
	}

	created := importState(t, e, "mode=create", []byte(edited))
	assert.Equal(t, state.OutcomeRefused, outcomes(created)["room:room-2"])
	assert.Equal(t, state.OutcomeUnchanged, outcomes(created)["room:room-1"])
}

func TestStateImportReportsOverTheStepStream(t *testing.T) {
	source, _ := stateService(seeded())
	file := exportState(t, source, ``)
	e, _ := stateService(newMemory("b"))
	rec := call(e, http.MethodPost, "/api/state/import", state.MIMENDJSON, respond.MIMEEventStream, file)
	var steps []respond.Step
	out, err := stream.Read(rec.Result(), func(step respond.Step) { steps = append(steps, step) })
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, out.Status)
	var results []state.Result
	for _, step := range steps {
		if step.Status != "done" || step.ParentID == "" {
			continue
		}
		body, _ := json.Marshal(step.Data)
		var result state.Result
		assert.NoError(t, json.Unmarshal(body, &result))
		results = append(results, result)
	}
	assert.Len(t, results, 4, "one step per record carrying its result")
	assert.Equal(t, state.OutcomeCreated, results[0].Outcome)
	assert.Contains(t, string(out.Body), `"outcome":"created"`)
}

func TestStateStoreAndImportAnAsset(t *testing.T) {
	m := seeded()
	e, operations := stateService(m)
	rec := call(e, http.MethodPost, "/api/state/export", echo.MIMEApplicationJSON, "", []byte(`{"store":true}`))
	assert.Equal(t, http.StatusCreated, rec.Code)
	var wrapped struct {
		Data state.Stored `json:"data"`
	}
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &wrapped))
	assert.Equal(t, "asset-1", wrapped.Data.AssetID)

	rec = call(e, http.MethodPost, "/api/state/import", echo.MIMEApplicationJSON, "", []byte(`{"asset_id":"asset-1","dry_run":true}`))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"unchanged": 4`)

	names := []string{}
	for _, operation := range operations {
		names = append(names, operation.Name+":"+operation.Access)
	}
	assert.Equal(t, []string{"state:read", "state_export:read", "state_import:destructive"}, names)
	server := &mcp.Server{Name: "demo", Version: "v1", Operations: operations}
	server.Mount(e, func(next echo.HandlerFunc) echo.HandlerFunc { return next })
	reply := call(e, http.MethodPost, "/mcp", echo.MIMEApplicationJSON, echo.MIMEApplicationJSON, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"state_import","arguments":{"asset_id":"asset-1","dry_run":true}}}`))
	assert.Equal(t, http.StatusOK, reply.Code)
	called := result(t, reply)
	assert.Nil(t, called["isError"], fmt.Sprint(called))
	assert.Equal(t, float64(4), called["structuredContent"].(map[string]any)["data"].(map[string]any)["counts"].(map[string]any)["unchanged"])
}

func TestStateCommand(t *testing.T) {
	source, _ := stateService(seeded())
	target := newMemory("b")
	e, _ := stateService(target)
	backend := source
	send := func(cmd *cobra.Command, method, path string, query url.Values, contentType string, body []byte) ([]byte, error) {
		target := "/api" + path
		if len(query) > 0 {
			target += "?" + query.Encode()
		}
		rec := call(backend, method, target, contentType, respond.MIMEEventStream, body)
		out, err := stream.Read(rec.Result(), nil)
		if err != nil {
			return nil, err
		}
		if out.Status >= http.StatusBadRequest {
			return nil, cli.Rejected(string(out.Body), "")
		}
		return out.Body, nil
	}
	run := func(args ...string) (string, string, error) {
		root := &cobra.Command{Use: "demo"}
		output := &cli.Output{JSON: true}
		root.AddCommand(cli.StateCommand(send, output))
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs(args)
		err := root.Execute()
		return stdout.String(), stderr.String(), err
	}

	out, _, err := run("state")
	assert.NoError(t, err)
	assert.Contains(t, out, `"format": "2"`)

	file := filepath.Join(t.TempDir(), "rooms.ndjson")
	_, errOut, err := run("state", "export", "--target", "room:kind=meeting", "--target", "location", "--blobs", "-o", file)
	assert.NoError(t, err)
	assert.Equal(t, "exported 3 records in 2 groups with 1 blob to "+file+"\n", errOut)
	written, _ := os.ReadFile(file)
	assert.NotContains(t, string(written), "Desk 4")

	backend = e
	out, errOut, err = run("state", "import", file, "--dry-run")
	assert.NoError(t, err)
	assert.Contains(t, out, `"outcome": "created"`)
	assert.Equal(t, "upsert dry run, nothing written: 3 created\n", errOut)
	assert.Equal(t, 0, target.writes)

	_, errOut, err = run("state", "import", file, "--mode", "create")
	assert.NoError(t, err)
	assert.Equal(t, "create: 3 created\n", errOut)

	_, _, err = run("state", "import", file, "--delete", "room:room-1")
	assert.Error(t, err)

	tools := cli.Tools(func() *cobra.Command {
		root := &cobra.Command{Use: "demo"}
		root.AddCommand(cli.StateCommand(send, nil))
		return root
	}(), "/bin/demo")
	access := map[string]any{}
	for _, tool := range tools {
		access[tool.Name] = tool.Annotations["readOnlyHint"]
	}
	assert.Equal(t, map[string]any{"state": true, "state_export": true, "state_import": false}, access)
}

type connectionBody struct {
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Config map[string]any `json:"config,omitempty"`
	Auth   map[string]any `json:"auth,omitempty"`
}

func connectionGroup(m *memory) state.Group[connectionBody] {
	connection := memoryGroup[connectionBody](m, "connection")
	connection.Schema = map[string]any{"type": "object", "properties": map[string]any{
		"name": map[string]any{"type": "string"},
		"type": map[string]any{"type": "string", "x-immutable": true},
		"config": map[string]any{"type": "object", "properties": map[string]any{
			"client_id":     map[string]any{"type": "string"},
			"client_secret": map[string]any{"type": "string", "writeOnly": true},
			"region":        map[string]any{"type": "string", "x-server-default": true},
			"mode":          map[string]any{"type": "string", "x-immutable": true},
		}},
		"auth": map[string]any{
			"type":          "object",
			"discriminator": map[string]any{"propertyName": "kind"},
			"oneOf": []any{
				map[string]any{"properties": map[string]any{"kind": map[string]any{"type": "string"}, "token": map[string]any{"type": "string", "x-sensitive": true}}},
				map[string]any{"properties": map[string]any{"kind": map[string]any{"type": "string"}, "user": map[string]any{"type": "string"}}},
			},
		},
	}}
	connection.Discriminator = "/type"
	connection.Sensitive = func() map[string][]string {
		return map[string][]string{"oauth": {"/config/refresh_token"}, "basic": {"/config/password", "/config/password"}}
	}
	return connection
}

func connectionService(m *memory) *echo.Echo {
	e := echo.New()
	state.Register(e.Group("/api"), state.Context{Service: "demo", Version: "v1.2.3", Keys: m}, connectionGroup(m))
	return e
}

func connections() *memory {
	m := newMemory("a")
	m.add("connection", "conn-1", map[string]any{"name": "Calendar", "type": "oauth",
		"config": map[string]any{"client_id": "public-id", "client_secret": "s3cret", "refresh_token": "r3fresh", "region": "eu", "mode": "push"},
		"auth":   map[string]any{"kind": "token", "token": "t0ken"}})
	m.add("connection", "conn-2", map[string]any{"name": "Printer", "type": "basic",
		"config": map[string]any{"client_id": "printer", "password": "p4ss", "refresh_token": "listed", "region": "us", "mode": "pull"},
		"auth":   map[string]any{"kind": "user", "user": "operator"}})
	return m
}

func TestStateSensitiveByInTheSchema(t *testing.T) {
	e := connectionService(newMemory("a"))
	rec := call(e, http.MethodGet, "/api/state", "", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"sensitive_by": {`)
	var wrapped struct {
		Data state.Document `json:"data"`
	}
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &wrapped))
	fields := wrapped.Data.Groups[0].Fields
	assert.Equal(t, []string{"/auth/token", "/config/client_secret"}, fields.Sensitive, "nested properties and discriminated branches are walked")
	assert.Equal(t, []string{"/config/mode", "/type"}, fields.Immutable)
	assert.Equal(t, []string{"/config/region"}, fields.ServerDefault)
	assert.Equal(t, "/type", fields.Discriminator)
	assert.Equal(t, map[string][]string{"basic": {"/config/password"}, "oauth": {"/config/refresh_token"}}, fields.SensitiveBy)
}

func TestStateStripsNestedPointers(t *testing.T) {
	e := connectionService(connections())
	file := string(exportState(t, e, ``))
	lines := strings.Split(strings.TrimSpace(file), "\n")
	assert.Len(t, lines, 3)
	assert.JSONEq(t, `{"group":"connection","key":"conn-1","data":{"name":"Calendar","type":"oauth","config":{"client_id":"public-id","region":"eu","mode":"push"},"auth":{"kind":"token"}}}`, lines[1], "exactly the flagged pointers go, the objects holding them stay")
	assert.NotContains(t, file, "s3cret")
	assert.NotContains(t, file, "t0ken")
}

func TestStateStripsByDiscriminator(t *testing.T) {
	e := connectionService(connections())
	file := string(exportState(t, e, ``))
	lines := strings.Split(strings.TrimSpace(file), "\n")
	assert.NotContains(t, lines[1], "r3fresh", "an oauth record loses its refresh token")
	assert.JSONEq(t, `{"group":"connection","key":"conn-2","data":{"name":"Printer","type":"basic","config":{"client_id":"printer","refresh_token":"listed","region":"us","mode":"pull"},"auth":{"kind":"user","user":"operator"}}}`, lines[2], "a basic record loses its password and keeps what only oauth hides")
	assert.NotContains(t, file, "p4ss")
}

func TestStateFieldFlagsByPointer(t *testing.T) {
	source := connectionService(connections())
	exported, err := state.Read(bytes.NewReader(exportState(t, source, ``)))
	assert.NoError(t, err)
	for i := range exported.Manifest.Groups {
		exported.Manifest.Groups[i].Hash = ""
	}
	var unhashed bytes.Buffer
	assert.NoError(t, exported.Write(&unhashed))

	target := newMemory("b")
	e := connectionService(target)
	assert.Equal(t, map[string]int{state.OutcomeCreated: 2}, importState(t, e, "", unhashed.Bytes()).Counts)
	assert.Equal(t, map[string]int{state.OutcomeUnchanged: 2}, importState(t, e, "", unhashed.Bytes()).Counts, "stripped secrets are not drift")

	edited := strings.Replace(unhashed.String(), `"region":"eu",`, ``, 1)
	edited = strings.Replace(edited, `"mode":"pull"`, `"mode":"push"`, 1)
	out := importState(t, e, "", []byte(edited))
	assert.Equal(t, state.OutcomeUnchanged, outcomes(out)["connection:conn-1"], "an omitted nested server default is not drift")
	assert.Equal(t, state.OutcomeRefused, outcomes(out)["connection:conn-2"])
	for _, result := range out.Results {
		if result.Key == "conn-2" {
			assert.Contains(t, result.Error, "connection.config.mode cannot change")
		}
	}

	withSecret := strings.Replace(unhashed.String(), `"client_id":"public-id"`, `"client_id":"public-id","refresh_token":"new"`, 1)
	assert.Equal(t, state.OutcomeUpdated, outcomes(importState(t, e, "", []byte(withSecret)))["connection:conn-1"], "a secret sent for the record's own type is always written")
}
