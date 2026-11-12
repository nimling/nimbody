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
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody/cli"
	"github.com/nimling/nimbody/endpoint"
	"github.com/nimling/nimbody/mcp"
	"github.com/nimling/nimbody/respond"
	"github.com/nimling/nimbody/state"
	"github.com/nimling/nimbody/stream"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
				id := record.Origin
				for _, row := range m.rows[group] {
					if row.id == id {
						id = ""
					}
				}
				if id == "" {
					m.next++
					id = fmt.Sprintf("%s-%d", m.prefix, m.next)
				}
				row := &stored{id: id, external: record.Key, data: data, blobs: map[string]state.Blob{}}
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

func stateGroups(m *memory, extra ...state.Definition) []state.Definition {
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
	return append([]state.Definition{room, location}, extra...)
}

func stateService(m *memory, extra ...state.Definition) (*echo.Echo, []mcp.Operation) {
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
	operations := state.Register(host, []echo.MiddlewareFunc{access}, stateGroups(m, extra...)...)
	mcp.Route(e.Group("/api"), operations...)
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
	assert.JSONEq(t, `{"group":"room","key":"room-1","id":"room-1","data":{"name":"North","seats":8,"kind":"meeting"},"refs":{"location_id":"loc-1"}}`, lines[3])

	target := newMemory("b")
	empty, _ := stateService(target)
	first := importState(t, empty, "", file)
	assert.Equal(t, map[string]int{state.OutcomeCreated: 4}, first.Counts)
	for _, result := range first.Results {
		assert.Equal(t, result.Key, result.ID, "a created record keeps the id of its line when that id is free")
	}
	assert.Equal(t, file, exportState(t, empty, `{"blobs":true}`), "the import holds the same records, refs and blobs under the same ids")

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

func TestStateReplacePrunesEveryGroup(t *testing.T) {
	source, _ := stateService(seeded())
	file := exportState(t, source, ``)

	target := newMemory("b")
	e, _ := stateService(target)
	importState(t, e, "", file)
	target.add("location", "stray-site", map[string]any{"name": "Stray site", "region": "south"})
	target.add("room", "stray-desk", map[string]any{"name": "Stray", "location_id": "b-1", "seats": 1, "kind": "desk"})

	synced := importState(t, e, "mode=sync&target=room:kind=meeting", file)
	assert.NotContains(t, outcomes(synced), "room:stray-desk", "a sync prunes only what its targets reach")
	assert.NotContains(t, outcomes(synced), "location:stray-site")
	assert.Equal(t, 3, target.count("room"))
	assert.Equal(t, 3, target.count("location"))

	planned := importState(t, e, "dry_run=true&mode=replace&target=room", file)
	assert.Equal(t, state.OutcomeDeleted, outcomes(planned)["room:stray-desk"])
	assert.Equal(t, state.OutcomeDeleted, outcomes(planned)["location:stray-site"], "a replace prunes a group outside the targets")
	assert.Equal(t, 3, target.count("room"), "a dry run writes nothing")
	assert.Equal(t, 3, target.count("location"))

	replaced := importState(t, e, "mode=replace", file)
	assert.Equal(t, state.ModeReplace, replaced.Mode)
	assert.Equal(t, state.OutcomeDeleted, outcomes(replaced)["room:stray-desk"])
	assert.Equal(t, state.OutcomeDeleted, outcomes(replaced)["location:stray-site"], "only the imported state stays")
	assert.Equal(t, state.OutcomeUnchanged, outcomes(replaced)["location:loc-1"])
	assert.Equal(t, 2, target.count("room"))
	assert.Equal(t, 2, target.count("location"))
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

func TestStateModesDecideWhatAnExistingRecordGets(t *testing.T) {
	m := seeded()
	source, _ := stateService(m)
	base := exportState(t, source, `{"blobs":true}`)

	read, err := state.Read(bytes.NewReader(base))
	require.NoError(t, err)
	for i := range read.Manifest.Groups {
		read.Manifest.Groups[i].Hash = ""
	}
	var unhashed bytes.Buffer
	require.NoError(t, read.Write(&unhashed))
	edited := strings.Replace(unhashed.String(), `"seats":8`, `"seats":10`, 1)
	edited = strings.Replace(edited, `"kind":"desk"`, `"kind":"meeting"`, 1)
	input := []byte(edited + `{"group":"room","key":"room-3","data":{"name":"Hub","seats":2,"kind":"desk"},"refs":{"location_id":"loc-1"}}` + "\n")

	m.rows["room"][0].blobs["photo"] = state.Blob{Name: "photo", MIME: "image/png", Data: []byte("south photo")}
	drifted := exportState(t, source, `{"blobs":true}`)

	prepared := func() (*echo.Echo, *memory) {
		target := newMemory("b")
		e, _ := stateService(target)
		importState(t, e, "", base)
		return e, target
	}
	room := func(target *memory, id string) *stored {
		for _, row := range target.rows["room"] {
			if row.id == id {
				return row
			}
		}
		return &stored{blobs: map[string]state.Blob{}}
	}

	for _, mode := range []struct {
		name       string
		query      string
		missing    string
		identical  string
		differs    string
		immutable  string
		drift      string
		seats      string
		kind       string
		photo      string
		writes     int
		blobWrites int
	}{
		{name: state.ModeUpsert, query: "", missing: state.OutcomeCreated, identical: state.OutcomeUnchanged, differs: state.OutcomeUpdated, immutable: state.OutcomeRefused, drift: state.OutcomeUpdated, seats: "10", kind: "desk", photo: "south photo", writes: 2, blobWrites: 1},
		{name: state.ModeCreate, query: "mode=create", missing: state.OutcomeCreated, identical: state.OutcomeUnchanged, differs: state.OutcomeRefused, immutable: state.OutcomeRefused, drift: state.OutcomeRefused, seats: "8", kind: "desk", photo: "north photo", writes: 1, blobWrites: 0},
		{name: state.ModeSync, query: "mode=sync", missing: state.OutcomeCreated, identical: state.OutcomeUnchanged, differs: state.OutcomeUpdated, immutable: state.OutcomeRefused, drift: state.OutcomeUpdated, seats: "10", kind: "desk", photo: "south photo", writes: 2, blobWrites: 1},
		{name: state.ModeSkip, query: "mode=skip", missing: state.OutcomeCreated, identical: state.OutcomeUnchanged, differs: state.OutcomeUnchanged, immutable: state.OutcomeUnchanged, drift: state.OutcomeUnchanged, seats: "8", kind: "desk", photo: "north photo", writes: 1, blobWrites: 0},
		{name: state.ModeReplace, query: "mode=replace", missing: state.OutcomeCreated, identical: state.OutcomeUnchanged, differs: state.OutcomeUpdated, immutable: state.OutcomeRefused, drift: state.OutcomeUpdated, seats: "10", kind: "desk", photo: "south photo", writes: 2, blobWrites: 1},
	} {
		t.Run(mode.name, func(t *testing.T) {
			want := map[string]string{
				"room:room-3":    mode.missing,
				"location:loc-1": mode.identical,
				"location:loc-2": mode.identical,
				"room:room-1":    mode.differs,
				"room:room-2":    mode.immutable,
			}
			e, target := prepared()
			writes := target.writes

			planned := importState(t, e, "dry_run=true&"+mode.query, input)
			assert.Equal(t, mode.name, planned.Mode, "a caller that sends no mode gets upsert")
			assert.True(t, planned.DryRun)
			assert.Equal(t, want, outcomes(planned), "a dry run reports the outcomes of its mode")
			assert.Equal(t, writes, target.writes, "a dry run writes nothing")
			assert.Equal(t, 2, target.count("room"))

			out := importState(t, e, mode.query, input)
			assert.Equal(t, mode.name, out.Mode)
			assert.Equal(t, want, outcomes(out))
			assert.Equal(t, writes+mode.writes, target.writes)
			assert.Equal(t, 3, target.count("room"), "a record the target lacks is created in every mode")
			assert.Equal(t, mode.seats, fmt.Sprint(room(target, "room-1").data["seats"]))
			assert.Equal(t, mode.kind, fmt.Sprint(room(target, "room-2").data["kind"]), "an immutable field never changes in place")

			blobbed, blobTarget := prepared()
			writes = blobTarget.writes
			planned = importState(t, blobbed, "dry_run=true&"+mode.query, drifted)
			assert.Equal(t, mode.drift, outcomes(planned)["room:room-1"])
			assert.Equal(t, writes, blobTarget.writes)

			out = importState(t, blobbed, mode.query, drifted)
			assert.Equal(t, mode.drift, outcomes(out)["room:room-1"], "a record whose blob alone differs")
			assert.Equal(t, writes+mode.blobWrites, blobTarget.writes)
			assert.Equal(t, mode.photo, string(room(blobTarget, "room-1").blobs["photo"].Data))
		})
	}
}

func TestStateExportDeclaresAnUnboundedFrame(t *testing.T) {
	e, _ := stateService(seeded())
	rec := call(e, http.MethodPost, "/api/state/export", echo.MIMEApplicationJSON, respond.MIMEEventStream, []byte(`{}`))
	assert.Equal(t, "0", rec.Header().Get(respond.HeaderFrameLimit))
	out, err := stream.Read(rec.Result(), nil)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, out.Status)

	file := exportState(t, e, ``)
	rec = call(e, http.MethodPost, "/api/state/import", state.MIMENDJSON, respond.MIMEEventStream, file)
	assert.Equal(t, strconv.Itoa(respond.FrameLimit), rec.Header().Get(respond.HeaderFrameLimit), "a route that declares nothing announces the default")
}

func TestStateExportOverTheStdioBridgePassesAFrameOver16MiB(t *testing.T) {
	m := seeded()
	m.add("location", "loc-big", map[string]any{"name": strings.Repeat("x", respond.FrameLimit+1), "region": "south"})
	e, operations := stateService(m)
	server := &mcp.Server{Name: "demo", Version: "v1", Operations: operations}
	server.Mount(e, func(next echo.HandlerFunc) echo.HandlerFunc { return next })
	backend := httptest.NewServer(e)
	defer backend.Close()
	remote := func(ctx context.Context) (string, string, error) { return backend.URL + "/mcp", "owner", nil }

	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"state_export","arguments":{"targets":[{"group":"location","keys":["loc-big"]}]},"_meta":{"progressToken":1}}}`
	var out bytes.Buffer
	assert.NoError(t, cli.Relay(context.Background(), "demo", remote, strings.NewReader(in), &out))
	var answer map[string]any
	progress := 0
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var message map[string]any
		assert.NoError(t, json.Unmarshal(line, &message))
		if message["id"] == float64(1) {
			answer = message
			continue
		}
		progress++
	}
	assert.Positive(t, progress, "the call streamed its steps, so the result came as one frame")
	require.NotNil(t, answer, "the result frame passed the bridge")
	called := answer["result"].(map[string]any)
	assert.Nil(t, called["isError"], fmt.Sprint(called["content"]))
	body, _ := json.Marshal(called["structuredContent"])
	assert.Greater(t, len(body), respond.FrameLimit)
	assert.Contains(t, string(body), "loc-big")
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
	assert.Equal(t, []string{"state:read", "state_export:read", "state_import:destructive", "state_import_session:write", "state_import_part:write", "state_import_commit:destructive", "state_import_session_delete:write"}, names)
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
		target := path
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
	assert.Equal(t, "location: 2 created\nroom: 1 created\nupsert dry run, nothing written: 3 created\n", errOut)
	assert.Equal(t, 0, target.writes)

	_, errOut, err = run("state", "import", file, "--mode", "create")
	assert.NoError(t, err)
	assert.Equal(t, "location: 2 created\nroom: 1 created\ncreate: 3 created\n", errOut)

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
	mcp.Route(e.Group("/api"), state.Register(state.Context{Service: "demo", Version: "v1.2.3", Keys: m}, nil, connectionGroup(m))...)
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
	assert.JSONEq(t, `{"group":"connection","key":"conn-1","id":"conn-1","data":{"name":"Calendar","type":"oauth","config":{"client_id":"public-id","region":"eu","mode":"push"},"auth":{"kind":"token"}}}`, lines[1], "exactly the flagged pointers go, the objects holding them stay")
	assert.NotContains(t, file, "s3cret")
	assert.NotContains(t, file, "t0ken")
}

func TestStateStripsByDiscriminator(t *testing.T) {
	e := connectionService(connections())
	file := string(exportState(t, e, ``))
	lines := strings.Split(strings.TrimSpace(file), "\n")
	assert.NotContains(t, lines[1], "r3fresh", "an oauth record loses its refresh token")
	assert.JSONEq(t, `{"group":"connection","key":"conn-2","id":"conn-2","data":{"name":"Printer","type":"basic","config":{"client_id":"printer","refresh_token":"listed","region":"us","mode":"pull"},"auth":{"kind":"user","user":"operator"}}}`, lines[2], "a basic record loses its password and keeps what only oauth hides")
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

type areaBody struct {
	Name     string `json:"name"`
	ParentID string `json:"parent_id,omitempty"`
}

func areaService(m *memory) *echo.Echo {
	area := memoryGroup[areaBody](m, "area")
	area.Schema = map[string]any{"type": "object", "properties": map[string]any{
		"name":      map[string]any{"type": "string"},
		"parent_id": map[string]any{"type": "string"},
	}}
	area.Refs = map[string]string{"parent_id": "area"}
	e, _ := stateService(m, area)
	return e
}

const areaFile = `{"format":"2","service":"demo","version":"v1.2.3","scope":[{"group":"area"}],"groups":[]}
{"group":"area","key":"a-lift","data":{"name":"Lift"},"refs":{"parent_id":"m-slope"}}
{"group":"area","key":"b-run","data":{"name":"Run"},"refs":{"parent_id":"m-slope"}}
{"group":"area","key":"m-slope","data":{"name":"Slope"},"refs":{"parent_id":"z-mountain"}}
{"group":"area","key":"z-mountain","data":{"name":"Mountain"}}
`

func TestStateImportPlacesAPointedLineFirst(t *testing.T) {
	m := newMemory("b")
	e := areaService(m)
	out := importState(t, e, "", []byte(areaFile))
	assert.Equal(t, map[string]int{state.OutcomeCreated: 4}, out.Counts, "a child named before its parent in the file still lands after it")
	ids := map[string]string{}
	for _, result := range out.Results {
		ids[result.Key] = result.ID
	}
	parents := map[string]any{}
	for _, row := range m.rows["area"] {
		parents[row.external] = row.data["parent_id"]
	}
	assert.Equal(t, ids["m-slope"], parents["a-lift"])
	assert.Equal(t, ids["z-mountain"], parents["m-slope"])
}

func TestStateImportSessionCommitsEveryPartInOneRun(t *testing.T) {
	m := newMemory("b")
	e := areaService(m)

	rec := call(e, http.MethodPost, "/api/state/import/session", echo.MIMEApplicationJSON, "", []byte(`{}`))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var started struct {
		Data state.Session `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &started))
	require.NotEmpty(t, started.Data.ID)
	path := "/api/state/import/session/" + started.Data.ID

	lines := strings.SplitAfter(areaFile, "\n")
	parts := []string{lines[0] + lines[1] + lines[2], lines[3], lines[4]}
	for i := len(parts) - 1; i >= 0; i-- {
		rec := call(e, http.MethodPut, path+"/part/"+strconv.Itoa(i+1), state.MIMENDJSON, "", []byte(parts[i]))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	assert.Equal(t, 0, m.count("area"), "nothing is written before the commit")

	rec = call(e, http.MethodPut, path+"/part/9", state.MIMENDJSON, "", []byte("not json\n"))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a part holds whole state lines")

	rec = call(e, http.MethodPost, path+"/commit", echo.MIMEApplicationJSON, "", []byte(`{"dry_run":true}`))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 0, m.count("area"), "a dry run commit writes nothing and keeps the session")

	rec = call(e, http.MethodPost, path+"/commit", echo.MIMEApplicationJSON, "", []byte(`{}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var committed struct {
		Data state.ImportResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &committed))
	assert.Equal(t, map[string]int{state.OutcomeCreated: 4}, committed.Data.Counts, "parts uploaded in any order commit as one file, parents first")
	assert.Equal(t, 4, m.count("area"))

	rec = call(e, http.MethodPost, path+"/commit", echo.MIMEApplicationJSON, "", []byte(`{}`))
	assert.Equal(t, http.StatusNotFound, rec.Code, "a committed session is gone")
}

func TestStateImportSessionRefusesAGap(t *testing.T) {
	e := areaService(newMemory("b"))
	rec := call(e, http.MethodPost, "/api/state/import/session", echo.MIMEApplicationJSON, "", nil)
	require.Equal(t, http.StatusCreated, rec.Code)
	var started struct {
		Data state.Session `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &started))
	path := "/api/state/import/session/" + started.Data.ID
	rec = call(e, http.MethodPut, path+"/part/2", state.MIMENDJSON, "", []byte(areaFile))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = call(e, http.MethodPost, path+"/commit", echo.MIMEApplicationJSON, "", nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	rec = call(e, http.MethodDelete, path, "", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestStateCommandStagesALargeFile(t *testing.T) {
	m := newMemory("b")
	e := areaService(m)
	var calls []string
	send := func(cmd *cobra.Command, method, path string, query url.Values, contentType string, body []byte) ([]byte, error) {
		calls = append(calls, method+" "+path)
		rec := call(e, method, path, contentType, "", body)
		if rec.Code >= http.StatusBadRequest {
			return nil, cli.Rejected(rec.Body.String(), "")
		}
		return rec.Body.Bytes(), nil
	}
	file := filepath.Join(t.TempDir(), "areas.ndjson")
	padding := strings.Repeat(" ", 600<<10)
	require.NoError(t, os.WriteFile(file, []byte(strings.ReplaceAll(areaFile, "\n{", padding+"\n{")), 0o600))

	root := &cobra.Command{Use: "demo"}
	root.AddCommand(cli.StateCommand(send, &cli.Output{JSON: true}))
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"state", "import", file, "--chunk", "1"})
	require.NoError(t, root.Execute(), stderr.String())

	assert.Equal(t, 4, m.count("area"))
	assert.Equal(t, "POST /api/state/import/session", calls[0], "a file over the chunk size opens a session")
	assert.True(t, strings.HasPrefix(calls[1], "PUT /api/state/import/session/") && strings.HasSuffix(calls[1], "/part/1"), calls[1])
	assert.True(t, strings.HasSuffix(calls[len(calls)-1], "/commit"), "and commits it")
	assert.Greater(t, len(calls), 3, "with more than one part")
	assert.Contains(t, stderr.String(), "area: 4 created\n")
}

