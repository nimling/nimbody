package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody"
	"github.com/nimling/nimbody/cli"
	"github.com/nimling/nimbody/endpoint"
	"github.com/nimling/nimbody/mcp"
	"github.com/nimling/nimbody/respond"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

type articleInput struct {
	ArticleID string `param:"article_id" json:"article_id" jsonschema:"the article to read"`
	Verbose   bool   `query:"verbose" json:"verbose,omitempty"`
}

type article struct {
	ID     string  `json:"id"`
	Title  string  `json:"title"`
	Reader string  `json:"reader"`
	Lead   *string `json:"lead"`
}

type articleCreate struct {
	Title string `json:"title"`
	Draft bool   `json:"draft,omitempty"`
}

var remoteSeen struct {
	sync.Mutex
	accept []string
}

func seeAccept(ctx echo.Context) {
	remoteSeen.Lock()
	defer remoteSeen.Unlock()
	remoteSeen.accept = append(remoteSeen.accept, ctx.Request().Method+" "+ctx.Request().Header.Get(echo.HeaderAccept))
}

func remoteService() (*echo.Echo, *mcp.Server) {
	auth := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(ctx echo.Context) error {
			if ctx.Request().Header.Get(echo.HeaderAuthorization) != "Bearer good" {
				return ctx.String(http.StatusUnauthorized, "Unauthorized")
			}
			ctx.Set("user", "reader-one")
			return next(ctx)
		}
	}
	owner := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(ctx echo.Context) error {
			if ctx.Param("article_id") == "locked" {
				return respond.Error(ctx, http.StatusForbidden, "no claim reaches article locked")
			}
			return next(ctx)
		}
	}
	get := mcp.Define(mcp.Spec{Name: "article_get", Description: "Read one article", Method: http.MethodGet, Path: "/articles/:article_id", Access: mcp.AccessRead, Middleware: []echo.MiddlewareFunc{owner}},
		func(ctx echo.Context, in articleInput) (article, error) {
			seeAccept(ctx)
			if in.ArticleID == "gone" {
				return article{}, echo.NewHTTPError(http.StatusNotFound, "no article is gone")
			}
			return article{ID: in.ArticleID, Title: "Orion", Reader: ctx.Get("user").(string)}, nil
		})
	list := mcp.Define(mcp.Spec{Name: "article_list", Description: "Read articles", Method: http.MethodGet, Path: "/articles", Access: mcp.AccessRead},
		func(ctx echo.Context, in struct{}) (mcp.List[article], error) {
			return mcp.List[article]{Rows: []article{{ID: "a", Title: "Orion"}, {ID: "b", Title: "Atlas"}}, Total: 9}, nil
		})
	create := mcp.Define(mcp.Spec{Name: "article_create", Description: "Add an article", Method: http.MethodPost, Path: "/articles", Access: mcp.AccessWrite},
		func(ctx echo.Context, in articleCreate) (article, error) {
			return article{ID: "new", Title: in.Title}, nil
		})
	publish := mcp.Define(mcp.Spec{Name: "article_publish", Description: "Publish an article", Method: http.MethodPost, Path: "/articles/:article_id/publish", Status: http.StatusOK, Access: mcp.AccessWrite},
		func(ctx echo.Context, in articleInput) (article, error) {
			seeAccept(ctx)
			steps := respond.GetSteps(ctx)
			steps.Start("publish", "publish the article", "")
			steps.Done("publish", nil)
			return article{ID: in.ArticleID, Title: "Orion", Reader: ctx.Get("user").(string)}, nil
		})
	operations := []mcp.Operation{get, list, create, publish}
	e := echo.New()
	mcp.Route(e.Group("/api", auth), operations...)
	server := &mcp.Server{Name: "demo", Version: "v1", Operations: operations, AuthorizationServer: "https://auth.example.test"}
	server.Mount(e, auth)
	return e, server
}

func post(e *echo.Echo, token string, accept string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.Header.Set(echo.HeaderAccept, accept)
	if token != "" {
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func result(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	var reply map[string]any
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reply), rec.Body.String())
	return reply["result"].(map[string]any)
}

func TestRemoteProtectedResourceMetadata(t *testing.T) {
	e, _ := remoteService()
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusOK, rec.Code)
		var metadata mcp.ResourceMetadata
		assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &metadata))
		assert.Equal(t, "http://example.com/mcp", metadata.Resource)
		assert.Equal(t, []string{"https://auth.example.test"}, metadata.AuthorizationServers)
	}
}

func TestRemoteChallengesWithoutAValidToken(t *testing.T) {
	e, _ := remoteService()
	for _, token := range []string{"", "bad"} {
		rec := post(e, token, "application/json, text/event-stream", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Equal(t, `Bearer resource_metadata="http://example.com/.well-known/oauth-protected-resource/mcp"`, rec.Header().Get(echo.HeaderWWWAuthenticate))
	}
}

func TestRemoteListsOperationsAsTools(t *testing.T) {
	e, _ := remoteService()
	rec := post(e, "good", "application/json, text/event-stream", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := map[string]map[string]any{}
	for _, tool := range result(t, rec)["tools"].([]any) {
		tools[tool.(map[string]any)["name"].(string)] = tool.(map[string]any)
	}
	assert.Len(t, tools, 4, "no run page and no steps tool over http")
	get := tools["article_get"]
	assert.Equal(t, true, get["annotations"].(map[string]any)["readOnlyHint"])
	schema := get["inputSchema"].(map[string]any)
	assert.Equal(t, []any{"article_id"}, schema["required"])
	properties := schema["properties"].(map[string]any)
	assert.Equal(t, "the article to read", properties["article_id"].(map[string]any)["description"])
	for _, name := range []string{"verbose", "fields", "sort", "jq", "template", "limit", "cursor"} {
		assert.Contains(t, properties, name)
	}
	output := get["outputSchema"].(map[string]any)["properties"].(map[string]any)["data"].(map[string]any)
	assert.Equal(t, []any{"string", "null"}, output["properties"].(map[string]any)["lead"].(map[string]any)["type"])
	assert.Contains(t, tools["article_create"]["description"], "confirm with the user")
	assert.NotContains(t, tools["article_create"]["inputSchema"].(map[string]any)["properties"], "cursor")
	list := tools["article_list"]["outputSchema"].(map[string]any)["properties"].(map[string]any)["data"].(map[string]any)
	assert.Equal(t, []any{"array", "null"}, list["type"])
}

func TestRemoteCallRunsTheServiceAsTheCaller(t *testing.T) {
	e, _ := remoteService()
	rec := post(e, "good", "application/json", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"article_get","arguments":{"article_id":"42","fields":["id","reader"]}}}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	called := result(t, rec)
	assert.Nil(t, called["isError"])
	assert.Equal(t, map[string]any{"id": "42", "title": "Orion", "reader": "reader-one", "lead": nil}, called["structuredContent"].(map[string]any)["data"])
	assert.JSONEq(t, `{"id":"42","reader":"reader-one"}`, called["content"].([]any)[0].(map[string]any)["text"].(string))

	rec = post(e, "good", "application/json", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"article_get","arguments":{"article_id":"locked"}}}`)
	called = result(t, rec)
	assert.Equal(t, true, called["isError"])
	assert.Equal(t, "no claim reaches article locked", called["content"].([]any)[0].(map[string]any)["text"])

	rec = post(e, "good", "application/json", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"article_get","arguments":{"article_id":"gone"}}}`)
	called = result(t, rec)
	assert.Equal(t, true, called["isError"])
	assert.Equal(t, "no article is gone", called["content"].([]any)[0].(map[string]any)["text"])

	rec = post(e, "good", "application/json", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"article_get","arguments":{"article_id":"1","template":"@/etc/passwd"}}}`)
	assert.Equal(t, true, result(t, rec)["isError"], "a template file is never read on the server")
}

func TestRemoteCallHintsWhenMoreRowsExist(t *testing.T) {
	e, _ := remoteService()
	rec := post(e, "good", "application/json", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"article_list","arguments":{"jq":".[].title"}}}`)
	text := result(t, rec)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Equal(t, "Orion\nAtlas\n2 of 9 rows, or narrow with fields or limit\n", text)
}

