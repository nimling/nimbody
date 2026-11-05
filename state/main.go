package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody"
	"github.com/nimling/nimbody/mcp"
	"github.com/nimling/nimbody/respond"
)

const FormatVersion = "2"

const MIMENDJSON = "application/x-ndjson"

const (
	ModeUpsert = "upsert"
	ModeCreate = "create"
	ModeSync   = "sync"
)

const (
	OutcomeCreated   = "created"
	OutcomeUpdated   = "updated"
	OutcomeUnchanged = "unchanged"
	OutcomeDeleted   = "deleted"
	OutcomeRefused   = "refused"
)

const (
	FlagWriteOnly     = "writeOnly"
	FlagSensitive     = "x-sensitive"
	FlagImmutable     = "x-immutable"
	FlagServerDefault = "x-server-default"
)

type Resolver interface {
	GetID(ctx context.Context, group, key string) (string, error)
	GetKey(ctx context.Context, group, id string) (string, error)
}

type Context struct {
	echo.Context
	Service     string
	Version     string
	Access      echo.MiddlewareFunc
	Headers     []mcp.Header
	Transaction func(ctx context.Context, run func(ctx context.Context) error) error
	Keys        Resolver
	PostAsset   func(ctx echo.Context, name string, body []byte) (string, error)
	GetAsset    func(ctx echo.Context, assetID string) ([]byte, error)
	Progress    func(ctx echo.Context, result Result)
	Steps       *respond.Steps
}

type Query struct {
	Filter  map[string]string
	Keys    []string
	History bool
}

type Record[T any] struct {
	ID   string
	Key  string
	Data T
	Body *nimbody.BodyValidator
}

type Blobs[T any] struct {
	Get func(ctx *Context, record Record[T]) ([]Blob, error)
	Put func(ctx *Context, record Record[T], blob Blob) error
}

type Group[T any] struct {
	Name          string
	Schema        map[string]any
	DependsOn     []string
	Refs          map[string]string
	Filters       []string
	Discriminator string
	Sensitive     func() map[string][]string
	Export        func(ctx *Context, query Query) ([]Record[T], error)
	Import        func(ctx *Context, record Record[T]) (string, error)
	Delete        func(ctx *Context, id string) error
	Blobs         Blobs[T]
}

type Definition interface {
	definition() group
}

type Target struct {
	Group  string            `json:"group"`
	Filter map[string]string `json:"filter,omitempty"`
	Keys   []string          `json:"keys,omitempty"`
}

type Fields struct {
	Sensitive     []string            `json:"sensitive"`
	Immutable     []string            `json:"immutable"`
	ServerDefault []string            `json:"server_default"`
	Discriminator string              `json:"discriminator,omitempty"`
	SensitiveBy   map[string][]string `json:"sensitive_by"`
}

type GroupSchema struct {
	Name      string            `json:"name"`
	Schema    map[string]any    `json:"schema"`
	DependsOn []string          `json:"depends_on"`
	Refs      map[string]string `json:"refs"`
	Filters   []string          `json:"filters"`
	Fields    Fields            `json:"fields"`
}

type Document struct {
	Format  string        `json:"format"`
	Service string        `json:"service"`
	Version string        `json:"version"`
	Groups  []GroupSchema `json:"groups"`
}

type ExportRequest struct {
	Targets []Target `json:"targets,omitempty" jsonschema:"groups to export, each with a filter and keys; none means every group the caller reaches"`
	History bool     `json:"history,omitempty" jsonschema:"include the history of each record"`
	Blobs   bool     `json:"blobs,omitempty" jsonschema:"inline the blobs of each record"`
	Store   bool     `json:"store,omitempty" jsonschema:"save the export as an asset and answer its id"`
}

type Stored struct {
	AssetID  string   `json:"asset_id"`
	Manifest Manifest `json:"manifest"`
}

type ImportRequest struct {
	AssetID string              `json:"asset_id,omitempty" jsonschema:"the stored export to import"`
	Mode    string              `json:"mode,omitempty" jsonschema:"upsert (default), create or sync; sync deletes what the targets reach and the input lacks"`
	DryRun  bool                `json:"dry_run,omitempty" jsonschema:"list every outcome and write nothing"`
	Delete  map[string][]string `json:"delete,omitempty" jsonschema:"keys to delete, per group"`
	Targets []Target            `json:"targets,omitempty" jsonschema:"the groups to import, and the scope a sync prunes"`
}