func TestStateHeadersReachTheAccessOverTheRelay(t *testing.T) {
	m := seeded()
	session := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(ctx echo.Context) error {
			if ctx.Request().Header.Get("SAuth-App-Slug") == "" {
				return ctx.NoContent(http.StatusUnauthorized)
			}
			return next(ctx)
		}
	}
	e := echo.New()
	operations := state.Register(state.Context{Service: "demo", Version: "v1.2.3", Keys: m, Headers: []mcp.Header{{Name: "app_slug", Param: "SAuth-App-Slug", Usage: "the app the session belongs to"}}}, []echo.MiddlewareFunc{session}, memoryGroup[locationBody](m, "location"))
	mcp.Route(e.Group("/api"), operations...)
	server := &mcp.Server{Name: "demo", Version: "v1", Operations: operations}
	server.Mount(e)
	backend := httptest.NewServer(e)
	defer backend.Close()
	remote := func(ctx context.Context) (string, string, error) { return backend.URL + "/mcp", "owner", nil }

	for _, operation := range operations {
		property := operation.InputSchema["properties"].(map[string]any)["app_slug"]
		assert.Equal(t, map[string]any{"type": "string", "description": "the app the session belongs to"}, property, operation.Name)
		required, _ := operation.InputSchema["required"].([]string)
		assert.NotContains(t, required, "app_slug", operation.Name)
	}
	_, plain := stateService(seeded())
	assert.NotContains(t, plain[1].InputSchema["properties"], "app_slug")

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"state_export","arguments":{"app_slug":"portal","targets":[{"group":"location"}]}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"state_export","arguments":{"targets":[{"group":"location"}]}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"state","arguments":{"app_slug":"portal"}}}`,
	}, "\n")
	var out bytes.Buffer
	assert.NoError(t, cli.Relay(context.Background(), "demo", remote, strings.NewReader(in), &out))
	byID := map[float64]map[string]any{}
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var message map[string]any
		assert.NoError(t, json.Unmarshal(line, &message))
		if id, ok := message["id"].(float64); ok {
			byID[id] = message["result"].(map[string]any)
		}
	}
	assert.Nil(t, byID[1]["isError"], fmt.Sprint(byID[1]["content"]))
	exported, _ := json.Marshal(byID[1]["structuredContent"])
	assert.Contains(t, string(exported), "Head office")
	assert.Equal(t, true, byID[2]["isError"])
	assert.Equal(t, http.StatusText(http.StatusUnauthorized), byID[2]["content"].([]any)[0].(map[string]any)["text"])
	assert.Nil(t, byID[3]["isError"])
}

func TestStateUnderAPIRunsTheServiceChain(t *testing.T) {
	m := seeded()
	claims := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(ctx echo.Context) error {
			if ctx.Request().Header.Get("X-Claim") != "state" {
				return respond.Error(ctx, http.StatusForbidden, "the caller holds no state claim")
			}
			return next(ctx)
		}
	}
	e := echo.New()
	server := &mcp.Server{Name: "demo", Version: "v1"}
	require.NoError(t, endpoint.Mount(e, endpoint.Set{Service: "demo", Version: "v1.2.3", MCP: server, APIMiddleware: []echo.MiddlewareFunc{claims}, State: &state.Context{Keys: m}, Groups: []state.Definition{memoryGroup[locationBody](m, "location")}}))
	send := func(method, target, contentType, claim string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, bytes.NewReader(body))
		req.Header.Set(echo.HeaderAuthorization, "Bearer anyone")
		if contentType != "" {
			req.Header.Set(echo.HeaderContentType, contentType)
		}
		if claim != "" {
			req.Header.Set("X-Claim", claim)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	exported := send(http.MethodPost, "/api/state/export", echo.MIMEApplicationJSON, "state", []byte(`{}`))
	require.Equal(t, http.StatusOK, exported.Code, exported.Body.String())
	assert.Contains(t, exported.Body.String(), "Head office")
	for _, route := range []struct {
		method, target, contentType string
		body                        []byte
	}{
		{http.MethodGet, "/api/state", "", nil},
		{http.MethodPost, "/api/state/export", echo.MIMEApplicationJSON, []byte(`{}`)},
		{http.MethodPost, "/api/state/import?dry_run=true", state.MIMENDJSON, exported.Body.Bytes()},
	} {
		refused := send(route.method, route.target, route.contentType, "", route.body)
		assert.Equal(t, http.StatusForbidden, refused.Code, route.target)
		assert.Contains(t, refused.Body.String(), "the caller holds no state claim", route.target)
		served := send(route.method, route.target, route.contentType, "state", route.body)
		assert.Equal(t, http.StatusOK, served.Code, route.target+": "+served.Body.String())
	}
	assert.Equal(t, 0, m.writes, "the dry run wrote nothing")

	for _, gone := range []string{"/state", "/state/export", "/state/import"} {
		assert.Equal(t, http.StatusNotFound, send(http.MethodGet, gone, "", "state", nil).Code, gone)
	}
	status := send(http.MethodGet, "/status", "", "", nil)
	var listed struct {
		Data endpoint.Status `json:"data"`
	}
	require.NoError(t, json.Unmarshal(status.Body.Bytes(), &listed))
	assert.Equal(t, []string{"/api", "/status", "/api/state", "/mcp"}, listed.Data.Endpoints)

	tool := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"state","arguments":{}}}`)
	assert.Equal(t, true, result(t, send(http.MethodPost, "/mcp", echo.MIMEApplicationJSON, "", tool))["isError"], "the tool runs the same chain")
	assert.Nil(t, result(t, send(http.MethodPost, "/mcp", echo.MIMEApplicationJSON, "state", tool))["isError"])
}