func TestRemoteCallStreamsProgress(t *testing.T) {
	e, _ := remoteService()
	rec := post(e, "good", "application/json, text/event-stream", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"article_publish","arguments":{"article_id":"42"},"_meta":{"progressToken":"p1"}}}`)
	assert.Equal(t, respond.MIMEEventStream, rec.Header().Get(echo.HeaderContentType))
	var messages []map[string]any
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var message map[string]any
			assert.NoError(t, json.Unmarshal([]byte(data), &message))
			messages = append(messages, message)
		}
	}
	assert.Len(t, messages, 3)
	assert.Equal(t, "notifications/progress", messages[0]["method"])
	assert.Equal(t, "p1", messages[0]["params"].(map[string]any)["progressToken"])
	assert.Equal(t, float64(7), messages[2]["id"])
}

func TestRemoteGetToolReadsOnce(t *testing.T) {
	e, _ := remoteService()
	remoteSeen.Lock()
	remoteSeen.accept = nil
	remoteSeen.Unlock()
	rec := post(e, "good", "application/json, text/event-stream", `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"article_get","arguments":{"article_id":"42"},"_meta":{"progressToken":"p2"}}}`)
	assert.Equal(t, echo.MIMEApplicationJSON, rec.Header().Get(echo.HeaderContentType), "a read answers once, never as a stream")
	called := result(t, rec)
	assert.Nil(t, called["isError"])
	assert.Equal(t, "42", called["structuredContent"].(map[string]any)["data"].(map[string]any)["id"])

	rec = post(e, "good", "application/json", `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"article_get","arguments":{"article_id":"locked"}}}`)
	assert.Equal(t, "no claim reaches article locked", result(t, rec)["content"].([]any)[0].(map[string]any)["text"], "a middleware refusal of a read keeps its message")

	post(e, "good", "application/json", `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"article_publish","arguments":{"article_id":"42"}}}`)
	remoteSeen.Lock()
	defer remoteSeen.Unlock()
	assert.Equal(t, []string{"GET application/json", "POST text/event-stream"}, remoteSeen.accept, "a GET tool reaches the service with Accept: application/json, a write keeps the step stream")
}

func TestRemoteNotificationIsAccepted(t *testing.T) {
	e, _ := remoteService()
	rec := post(e, "good", "application/json", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	assert.Equal(t, http.StatusAccepted, rec.Code)
}

func TestRemoteRouteIsTheSameOperation(t *testing.T) {
	e, _ := remoteService()
	req := httptest.NewRequest(http.MethodGet, "/api/articles/42", nil)
	req.Header.Set(echo.HeaderAuthorization, "Bearer good")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"reader": "reader-one"`)

	req = httptest.NewRequest(http.MethodGet, "/api/articles/locked", nil)
	req.Header.Set(echo.HeaderAuthorization, "Bearer good")
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	req = httptest.NewRequest(http.MethodPost, "/api/articles", strings.NewReader(`{"title":"Atlas"}`))
	req.Header.Set(echo.HeaderAuthorization, "Bearer good")
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Contains(t, rec.Body.String(), `"title": "Atlas"`)
}

func TestRemoteBridgeRelaysStdio(t *testing.T) {
	e, _ := remoteService()
	backend := httptest.NewServer(e)
	defer backend.Close()
	token := "good"
	remote := func(ctx context.Context) (string, string, error) { return backend.URL + "/mcp", token, nil }
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"article_publish","arguments":{"article_id":"9"},"_meta":{"progressToken":1}}}`,
	}, "\n")
	var out bytes.Buffer
	assert.NoError(t, cli.Relay(context.Background(), "demo", remote, strings.NewReader(in), &out))
	byID := map[float64]map[string]any{}
	progress := 0
	lines := bufio.NewScanner(&out)
	for lines.Scan() {
		var message map[string]any
		assert.NoError(t, json.Unmarshal(lines.Bytes(), &message), lines.Text())
		if id, ok := message["id"].(float64); ok {
			byID[id] = message
			continue
		}
		progress++
	}
	assert.Equal(t, "2026-07-28", byID[1]["result"].(map[string]any)["protocolVersion"])
	assert.Equal(t, "9", byID[2]["result"].(map[string]any)["structuredContent"].(map[string]any)["data"].(map[string]any)["id"])
	assert.Equal(t, 2, progress)

	token = "bad"
	out.Reset()
	assert.NoError(t, cli.Relay(context.Background(), "demo", remote, strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`), &out))
	assert.Contains(t, out.String(), "run 'demo login'")
}

func TestOperationCommandMapsTheRoute(t *testing.T) {
	_, server := remoteService()
	var method, path string
	var query url.Values
	var body []byte
	call := func(cmd *cobra.Command, m, p string, q url.Values, kind string, b []byte) error {
		method, path, query, body = m, p, q, b
		return nil
	}
	get := cli.OperationCommand(server.Operations[0], call)
	assert.Equal(t, "get <article_id>", get.Use)
	assert.Equal(t, mcp.AccessRead, cli.Access(get))
	get.SetArgs([]string{"42", "--verbose"})
	assert.NoError(t, get.Execute())
	assert.Equal(t, http.MethodGet, method)
	assert.Equal(t, "/articles/42", path)
	assert.Equal(t, url.Values{"verbose": {"true"}}, query)
	assert.Nil(t, body)

	create := cli.OperationCommand(server.Operations[2], call)
	create.SetArgs([]string{"--title", "Atlas", "--draft"})
	assert.NoError(t, create.Execute())
	assert.Equal(t, http.MethodPost, method)
	assert.JSONEq(t, `{"title":"Atlas","draft":true}`, string(body))
}

type sourceInput struct {
	SourceID string            `param:"source_id" json:"source_id,omitempty" jsonschema:"the source to read, every source when left out"`
	AppSlug  string            `header:"SAuth-App-Slug" json:"app_slug,omitempty" jsonschema:"the app the session belongs to"`
	Format   string            `query:"format" json:"format,omitempty"`
	Vars     map[string]string `query:"var.*" json:"var,omitempty" jsonschema:"variables, var.<name>"`
	Filters  map[string]any    `query:"*" json:"filter,omitempty" jsonschema:"any other filter"`
	Title    string            `json:"title,omitempty"`
}

type source struct {
	ID     string            `json:"id"`
	App    string            `json:"app"`
	Route  string            `json:"route"`
	Title  string            `json:"title"`
	Format string            `json:"format"`
	Vars   map[string]string `json:"vars"`
	Filter map[string]any    `json:"filter"`
	Query  map[string]string `json:"query"`
}