type Result struct {
	Key     string `json:"key"`
	Group   string `json:"group"`
	ID      string `json:"id,omitempty"`
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

type ImportResponse struct {
	Mode    string         `json:"mode"`
	DryRun  bool           `json:"dry_run"`
	Counts  map[string]int `json:"counts"`
	Results []Result       `json:"results"`
}

type exported struct {
	id   string
	key  string
	data map[string]any
	put  func(ctx *Context, blob Blob) error
	get  func(ctx *Context) ([]Blob, error)
}

type group struct {
	name      string
	schema    map[string]any
	dependsOn []string
	refs      map[string]string
	filters   []string
	fields    Fields
	sensitive func() map[string][]string
	blobs     bool
	export    func(ctx *Context, query Query) ([]exported, error)
	write     func(ctx *Context, id, key string, data map[string]any) (string, func(ctx *Context, blob Blob) error, error)
	delete    func(ctx *Context, id string) error
}

func (g Group[T]) definition() group {
	if g.Discriminator != "" {
		if _, err := ParsePointer(g.Discriminator); err != nil {
			panic("state: group " + g.Name + " names discriminator " + g.Discriminator + ", which is not a field pointer")
		}
	}
	if g.Sensitive != nil && g.Discriminator == "" {
		panic("state: group " + g.Name + " declares sensitive fields per type without a discriminator")
	}
	defined := group{
		name:      g.Name,
		schema:    g.Schema,
		dependsOn: g.DependsOn,
		refs:      g.Refs,
		filters:   g.Filters,
		fields:    fieldsOf(g.Schema),
		sensitive: g.Sensitive,
		blobs:     g.Blobs.Get != nil,
		delete:    g.Delete,
	}
	defined.fields.Discriminator = g.Discriminator
	put := func(record Record[T]) func(ctx *Context, blob Blob) error {
		if g.Blobs.Put == nil {
			return nil
		}
		return func(ctx *Context, blob Blob) error { return g.Blobs.Put(ctx, record, blob) }
	}
	defined.export = func(ctx *Context, query Query) ([]exported, error) {
		records, err := g.Export(ctx, query)
		if err != nil {
			return nil, err
		}
		out := make([]exported, 0, len(records))
		for _, record := range records {
			data, err := decode(record.Data)
			if err != nil {
				return nil, err
			}
			key := record.Key
			if key == "" {
				key = record.ID
			}
			row := exported{id: record.ID, key: key, data: data, put: put(record)}
			if g.Blobs.Get != nil {
				row.get = func(ctx *Context) ([]Blob, error) { return g.Blobs.Get(ctx, record) }
			}
			out = append(out, row)
		}
		return out, nil
	}
	defined.write = func(ctx *Context, id, key string, data map[string]any) (string, func(ctx *Context, blob Blob) error, error) {
		body, err := json.Marshal(data)
		if err != nil {
			return "", nil, err
		}
		record := Record[T]{ID: id, Key: key, Body: &nimbody.BodyValidator{Body: nimbody.JSONBody(data)}}
		if err := json.Unmarshal(body, &record.Data); err != nil {
			return "", nil, echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		written, err := g.Import(ctx, record)
		if err != nil {
			return "", nil, err
		}
		record.ID = written
		return written, put(record), nil
	}
	return defined
}

func decode(value any) (map[string]any, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func fieldsOf(schema map[string]any) Fields {
	flags := map[string]map[string]bool{FlagSensitive: {}, FlagImmutable: {}, FlagServerDefault: {}}
	walkFields(schema, "", flags)
	return Fields{
		Sensitive:     sorted(flags[FlagSensitive]),
		Immutable:     sorted(flags[FlagImmutable]),
		ServerDefault: sorted(flags[FlagServerDefault]),
		SensitiveBy:   map[string][]string{},
	}
}

func walkFields(schema map[string]any, pointer string, flags map[string]map[string]bool) {
	for name, raw := range GetProperties(schema) {
		property, _ := raw.(map[string]any)
		at := JoinPointer(pointer, name)
		if property[FlagWriteOnly] == true || property[FlagSensitive] == true {
			flags[FlagSensitive][at] = true
		}
		if property[FlagImmutable] == true {
			flags[FlagImmutable][at] = true
		}
		if property[FlagServerDefault] == true {
			flags[FlagServerDefault][at] = true
		}
		walkFields(property, at, flags)
	}
}

func (g group) resolved() group {
	by := map[string][]string{}
	if g.sensitive != nil {
		for value, pointers := range g.sensitive() {
			set := map[string]bool{}
			for _, pointer := range pointers {
				set[pointer] = true
			}
			by[value] = sorted(set)
		}
	}
	g.fields.SensitiveBy = by
	return g
}

func (g group) secrets(data map[string]any) []string {
	return append(append([]string{}, g.fields.Sensitive...), g.fields.SensitiveBy[discriminated(data, g.fields.Discriminator)]...)
}

type service struct {
	host   Context
	groups []group
	byName map[string]group
}

func Register(router *echo.Group, host Context, definitions ...Definition) []mcp.Operation {
	s := &service{host: host, byName: map[string]group{}}
	var declared []group
	for _, definition := range definitions {
		defined := definition.definition()
		if _, taken := s.byName[defined.name]; taken {
			panic("state: two groups are called " + defined.name)
		}
		s.byName[defined.name] = defined
		declared = append(declared, defined)
	}
	s.groups = ordered(declared, s.byName)

	var middleware []echo.MiddlewareFunc
	if host.Access != nil {
		middleware = append(middleware, host.Access)
	}
	router.GET("/state", s.getDocument, middleware...)
	export := append([]echo.MiddlewareFunc{respond.FrameLimitMiddleware(0)}, middleware...)
	router.POST("/state/export", s.postExport, export...)
	router.POST("/state/import", s.postImport, middleware...)

	return []mcp.Operation{
		mcp.Define(mcp.Spec{Name: "state", Description: "The state schema: every group with its create body schema, dependencies, references, filters, field flags and the format version", Method: http.MethodGet, Path: "/state", Access: mcp.AccessRead, Headers: host.Headers, Middleware: middleware},
			func(ctx echo.Context, in struct{}) (Document, error) {
				return s.document(), nil
			}),
		mcp.Define(mcp.Spec{Name: "state_export", Description: "Export records by group, filter and keys as state lines, or store the export as an asset and answer its id", Method: http.MethodPost, Path: "/state/export", Access: mcp.AccessRead, Headers: host.Headers, Middleware: export},
			func(ctx echo.Context, in ExportRequest) (any, error) {
				file, err := s.export(s.context(ctx), in)
				if err != nil {
					return nil, err
				}
				if in.Store {
					return s.store(ctx, file)
				}
				return file.Lines()
			}),
		mcp.Define(mcp.Spec{Name: "state_import", Description: "Import a stored export by asset id: upsert, create or sync, with deletes by key and a dry run that lists every outcome", Method: http.MethodPost, Path: "/state/import", Access: mcp.AccessDestructive, Headers: host.Headers, Middleware: middleware},
			func(ctx echo.Context, in ImportRequest) (ImportResponse, error) {
				if in.AssetID == "" {
					return ImportResponse{}, echo.NewHTTPError(http.StatusBadRequest, "an import over mcp takes asset_id")
				}
				file, err := s.load(ctx, in.AssetID)
				if err != nil {
					return ImportResponse{}, err
				}
				return s.importFile(s.context(ctx), in, file)
			}),
	}
}

func ordered(declared []group, byName map[string]group) []group {
	var out []group
	state := map[string]int{}
	var visit func(g group, path []string)
	visit = func(g group, path []string) {
		switch state[g.name] {
		case 1:
			panic("state: groups depend on each other in a cycle: " + strings.Join(append(path, g.name), " -> "))
		case 2:
			return
		}
		state[g.name] = 1
		for _, name := range g.dependsOn {
			dependency, ok := byName[name]
			if !ok {
				panic("state: group " + g.name + " depends on " + name + ", which is not registered")
			}
			visit(dependency, append(path, g.name))
		}
		state[g.name] = 2
		out = append(out, g)
	}
	for _, g := range declared {
		visit(g, nil)
	}
	return out
}

func (s *service) context(ctx echo.Context) *Context {
	c := s.host
	c.Context = ctx
	c.Steps = respond.GetSteps(ctx)
	return &c
}

func (s *service) document() Document {
	doc := Document{Format: FormatVersion, Service: s.host.Service, Version: s.host.Version, Groups: make([]GroupSchema, 0, len(s.groups))}
	for _, g := range s.groups {
		entry := GroupSchema{Name: g.name, Schema: g.schema, DependsOn: g.dependsOn, Refs: g.refs, Filters: g.filters, Fields: g.resolved().fields}
		if entry.Schema == nil {
			entry.Schema = map[string]any{}
		}
		if entry.DependsOn == nil {
			entry.DependsOn = []string{}
		}
		if entry.Refs == nil {
			entry.Refs = map[string]string{}
		}
		if entry.Filters == nil {
			entry.Filters = []string{}
		}
		doc.Groups = append(doc.Groups, entry)
	}
	return doc
}

func (s *service) getDocument(ctx echo.Context) error {
	return respond.JSON(ctx, http.StatusOK, s.document(), 1)
}

func (s *service) postExport(ctx echo.Context) error {
	c := s.context(ctx)
	var in ExportRequest
	if err := json.NewDecoder(ctx.Request().Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		return respond.Error(ctx, http.StatusBadRequest, "the export body is not json: "+err.Error())
	}
	file, err := s.export(c, in)
	if err != nil {
		return failure(ctx, "state export", err)
	}
	if in.Store {
		stored, err := s.store(ctx, file)
		if err != nil {
			return failure(ctx, "state export", err)
		}
		return respond.JSON(ctx, http.StatusCreated, stored, 1)
	}
	if respond.Streaming(ctx) {
		lines, err := file.Lines()
		if err != nil {
			return failure(ctx, "state export", err)
		}
		return respond.JSON(ctx, http.StatusOK, lines, len(lines))
	}
	res := ctx.Response()
	res.Header().Set(echo.HeaderContentType, MIMENDJSON)
	res.WriteHeader(http.StatusOK)
	return file.Write(res)
}

func (s *service) postImport(ctx echo.Context) error {
	c := s.context(ctx)
	in, file, err := s.importRequest(ctx)
	if err != nil {
		return failure(ctx, "state import", err)
	}
	out, err := s.importFile(c, in, file)
	if err != nil {
		return failure(ctx, "state import", err)
	}
	return respond.JSON(ctx, http.StatusOK, out, len(out.Results))
}

func (s *service) importRequest(ctx echo.Context) (ImportRequest, File, error) {
	req := ctx.Request()
	var in ImportRequest
	if strings.HasPrefix(req.Header.Get(echo.HeaderContentType), MIMENDJSON) {
		values := ctx.QueryParams()
		in.Mode = values.Get("mode")
		in.DryRun = values.Get("dry_run") == "true"
		for _, raw := range values["delete"] {
			name, key, err := ParseKey(raw)
			if err != nil {
				return in, File{}, err
			}
			if in.Delete == nil {
				in.Delete = map[string][]string{}
			}
			in.Delete[name] = append(in.Delete[name], key)
		}
		for _, raw := range values["target"] {
			target, err := ParseTarget(raw)
			if err != nil {
				return in, File{}, err
			}
			in.Targets = append(in.Targets, target)
		}
		file, err := Read(req.Body)
		if err != nil {
			return in, File{}, echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return in, file, nil
	}
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		return in, File{}, echo.NewHTTPError(http.StatusBadRequest, "the import body is neither state lines nor json: "+err.Error())
	}
	if in.AssetID == "" {
		return in, File{}, echo.NewHTTPError(http.StatusBadRequest, "an import takes state lines as "+MIMENDJSON+" or a json body with asset_id")
	}
	file, err := s.load(ctx, in.AssetID)
	return in, file, err
}

func (s *service) store(ctx echo.Context, file File) (Stored, error) {
	if s.host.PostAsset == nil {
		return Stored{}, echo.NewHTTPError(http.StatusBadRequest, "this service does not store exports")
	}
	var body bytes.Buffer
	if err := file.Write(&body); err != nil {
		return Stored{}, err
	}
	id, err := s.host.PostAsset(ctx, s.host.Service+".state.ndjson", body.Bytes())
	if err != nil {
		return Stored{}, err
	}
	return Stored{AssetID: id, Manifest: file.Manifest}, nil
}

func (s *service) load(ctx echo.Context, assetID string) (File, error) {
	if s.host.GetAsset == nil {
		return File{}, echo.NewHTTPError(http.StatusBadRequest, "this service does not store exports")
	}
	body, err := s.host.GetAsset(ctx, assetID)
	if err != nil {
		return File{}, err
	}
	file, err := Read(bytes.NewReader(body))
	if err != nil {
		return File{}, echo.NewHTTPError(http.StatusBadRequest, "asset "+assetID+": "+err.Error())
	}
	return file, nil
}

func failure(ctx echo.Context, name string, err error) error {
	var rejected *echo.HTTPError
	if errors.As(err, &rejected) {
		return respond.Error(ctx, rejected.Code, message(rejected))
	}
	ctx.Logger().Error(name + ": " + err.Error())
	return respond.ErrorInternal(ctx)
}

func message(err error) string {
	var rejected *echo.HTTPError
	if errors.As(err, &rejected) {
		if text, ok := rejected.Message.(string); ok {
			return text
		}
	}
	return err.Error()
}