func TestStateImportTakesANewIDWhenTheLineIDIsTaken(t *testing.T) {
	seed := seeded()
	seed.rows["location"][0].external = "hq"
	source, _ := stateService(seed)
	file := exportState(t, source, `{"targets":[{"group":"location"}]}`)

	target := newMemory("b")
	target.add("location", "loc-1", map[string]any{"name": "Elsewhere", "region": "south"}).external = "other"
	e, _ := stateService(target)
	out := importState(t, e, "", file)
	ids := map[string]string{}
	for _, result := range out.Results {
		ids[result.Key] = result.ID
	}
	assert.Equal(t, "b-1", ids["hq"], "the id is taken by another record, so the created one takes a new id")
	assert.Equal(t, "loc-2", ids["loc-2"], "a free id is kept")
}

func TestStateReadsALineWithoutID(t *testing.T) {
	file := "{\"format\":\"" + state.FormatVersion + "\",\"service\":\"demo\",\"version\":\"1\",\"scope\":[],\"groups\":[]}\n{\"group\":\"location\",\"key\":\"loc-9\",\"data\":{\"name\":\"Old\"}}\n"
	e, _ := stateService(newMemory("b"))
	out := importState(t, e, "", []byte(file))
	assert.Equal(t, map[string]int{state.OutcomeCreated: 1}, out.Counts)
	assert.Equal(t, "b-1", out.Results[0].ID, "a line from before ids were exported creates under a new id, as before")
}