func routeService(chain *int) (*echo.Echo, []mcp.Operation) {
	auth := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(ctx echo.Context) error {
			if ctx.Request().Header.Get(echo.HeaderAuthorization) != "Bearer good" {
				return ctx.String(http.StatusUnauthorized, "Unauthorized")
			}
			*chain++
			return next(ctx)
		}
	}
	session := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(ctx echo.Context) error {
			slug := ctx.Param("app_slug")
			if slug == "" {
				slug = ctx.Request().Header.Get("SAuth-App-Slug")
			}
			if slug == "" {
				return respond.Error(ctx, http.StatusUnauthorized, "no app session", map[string]any{"needs": "app_slug"})
			}
			ctx.Set("app", slug)
			return next(ctx)
		}
	}
	read := func(ctx echo.Context, in sourceInput) (source, error) {
		query := map[string]string{}
		for key := range ctx.QueryParams() {
			query[key] = ctx.QueryParam(key)
		}
		return source{ID: in.SourceID, App: ctx.Get("app").(string), Route: ctx.Path(), Title: in.Title, Format: in.Format, Vars: in.Vars, Filter: in.Filters, Query: query}, nil
	}
	chained := []echo.MiddlewareFunc{auth, session}
	get := mcp.Define(mcp.Spec{Name: "source_get", Description: "Read sources, or one source", Method: http.MethodGet, Path: "/sources", Paths: []string{"/sources/:source_id"}, Access: mcp.AccessRead, Middleware: chained}, read)
	put := mcp.Define(mcp.Spec{Name: "source_replace", Description: "Replace a source", Method: http.MethodPut, Path: "/sources/:source_id", Access: mcp.AccessWrite, Middleware: chained}, read)
	test := mcp.Define(mcp.Spec{Name: "source_test", Description: "Test a source", Method: http.MethodPost, Path: "/sources/:source_id/test", Status: http.StatusOK, Access: mcp.AccessWrite, Middleware: chained}, read)
	run := mcp.Define(mcp.Spec{Name: "source_run", Description: "Run a source", Method: http.MethodPost, Path: "/sources/:source_id/run", Status: http.StatusAccepted, Access: mcp.AccessWrite, Middleware: chained}, read)
	remove := mcp.Define(mcp.Spec{Name: "source_delete", Description: "Delete a source", Method: http.MethodDelete, Path: "/sources/:source_id", Status: http.StatusNoContent, Access: mcp.AccessDestructive, Middleware: chained},
		func(ctx echo.Context, in sourceInput) (struct{}, error) {
			return struct{}{}, nil
		})
	check := mcp.Define(mcp.Spec{Name: "source_check", Description: "Check a source", Method: http.MethodPost, Path: "/sources/:source_id/check", Status: http.StatusOK, Invalid: "the check body is malformed", Access: mcp.AccessRead, Middleware: chained},
		func(ctx echo.Context, in struct {
			SourceID string `param:"source_id" json:"source_id"`
			AppSlug  string `header:"SAuth-App-Slug" json:"app_slug,omitempty"`
			Limit    int    `json:"limit_rows"`
		}) (map[string]any, error) {
			switch in.SourceID {
			case "bad":
				return nil, &respond.Rejection{Status: http.StatusBadRequest, Message: "source_id must be a uuid"}
			case "conflict":
				return nil, &respond.Rejection{Status: http.StatusConflict, Message: "the source is running", Data: map[string]any{"run_id": "r1"}}
			case "broken":
				return nil, errors.New("database refused the query")
			case "loud":
				return nil, &respond.Rejection{Status: http.StatusInternalServerError, Message: errors.New("database refused the query").Error()}
			}
			return map[string]any{"checked": in.SourceID, "rows": in.Limit}, nil
		})
	operations := []mcp.Operation{get, put, test, run, remove, check}
	e := echo.New()
	mcp.Route(e.Group("/api"), operations...)
	server := &mcp.Server{Name: "demo", Version: "v1", Operations: operations, AuthorizationServer: "https://auth.example.test"}
	server.Mount(e)
	return e, operations
}

func serveRoute(e *echo.Echo, method, target string, header map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderAuthorization, "Bearer good")
	if body != "" {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	for key, value := range header {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func callTool(t *testing.T, e *echo.Echo, name, arguments string) map[string]any {
	rec := post(e, "good", "application/json", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+arguments+`}}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	return result(t, rec)
}

func routeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	var envelope map[string]any
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope), rec.Body.String())
	return envelope
}

func TestDefineStatusIsTheSpecs(t *testing.T) {
	var chain int
	e, _ := routeService(&chain)
	slug := map[string]string{"SAuth-App-Slug": "portal"}
	assert.Equal(t, http.StatusOK, serveRoute(e, http.MethodPost, "/api/sources/7/test", slug, "").Code)
	assert.Equal(t, http.StatusAccepted, serveRoute(e, http.MethodPost, "/api/sources/7/run", slug, "").Code)
	removed := serveRoute(e, http.MethodDelete, "/api/sources/7", slug, "")
	assert.Equal(t, http.StatusNoContent, removed.Code)
	assert.Empty(t, removed.Body.String())

	for _, name := range []string{"source_test", "source_run", "source_delete"} {
		called := callTool(t, e, name, `{"source_id":"7","app_slug":"portal"}`)
		assert.Nil(t, called["isError"], name)
	}
}

func TestDefineErrorIsTheServicesOnBothTransports(t *testing.T) {
	var chain int
	e, _ := routeService(&chain)
	slug := map[string]string{"SAuth-App-Slug": "portal"}
	for id, want := range map[string]struct {
		status  int
		message string
		data    any
	}{
		"bad":      {http.StatusBadRequest, "source_id must be a uuid", nil},
		"conflict": {http.StatusConflict, "the source is running", map[string]any{"run_id": "r1"}},
		"broken":   {http.StatusInternalServerError, "Internal server error", nil},
		"loud":     {http.StatusInternalServerError, "database refused the query", nil},
	} {
		rec := serveRoute(e, http.MethodPost, "/api/sources/"+id+"/check", slug, "")
		assert.Equal(t, want.status, rec.Code, id)
		envelope := routeEnvelope(t, rec)
		assert.Equal(t, want.message, envelope["error"].(map[string]any)["message"], id)
		called := callTool(t, e, "source_check", `{"source_id":"`+id+`","app_slug":"portal"}`)
		assert.Equal(t, true, called["isError"], id)
		if id == "broken" {
			assert.Equal(t, "the server failed on source_check", called["content"].([]any)[0].(map[string]any)["text"])
			continue
		}
		assert.Equal(t, want.message, called["content"].([]any)[0].(map[string]any)["text"], id)
		if want.data == nil {
			assert.Nil(t, called["structuredContent"], id)
			continue
		}
		assert.Equal(t, want.data, envelope["data"], id)
		assert.Equal(t, want.data, called["structuredContent"].(map[string]any)["data"], id)
	}

	rec := serveRoute(e, http.MethodPost, "/api/sources/7/check", slug, `{"limit_rows":"many"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "the check body is malformed", routeEnvelope(t, rec)["error"].(map[string]any)["message"])
	called := callTool(t, e, "source_check", `{"source_id":"7","app_slug":"portal","limit_rows":"many"}`)
	assert.Equal(t, true, called["isError"])
	assert.Equal(t, "the check body is malformed", called["content"].([]any)[0].(map[string]any)["text"])
}

func TestDefineOpenQueryKeysReachBothTransports(t *testing.T) {
	var chain int
	e, _ := routeService(&chain)
	rec := serveRoute(e, http.MethodGet, "/api/sources/7?format=d2&var.region=eu&claim=reader&with=owner&limit=5", map[string]string{"SAuth-App-Slug": "portal"}, "")
	assert.Equal(t, http.StatusOK, rec.Code)
	routed := routeEnvelope(t, rec)["data"].(map[string]any)
	called := callTool(t, e, "source_get", `{"source_id":"7","app_slug":"portal","format":"d2","var":{"region":"eu"},"filter":{"claim":"reader","with":"owner"},"limit":5}`)
	assert.Nil(t, called["isError"])
	tooled := called["structuredContent"].(map[string]any)["data"].(map[string]any)
	for _, data := range []map[string]any{routed, tooled} {
		assert.Equal(t, "d2", data["format"])
		assert.Equal(t, map[string]any{"region": "eu"}, data["vars"])
		assert.Equal(t, map[string]any{"claim": "reader", "with": "owner"}, data["filter"])
		assert.Equal(t, map[string]any{"format": "d2", "var.region": "eu", "claim": "reader", "with": "owner", "limit": "5"}, data["query"])
	}

	_, operations := routeService(&chain)
	properties := operations[0].InputSchema["properties"].(map[string]any)
	assert.Equal(t, "variables, var.<name>", properties["var"].(map[string]any)["description"])
	assert.Contains(t, properties, "filter")
}

func TestDefineHeaderPlaceholderReachesTheMiddleware(t *testing.T) {
	var chain int
	e, _ := routeService(&chain)
	rec := serveRoute(e, http.MethodGet, "/api/sources/7", map[string]string{"SAuth-App-Slug": "portal"}, "")
	assert.Equal(t, "portal", routeEnvelope(t, rec)["data"].(map[string]any)["app"])
	called := callTool(t, e, "source_get", `{"source_id":"7","app_slug":"portal"}`)
	assert.Equal(t, "portal", called["structuredContent"].(map[string]any)["data"].(map[string]any)["app"])

	rec = serveRoute(e, http.MethodGet, "/api/sources/7", nil, "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, map[string]any{"needs": "app_slug"}, routeEnvelope(t, rec)["data"])
	called = callTool(t, e, "source_get", `{"source_id":"7"}`)
	assert.Equal(t, true, called["isError"])
	assert.Equal(t, "no app session", called["content"].([]any)[0].(map[string]any)["text"])
	assert.Equal(t, map[string]any{"needs": "app_slug"}, called["structuredContent"].(map[string]any)["data"])
}

func TestDefineOneOperationOverSeveralPaths(t *testing.T) {
	var chain int
	e, operations := routeService(&chain)
	assert.NotContains(t, operations[0].InputSchema, "required")
	assert.Equal(t, []string{"source_id"}, operations[1].InputSchema["required"])

	slug := map[string]string{"SAuth-App-Slug": "portal"}
	listed := routeEnvelope(t, serveRoute(e, http.MethodGet, "/api/sources", slug, ""))["data"].(map[string]any)
	one := routeEnvelope(t, serveRoute(e, http.MethodGet, "/api/sources/7", slug, ""))["data"].(map[string]any)
	assert.Equal(t, "/api/sources", listed["route"])
	assert.Equal(t, "", listed["id"])
	assert.Equal(t, "/api/sources/:source_id", one["route"])
	assert.Equal(t, "7", one["id"])

	listed = callTool(t, e, "source_get", `{"app_slug":"portal"}`)["structuredContent"].(map[string]any)["data"].(map[string]any)
	one = callTool(t, e, "source_get", `{"source_id":"7","app_slug":"portal"}`)["structuredContent"].(map[string]any)["data"].(map[string]any)
	assert.Equal(t, "/sources", listed["route"])
	assert.Equal(t, "", listed["id"])
	assert.Equal(t, "/sources/:source_id", one["route"])
	assert.Equal(t, "7", one["id"])

	called := callTool(t, e, "source_replace", `{"app_slug":"portal"}`)
	assert.Equal(t, true, called["isError"])
	assert.Equal(t, "source_replace needs source_id", called["content"].([]any)[0].(map[string]any)["text"])
}

func TestDefinePathParamsWinOverTheBody(t *testing.T) {
	var chain int
	e, _ := routeService(&chain)
	rec := serveRoute(e, http.MethodPut, "/api/sources/7", map[string]string{"SAuth-App-Slug": "portal"}, `{"source_id":"other","title":"Atlas"}`)
	data := routeEnvelope(t, rec)["data"].(map[string]any)
	assert.Equal(t, "7", data["id"])
	assert.Equal(t, "Atlas", data["title"])
}

func TestDefineMiddlewareIsOneChain(t *testing.T) {
	var chain int
	e, _ := routeService(&chain)
	serveRoute(e, http.MethodGet, "/api/sources/7", map[string]string{"SAuth-App-Slug": "portal"}, "")
	assert.Equal(t, 1, chain)
	callTool(t, e, "source_get", `{"source_id":"7","app_slug":"portal"}`)
	assert.Equal(t, 2, chain)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sources/7", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	called := callTool(t, e, "source_get", `{"source_id":"7","app_slug":"portal"}`)
	assert.Nil(t, called["isError"])
}

func TestMountRefusesTwoToolsWithOneName(t *testing.T) {
	var chain int
	_, operations := routeService(&chain)
	e := echo.New()
	err := endpoint.Mount(e, endpoint.Set{Service: "demo", Version: "v1", MCP: &mcp.Server{Name: "demo", Operations: append(operations, operations[0])}})
	assert.EqualError(t, err, "endpoint: two tools are called source_get")
	assert.NoError(t, endpoint.Mount(echo.New(), endpoint.Set{Service: "demo", Version: "v1", MCP: &mcp.Server{Name: "demo", Operations: operations}}))
}

func TestOperationCommandCoversSeveralPaths(t *testing.T) {
	var chain int
	_, operations := routeService(&chain)
	var path string
	var query url.Values
	call := func(cmd *cobra.Command, m, p string, q url.Values, kind string, b []byte) error {
		path, query = p, q
		return nil
	}
	get := cli.OperationCommand(operations[0], call)
	assert.Equal(t, "get [source_id]", get.Use)
	assert.Nil(t, get.Flags().Lookup("SAuth-App-Slug"))
	get.SetArgs([]string{})
	assert.NoError(t, get.Execute())
	assert.Equal(t, "/sources", path)
	get = cli.OperationCommand(operations[0], call)
	get.SetArgs([]string{"7", "--var", "region=eu", "--filter", "claim=reader"})
	assert.NoError(t, get.Execute())
	assert.Equal(t, "/sources/7", path)
	assert.Equal(t, url.Values{"var.region": {"eu"}, "claim": {"reader"}}, query)
}

type deviceInput struct {
	nimbody.Body
	DeviceID   string  `param:"device_id" json:"device_id"`
	Name       *string `json:"name"`
	LocationID *string `json:"location_id"`
}

type callInput struct {
	Name  string          `param:"name" json:"name"`
	Input json.RawMessage `json:"input,omitempty" body:"*"`
}

type readCounter struct {
	io.Reader
	read *bool
}

func (r readCounter) Read(p []byte) (int, error) {
	*r.read = true
	return r.Reader.Read(p)
}

func bodyService() *echo.Echo {
	put := mcp.Define(mcp.Spec{Name: "device_update", Description: "Change a device", Method: http.MethodPut, Path: "/devices/:device_id", Access: mcp.AccessWrite},
		func(ctx echo.Context, in deviceInput) (map[string]any, error) {
			if denied := in.Deny("id", "sa_*"); len(denied) > 0 {
				return nil, &respond.Rejection{Status: http.StatusBadRequest, Message: "read only fields: " + denied[0]}
			}
			keys := []string{}
			if in.Validator() != nil {
				for key := range in.Validator().Body {
					keys = append(keys, key)
				}
			}
			return map[string]any{
				"device":         in.DeviceID,
				"keys":           keys,
				"has_name":       in.HasKey("name"),
				"clear_location": in.HasKey("location_id") && in.LocationID == nil,
				"empty":          in.IsEmpty(),
			}, nil
		})
	call := mcp.Define(mcp.Spec{Name: "operation_call", Description: "Call an operation", Method: http.MethodPost, Path: "/calls/:name", Status: http.StatusOK, Access: mcp.AccessWrite},
		func(ctx echo.Context, in callInput) (map[string]any, error) {
			if in.Name == "gone" {
				return nil, &respond.Rejection{Status: http.StatusNotFound, Message: "operation not found"}
			}
			raw, err := io.ReadAll(io.LimitReader(ctx.Request().Body, 65))
			if err != nil || len(raw) > 64 {
				return nil, &respond.Rejection{Status: http.StatusRequestEntityTooLarge, Message: "input is larger than 64 bytes"}
			}
			var input any
			if err := json.Unmarshal(raw, &input); err != nil {
				return nil, &respond.Rejection{Status: http.StatusBadRequest, Message: "input must be json"}
			}
			return map[string]any{"input": input, "field": len(in.Input)}, nil
		})
	operations := []mcp.Operation{put, call}
	e := echo.New()
	mcp.Route(e.Group("/api"), operations...)
	server := &mcp.Server{Name: "demo", Version: "v1", Operations: operations, AuthorizationServer: "https://auth.example.test"}
	server.Mount(e)
	return e
}

func TestDefineRecordsTheBodyKeysOnBothTransports(t *testing.T) {
	e := bodyService()
	rec := serveRoute(e, http.MethodPut, "/api/devices/7", nil, `{"name":"Hall","location_id":null}`)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	routed := routeEnvelope(t, rec)["data"].(map[string]any)
	called := callTool(t, e, "device_update", `{"device_id":"7","name":"Hall","location_id":null,"fields":["device"],"limit":3}`)
	assert.Nil(t, called["isError"])
	tooled := called["structuredContent"].(map[string]any)["data"].(map[string]any)
	for _, data := range []map[string]any{routed, tooled} {
		assert.Equal(t, "7", data["device"])
		assert.ElementsMatch(t, []any{"name", "location_id"}, data["keys"])
		assert.Equal(t, true, data["has_name"])
		assert.Equal(t, true, data["clear_location"])
		assert.Equal(t, false, data["empty"])
	}

	routed = routeEnvelope(t, serveRoute(e, http.MethodPut, "/api/devices/7", nil, `{"name":"Hall"}`))["data"].(map[string]any)
	tooled = callTool(t, e, "device_update", `{"device_id":"7","name":"Hall"}`)["structuredContent"].(map[string]any)["data"].(map[string]any)
	for _, data := range []map[string]any{routed, tooled} {
		assert.Equal(t, false, data["clear_location"])
	}

	routed = routeEnvelope(t, serveRoute(e, http.MethodPut, "/api/devices/7", nil, ""))["data"].(map[string]any)
	tooled = callTool(t, e, "device_update", `{"device_id":"7"}`)["structuredContent"].(map[string]any)["data"].(map[string]any)
	for _, data := range []map[string]any{routed, tooled} {
		assert.Equal(t, false, data["has_name"])
		assert.Equal(t, true, data["empty"])
	}

	rec = serveRoute(e, http.MethodPut, "/api/devices/7", nil, `{"name":"Hall","sa_owner":"x"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "read only fields: sa_owner", routeEnvelope(t, rec)["error"].(map[string]any)["message"])
	called = callTool(t, e, "device_update", `{"device_id":"7","name":"Hall","sa_owner":"x"}`)
	assert.Equal(t, true, called["isError"])
	assert.Equal(t, "read only fields: sa_owner", called["content"].([]any)[0].(map[string]any)["text"])
}

func TestDefineRawBodyIsAnyJSONOnBothTransports(t *testing.T) {
	e := bodyService()
	for _, input := range []string{`[1,"two",{"three":3}]`, `42`, `"hall"`, `true`} {
		var want any
		assert.NoError(t, json.Unmarshal([]byte(input), &want))
		rec := serveRoute(e, http.MethodPost, "/api/calls/read", nil, input)
		assert.Equal(t, http.StatusOK, rec.Code, input)
		routed := routeEnvelope(t, rec)["data"].(map[string]any)
		called := callTool(t, e, "operation_call", `{"name":"read","input":`+input+`}`)
		assert.Nil(t, called["isError"], input)
		tooled := called["structuredContent"].(map[string]any)["data"].(map[string]any)
		for _, data := range []map[string]any{routed, tooled} {
			assert.Equal(t, want, data["input"], input)
			assert.Equal(t, float64(0), data["field"], input)
		}
	}

	rec := serveRoute(e, http.MethodPost, "/api/calls/read", nil, `"`+strings.Repeat("a", 80)+`"`)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	called := callTool(t, e, "operation_call", `{"name":"read","input":"`+strings.Repeat("a", 80)+`"}`)
	assert.Equal(t, "input is larger than 64 bytes", called["content"].([]any)[0].(map[string]any)["text"])
}

func TestDefineRawBodyNeedsNoContentType(t *testing.T) {
	e := bodyService()
	for _, contentType := range []string{"", "text/plain", "application/octet-stream"} {
		req := httptest.NewRequest(http.MethodPost, "/api/calls/read", strings.NewReader(`[1,2]`))
		if contentType != "" {
			req.Header.Set(echo.HeaderContentType, contentType)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, contentType)
		assert.Equal(t, []any{float64(1), float64(2)}, routeEnvelope(t, rec)["data"].(map[string]any)["input"], contentType)
	}
}

func TestDefineRawBodyAnswersNotFoundBeforeTheBodyIsRead(t *testing.T) {
	e := bodyService()
	read := false
	req := httptest.NewRequest(http.MethodPost, "/api/calls/gone", readCounter{Reader: strings.NewReader(`{"broken`), read: &read})
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "operation not found", routeEnvelope(t, rec)["error"].(map[string]any)["message"])
	assert.False(t, read)

	called := callTool(t, e, "operation_call", `{"name":"gone","input":[1]}`)
	assert.Equal(t, true, called["isError"])
	assert.Equal(t, "operation not found", called["content"].([]any)[0].(map[string]any)["text"])

	rec = serveRoute(e, http.MethodPut, "/api/devices/7", nil, `{"name":`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestOperationCommandSendsTheRawBodyAsGiven(t *testing.T) {
	operation := mcp.Define(mcp.Spec{Name: "operation_call", Description: "Call an operation", Method: http.MethodPost, Path: "/calls/:name", Access: mcp.AccessWrite},
		func(ctx echo.Context, in callInput) (map[string]any, error) {
			return nil, nil
		})
	var sent []byte
	call := func(cmd *cobra.Command, m, p string, q url.Values, kind string, b []byte) error {
		sent = b
		return nil
	}
	file := t.TempDir() + "/input.json"
	assert.NoError(t, os.WriteFile(file, []byte(`42`), 0o600))
	for _, run := range []struct {
		args  []string
		stdin string
		want  string
	}{
		{[]string{"read", "--data", `[1,"two"]`}, "", `[1,"two"]`},
		{[]string{"read", "--file", file}, "", `42`},
		{[]string{"read", "--file", "-"}, `"hall"`, `"hall"`},
		{[]string{"read"}, "", ``},
	} {
		sent = nil
		cmd := cli.OperationCommand(operation, call)
		assert.Nil(t, cmd.Flags().Lookup("*"))
		assert.Nil(t, cmd.Flags().Lookup("set"))
		cmd.SetIn(strings.NewReader(run.stdin))
		cmd.SetArgs(run.args)
		assert.NoError(t, cmd.Execute(), run.args)
		assert.Equal(t, run.want, string(sent), run.args)
	}

	for _, args := range [][]string{{"read", "--data", `{"broken`}, {"read", "--data", `1`, "--file", file}} {
		cmd := cli.OperationCommand(operation, call)
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		cmd.SetArgs(args)
		assert.Error(t, cmd.Execute(), args)
	}
}

type syncRun struct {
	ID      string   `json:"id"`
	Mappers []string `json:"mappers"`
}

func answerService() (*echo.Echo, []mcp.Operation) {
	post := mcp.Define(mcp.Spec{Name: "mapper_create", Description: "Add a mapper, or answer the one with the same name", Method: http.MethodPost, Path: "/mappers", Access: mcp.AccessWrite},
		func(ctx echo.Context, in articleCreate) (mcp.Answer[article], error) {
			if in.Title == "existing" {
				return mcp.Answer[article]{Data: article{ID: "old", Title: in.Title}, Status: http.StatusOK}, nil
			}
			return mcp.Answer[article]{Data: article{ID: "new", Title: in.Title}}, nil
		})
	sync := mcp.Define(mcp.Spec{Name: "node_sync", Description: "Sync a node", Method: http.MethodPost, Path: "/nodes/:node_id/sync", Status: http.StatusOK, Access: mcp.AccessWrite},
		func(ctx echo.Context, in struct {
			NodeID string `param:"node_id" json:"node_id"`
		}) (mcp.Answer[syncRun], error) {
			run := syncRun{ID: in.NodeID, Mappers: []string{"m1", "m2", "m3"}}
			count := len(run.Mappers)
			return mcp.Answer[syncRun]{Data: run, Count: &count}, nil
		})
	test := mcp.Define(mcp.Spec{Name: "node_test", Description: "Test a node", Method: http.MethodPost, Path: "/nodes/:node_id/test", Status: http.StatusOK, Access: mcp.AccessWrite},
		func(ctx echo.Context, in struct {
			NodeID string `param:"node_id" json:"node_id"`
		}) (mcp.Answer[[]string], error) {
			one := 1
			return mcp.Answer[[]string]{Data: []string{"a", "b", "c"}, Count: &one}, nil
		})
	objects := mcp.Define(mcp.Spec{Name: "provider_object_get", Description: "Read provider objects", Method: http.MethodGet, Path: "/providers/:provider_id/objects", Access: mcp.AccessRead},
		func(ctx echo.Context, in struct {
			ProviderID string `param:"provider_id" json:"provider_id"`
		}) (mcp.Answer[[]article], error) {
			none := 0
			return mcp.Answer[[]article]{Data: []article{{ID: "a"}, {ID: "b"}}, Count: &none}, nil
		})
	operations := []mcp.Operation{post, sync, test, objects}
	e := echo.New()
	mcp.Route(e.Group("/api"), operations...)
	server := &mcp.Server{Name: "demo", Version: "v1", Operations: operations, AuthorizationServer: "https://auth.example.test"}
	server.Mount(e)
	return e, operations
}

func TestDefineStatusIsTheServicesPerCall(t *testing.T) {
	e, _ := answerService()
	for title, want := range map[string]int{"fresh": http.StatusCreated, "existing": http.StatusOK} {
		rec := serveRoute(e, http.MethodPost, "/api/mappers", nil, `{"title":"`+title+`"}`)
		assert.Equal(t, want, rec.Code, title)
		envelope := routeEnvelope(t, rec)
		assert.Equal(t, title, envelope["data"].(map[string]any)["title"], title)
		assert.Equal(t, float64(1), envelope["count"], title)

		streamed := serveRoute(e, http.MethodPost, "/api/mappers", map[string]string{echo.HeaderAccept: respond.MIMEEventStream}, `{"title":"`+title+`"}`)
		assert.Equal(t, http.StatusOK, streamed.Code, title)
		assert.Contains(t, streamed.Body.String(), `"status":`+strconv.Itoa(want), title)

		called := callTool(t, e, "mapper_create", `{"title":"`+title+`"}`)
		assert.Nil(t, called["isError"], title)
		content := called["structuredContent"].(map[string]any)
		assert.Equal(t, float64(want), content["status"], title)
		assert.Equal(t, float64(1), content["count"], title)
		assert.Equal(t, title, content["data"].(map[string]any)["title"], title)
	}

	var chain int
	plain, _ := routeService(&chain)
	called := callTool(t, plain, "source_test", `{"source_id":"7","app_slug":"portal"}`)
	assert.NotContains(t, called["structuredContent"], "status")
	assert.NotContains(t, called["structuredContent"], "count")
}

func TestDefineCountIsTheServicesOnBothTransports(t *testing.T) {
	e, operations := answerService()
	for name, want := range map[string]struct {
		method string
		path   string
		count  float64
		rows   int
	}{
		"node_sync":           {http.MethodPost, "/api/nodes/n1/sync", 3, -1},
		"node_test":           {http.MethodPost, "/api/nodes/n1/test", 1, 3},
		"provider_object_get": {http.MethodGet, "/api/providers/p1/objects", 0, 2},
	} {
		rec := serveRoute(e, want.method, want.path, nil, "")
		assert.Equal(t, http.StatusOK, rec.Code, name)
		envelope := routeEnvelope(t, rec)
		assert.Equal(t, want.count, envelope["count"], name)
		if want.rows >= 0 {
			assert.Len(t, envelope["data"], want.rows, name)
		}

		called := callTool(t, e, name, `{"node_id":"n1","provider_id":"p1"}`)
		assert.Nil(t, called["isError"], name)
		content := called["structuredContent"].(map[string]any)
		assert.Equal(t, float64(http.StatusOK), content["status"], name)
		assert.Equal(t, want.count, content["count"], name)
		if want.rows >= 0 {
			assert.Len(t, content["data"], want.rows, name)
		}
	}
	assert.Equal(t, []any{"m1", "m2", "m3"}, routeEnvelope(t, serveRoute(e, http.MethodPost, "/api/nodes/n1/sync", nil, ""))["data"].(map[string]any)["mappers"])

	properties := operations[1].OutputSchema["properties"].(map[string]any)
	assert.Equal(t, map[string]any{"type": "integer"}, properties["status"])
	assert.Equal(t, map[string]any{"type": "integer"}, properties["count"])
	assert.Contains(t, properties["data"].(map[string]any)["properties"], "mappers")
	assert.Equal(t, []string{"data"}, operations[1].OutputSchema["required"])
	assert.Equal(t, []string{"array", "null"}, operations[3].OutputSchema["properties"].(map[string]any)["data"].(map[string]any)["type"])
}

type assetUpload struct {
	nimbody.Body
	Name  string     `json:"name,omitempty"`
	Size  int        `json:"size,omitempty"`
	File  mcp.File   `file:"file" json:"file"`
	Extra []mcp.File `file:"extra" json:"extra,omitempty"`
}

type feedInput struct {
	Feed        json.RawMessage `body:"*" json:"feed"`
	ContentType string          `header:"Content-Type" json:"content_type,omitempty"`
}

func fileService() (*echo.Echo, []mcp.Operation) {
	create := mcp.Define(mcp.Spec{Name: "asset_create", Description: "Add an asset", Method: http.MethodPost, Path: "/assets", FileLimit: 16, Access: mcp.AccessWrite},
		func(ctx echo.Context, in assetUpload) (map[string]any, error) {
			extra := []string{}
			for _, file := range in.Extra {
				extra = append(extra, file.Name+":"+string(file.Content))
			}
			return map[string]any{"name": in.Name, "size": in.Size, "has_name": in.HasKey("name"), "file_name": in.File.Name, "content_type": in.File.ContentType, "content": string(in.File.Content), "extra": extra}, nil
		})
	data := mcp.Define(mcp.Spec{Name: "asset_data", Description: "Read the bytes of an asset", Method: http.MethodGet, Path: "/assets/:asset_id/data", FileLimit: 32, Access: mcp.AccessRead},
		func(ctx echo.Context, in struct {
			AssetID string `param:"asset_id" json:"asset_id"`
		}) (mcp.Blob, error) {
			switch in.AssetID {
			case "logo":
				return mcp.Blob{Content: []byte("\x89PNG\r\n\x1a\nlogo"), ContentType: "image/png", ETag: `"v1"`, CacheControl: "no-store"}, nil
			case "big":
				return mcp.Blob{Content: bytes.Repeat([]byte("a"), 33), ContentType: "text/plain"}, nil
			case "gone":
				return mcp.Blob{}, &respond.Rejection{Status: http.StatusNotFound, Message: "no asset is gone"}
			}
			return mcp.Blob{Content: []byte("%PDF-1.7"), ContentType: "application/pdf"}, nil
		})
	feed := mcp.Define(mcp.Spec{Name: "subscription_create", Description: "Add a subscription from a feed", Method: http.MethodPost, Path: "/subscriptions", Status: http.StatusOK, Access: mcp.AccessWrite},
		func(ctx echo.Context, in feedInput) (map[string]any, error) {
			raw, err := io.ReadAll(ctx.Request().Body)
			if err != nil {
				return nil, err
			}
			return map[string]any{"content_type": ctx.Request().Header.Get(echo.HeaderContentType), "body": string(raw)}, nil
		})
	operations := []mcp.Operation{create, data, feed}
	e := echo.New()
	mcp.Route(e.Group("/api"), operations...)
	server := &mcp.Server{Name: "demo", Version: "v1", Operations: operations, AuthorizationServer: "https://auth.example.test"}
	server.Mount(e)
	return e, operations
}

func multipartBody(t *testing.T, fields map[string]string, files ...[3]string) (*bytes.Buffer, string) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		assert.NoError(t, writer.WriteField(key, value))
	}
	for _, file := range files {
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", `form-data; name="`+file[0]+`"; filename="`+file[1]+`"`)
		part, err := writer.CreatePart(header)
		assert.NoError(t, err)
		_, err = part.Write([]byte(file[2]))
		assert.NoError(t, err)
	}
	assert.NoError(t, writer.Close())
	return &body, writer.FormDataContentType()
}

func TestDefineFileBindsFromMultipartAndFromATool(t *testing.T) {
	e, operations := fileService()
	body, contentType := multipartBody(t, map[string]string{"name": "logo", "size": "5"}, [3]string{"file", "logo.txt", "hello"}, [3]string{"extra", "a.txt", "one"}, [3]string{"extra", "b.txt", "two"})
	rec := serveRoute(e, http.MethodPost, "/api/assets", map[string]string{echo.HeaderContentType: contentType}, body.String())
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	routed := routeEnvelope(t, rec)["data"].(map[string]any)

	encoded := base64.StdEncoding.EncodeToString([]byte("hello"))
	called := callTool(t, e, "asset_create", `{"name":"logo","size":5,"file":{"name":"logo.txt","content":"`+encoded+`"},"extra":[{"name":"a.txt","content":"`+base64.StdEncoding.EncodeToString([]byte("one"))+`"},{"name":"b.txt","content":"`+base64.StdEncoding.EncodeToString([]byte("two"))+`"}]}`)
	assert.Nil(t, called["isError"], called["content"])
	tooled := called["structuredContent"].(map[string]any)["data"].(map[string]any)
	for _, data := range []map[string]any{routed, tooled} {
		assert.Equal(t, "logo", data["name"])
		assert.Equal(t, float64(5), data["size"])
		assert.Equal(t, true, data["has_name"])
		assert.Equal(t, "logo.txt", data["file_name"])
		assert.Equal(t, "text/plain; charset=utf-8", data["content_type"])
		assert.Equal(t, "hello", data["content"])
		assert.Equal(t, []any{"a.txt:one", "b.txt:two"}, data["extra"])
	}

	called = callTool(t, e, "asset_create", `{"file":{"name":"logo.svg","content_type":"image/svg+xml","content":"`+encoded+`"}}`)
	assert.Equal(t, "image/svg+xml", called["structuredContent"].(map[string]any)["data"].(map[string]any)["content_type"])

	body, contentType = multipartBody(t, nil, [3]string{"file", "big.txt", strings.Repeat("a", 17)})
	rec = serveRoute(e, http.MethodPost, "/api/assets", map[string]string{echo.HeaderContentType: contentType}, body.String())
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, "file is larger than 16 bytes", routeEnvelope(t, rec)["error"].(map[string]any)["message"])
	called = callTool(t, e, "asset_create", `{"file":{"content":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), 17))+`"}}`)
	assert.Equal(t, true, called["isError"])
	assert.Equal(t, "file is larger than 16 bytes", called["content"].([]any)[0].(map[string]any)["text"])

	rec = serveRoute(e, http.MethodPost, "/api/assets", nil, `{"name":"pending"}`)
	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "pending", routeEnvelope(t, rec)["data"].(map[string]any)["name"])

	schema := operations[0].InputSchema
	file := schema["properties"].(map[string]any)["file"].(map[string]any)
	assert.Equal(t, "base64", file["properties"].(map[string]any)["content"].(map[string]any)["contentEncoding"])
	assert.Contains(t, schema["required"], "file")
	assert.NotContains(t, schema["required"], "extra")
}

func TestOperationCommandSendsAnUploadAsMultipart(t *testing.T) {
	_, operations := fileService()
	var kind string
	var sent []byte
	call := func(cmd *cobra.Command, m, p string, q url.Values, contentType string, b []byte) error {
		kind, sent = contentType, b
		return nil
	}
	file := t.TempDir() + "/logo.txt"
	assert.NoError(t, os.WriteFile(file, []byte("hello"), 0o600))
	cmd := cli.OperationCommand(operations[0], call)
	assert.NotNil(t, cmd.Flags().Lookup("upload"))
	assert.NotNil(t, cmd.Flags().Lookup("upload-extra"))
	cmd.SetArgs([]string{"--upload", file, "--upload-extra", file, "--name", "logo", "--size", "5"})
	assert.NoError(t, cmd.Execute())
	mediaType, params, err := mime.ParseMediaType(kind)
	assert.NoError(t, err)
	assert.Equal(t, "multipart/form-data", mediaType)
	form, err := multipart.NewReader(bytes.NewReader(sent), params["boundary"]).ReadForm(1 << 20)
	assert.NoError(t, err)
	assert.Equal(t, []string{"logo"}, form.Value["name"])
	assert.Equal(t, []string{"5"}, form.Value["size"])
	assert.Equal(t, "logo.txt", form.File["file"][0].Filename)
	assert.Len(t, form.File["extra"], 1)

	cmd = cli.OperationCommand(operations[0], call)
	cmd.SetArgs([]string{"--name", "logo"})
	assert.NoError(t, cmd.Execute())
	assert.Equal(t, cli.ContentTypeJSON, kind)
	assert.JSONEq(t, `{"name":"logo"}`, string(sent))
}

func TestDefineBlobAnswersTheBytes(t *testing.T) {
	e, operations := fileService()
	rec := serveRoute(e, http.MethodGet, "/api/assets/logo/data", nil, "")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "\x89PNG\r\n\x1a\nlogo", rec.Body.String())
	assert.Equal(t, "image/png", rec.Header().Get(echo.HeaderContentType))
	assert.Equal(t, "12", rec.Header().Get(echo.HeaderContentLength))
	assert.Equal(t, `"v1"`, rec.Header().Get("ETag"))
	assert.Equal(t, "no-store", rec.Header().Get(echo.HeaderCacheControl))
	cached := serveRoute(e, http.MethodGet, "/api/assets/logo/data", map[string]string{"If-None-Match": `"v1"`}, "")
	assert.Equal(t, http.StatusNotModified, cached.Code)
	assert.Empty(t, cached.Body.String())
	assert.Equal(t, http.StatusNotFound, serveRoute(e, http.MethodGet, "/api/assets/gone/data", nil, "").Code)

	called := callTool(t, e, "asset_data", `{"asset_id":"logo"}`)
	assert.Nil(t, called["isError"])
	assert.Nil(t, called["structuredContent"])
	image := called["content"].([]any)[0].(map[string]any)
	assert.Equal(t, "image", image["type"])
	assert.Equal(t, "image/png", image["mimeType"])
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nlogo")), image["data"])

	called = callTool(t, e, "asset_data", `{"asset_id":"report"}`)
	block := called["content"].([]any)[0].(map[string]any)
	assert.Equal(t, "resource", block["type"])
	assert.Equal(t, map[string]any{"uri": "demo://assets/report/data", "mimeType": "application/pdf", "blob": base64.StdEncoding.EncodeToString([]byte("%PDF-1.7"))}, block["resource"])

	called = callTool(t, e, "asset_data", `{"asset_id":"big"}`)
	assert.Equal(t, true, called["isError"])
	assert.Equal(t, "asset_data answers 33 bytes, more than the 32 a tool call carries", called["content"].([]any)[0].(map[string]any)["text"])
	called = callTool(t, e, "asset_data", `{"asset_id":"gone"}`)
	assert.Equal(t, "no asset is gone", called["content"].([]any)[0].(map[string]any)["text"])

	assert.Nil(t, operations[1].OutputSchema)
	listed, _ := json.Marshal(operations[1].Tool())
	assert.NotContains(t, string(listed), "outputSchema")
}

func TestDefineRawBodyTakesAStringWithItsContentType(t *testing.T) {
	e, _ := fileService()
	rec := serveRoute(e, http.MethodPost, "/api/subscriptions", map[string]string{echo.HeaderContentType: "application/rss+xml"}, "<rss/>")
	assert.Equal(t, http.StatusOK, rec.Code)
	routed := routeEnvelope(t, rec)["data"].(map[string]any)
	tooled := callTool(t, e, "subscription_create", `{"feed":"<rss/>","content_type":"application/rss+xml"}`)["structuredContent"].(map[string]any)["data"].(map[string]any)
	for _, data := range []map[string]any{routed, tooled} {
		assert.Equal(t, "application/rss+xml", data["content_type"])
		assert.Equal(t, "<rss/>", data["body"])
	}
	for arguments, want := range map[string]map[string]any{
		`{"feed":"<rss/>"}`: {"content_type": "application/json", "body": `"<rss/>"`},
		`{"feed":"<rss/>","content_type":"application/feed+json"}`: {"content_type": "application/feed+json", "body": `"<rss/>"`},
		`{"feed":{"items":[]},"content_type":"text/xml"}`:          {"content_type": "text/xml", "body": `{"items":[]}`},
	} {
		assert.Equal(t, want, callTool(t, e, "subscription_create", arguments)["structuredContent"].(map[string]any)["data"], arguments)
	}
}

func TestOperationCommandSendsTheRawBodyWithItsContentType(t *testing.T) {
	_, operations := fileService()
	var kind string
	var sent []byte
	call := func(cmd *cobra.Command, m, p string, q url.Values, contentType string, b []byte) error {
		kind, sent = contentType, b
		return nil
	}
	feed := t.TempDir() + "/feed.xml"
	assert.NoError(t, os.WriteFile(feed, []byte("<rss/>"), 0o600))
	cmd := cli.OperationCommand(operations[2], call)
	cmd.SetArgs([]string{"--file", feed, "--content-type", "application/rss+xml"})
	assert.NoError(t, cmd.Execute())
	assert.Equal(t, "application/rss+xml", kind)
	assert.Equal(t, "<rss/>", string(sent))

	cmd = cli.OperationCommand(operations[2], call)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"--file", feed})
	assert.Error(t, cmd.Execute())

	cmd = cli.OperationCommand(operations[2], call)
	cmd.SetArgs([]string{"--data", `{"items":[]}`})
	assert.NoError(t, cmd.Execute())
	assert.Equal(t, cli.ContentTypeJSON, kind)
}

func TestRemoteBridgeKeepsTheLocalTools(t *testing.T) {
	e, _ := remoteService()
	backend := httptest.NewServer(e)
	defer backend.Close()
	remote := func(ctx context.Context) (string, string, error) { return backend.URL + "/mcp", "good", nil }
	ran := func(text string) func(context.Context, json.RawMessage, func(respond.Step)) (string, bool) {
		return func(ctx context.Context, arguments json.RawMessage, onStep func(respond.Step)) (string, bool) {
			return text, false
		}
	}
	local := &mcp.Server{Name: "demo", Version: "v1", Tools: []mcp.Tool{
		{Name: "article_get", Description: "the local article get", InputSchema: map[string]any{"type": "object"}, Run: ran("local article")},
		{Name: "cache_clear", Description: "Clear the local cache", InputSchema: map[string]any{"type": "object"}, Run: ran("cleared")},
	}}
	relay := func(lines ...string) map[float64]map[string]any {
		var out bytes.Buffer
		assert.NoError(t, cli.Relay(context.Background(), "demo", remote, strings.NewReader(strings.Join(lines, "\n")), &out, local))
		byID := map[float64]map[string]any{}
		scanner := bufio.NewScanner(&out)
		for scanner.Scan() {
			var message map[string]any
			assert.NoError(t, json.Unmarshal(scanner.Bytes(), &message), scanner.Text())
			if id, ok := message["id"].(float64); ok {
				byID[id] = message
			}
		}
		return byID
	}

	byID := relay(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"cache_clear","arguments":{}}}`)
	assert.Equal(t, "cleared", byID[1]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"])

	byID = relay(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`,
	)
	described := map[string]string{}
	for _, tool := range byID[1]["result"].(map[string]any)["tools"].([]any) {
		listed := tool.(map[string]any)
		_, twice := described[listed["name"].(string)]
		assert.False(t, twice, listed["name"])
		described[listed["name"].(string)] = listed["description"].(string)
	}
	assert.Contains(t, described["article_get"], "Read one article")
	assert.Contains(t, described, "article_publish")
	assert.Contains(t, described, "cache_clear")
	assert.Contains(t, described, "steps")
	uris := []string{}
	for _, listed := range byID[2]["result"].(map[string]any)["resources"].([]any) {
		uris = append(uris, listed.(map[string]any)["uri"].(string))
	}
	assert.Contains(t, uris, "ui://demo/run")

	byID = relay(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"article_get","arguments":{"article_id":"9"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"cache_clear","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"ui://demo/run"}}`,
	)
	assert.Equal(t, "9", byID[2]["result"].(map[string]any)["structuredContent"].(map[string]any)["data"].(map[string]any)["id"])
	assert.Equal(t, "cleared", byID[3]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"])
	assert.Equal(t, "ui://demo/run", byID[4]["result"].(map[string]any)["contents"].([]any)[0].(map[string]any)["uri"])
}
