package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
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
			steps := respond.GetSteps(ctx)
			steps.Start("read", "read the article", "")
			steps.Done("read", nil)
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
	operations := []mcp.Operation{get, list, create}
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
	assert.Len(t, tools, 3, "no run page and no steps tool over http")
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
	rec := post(e, "good", "application/json, text/event-stream", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"article_get","arguments":{"article_id":"42"},"_meta":{"progressToken":"p1"}}}`)
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
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"article_get","arguments":{"article_id":"9"},"_meta":{"progressToken":1}}}`,
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
	call := func(cmd *cobra.Command, m, p string, q url.Values, b []byte) error {
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

func tableTree() *cobra.Command {
	settings := &cli.Settings{Name: "demo", Prefix: "DEMO_"}
	output := &cli.Output{}
	root := &cobra.Command{Use: "demo", Short: "Operate the demo api"}
	cli.Root(root, settings, output)
	root.PersistentFlags().String("endpoint", "", "api base url")
	root.PersistentFlags().String("app", "", "app slug the request is made against")
	send := func(cmd *cobra.Command, method, path string, query url.Values, contentType string, body []byte) ([]byte, error) {
		return nil, nil
	}
	root.AddCommand(cli.APICommand(send, output), &cobra.Command{Use: "hello", Short: "Greet locally", RunE: func(cmd *cobra.Command, args []string) error { return nil }})
	cli.Table{Name: "demo", Send: send, Output: output, Fill: map[string]func() (string, error){"app": func() (string, error) { return "my-app", nil }}, Resources: []cli.Resource{{
		Name: "article", Short: "Articles",
		Verbs: []cli.Verb{
			{Name: "get", Short: "Read articles", Method: http.MethodGet, Paths: []string{"/api/articles", "/api/articles/{id}"}},
			{Name: "create", Short: "Add an article", Method: http.MethodPost, Paths: []string{"/api/articles"}, Body: true},
			{Name: "cover", Short: "Upload a cover", Method: http.MethodPost, Paths: []string{"", "/api/articles/{id}/cover"}, Upload: true},
			{Name: "delete", Short: "Remove an article", Method: http.MethodDelete, Paths: []string{"", "/api/articles/{id}"}},
		},
		Children: []cli.Resource{{Name: "comment", Short: "Comments", Verbs: []cli.Verb{
			{Name: "get", Short: "Read comments", Method: http.MethodGet, Paths: []string{"", "/api/articles/{id}/comments", "/api/articles/{id}/comments/{comment}"}},
		}}},
	}, {
		Name: "setting", Short: "Settings of the app",
		Verbs: []cli.Verb{{Name: "get", Short: "Read the settings", Method: http.MethodGet, Paths: []string{"/api/app/{app}/settings"}}},
	}}}.Mount(root)
	get, _, _ := root.Find([]string{"article", "get"})
	get.Flags().Bool("stream", false, "print every event as it arrives")
	get.Annotations[cli.AnnotationMCPPage] = "article"
	cli.PutOutput[[]article](get)
	return root
}

type routeCall struct {
	method, path, authorization, contentType string
	query                                    url.Values
	body                                     []byte
}

func tableService(root *cobra.Command, operations ...mcp.Operation) (*echo.Echo, *[]routeCall, error) {
	auth := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(ctx echo.Context) error {
			if ctx.Request().Header.Get(echo.HeaderAuthorization) != "Bearer good" {
				return ctx.String(http.StatusUnauthorized, "Unauthorized")
			}
			return next(ctx)
		}
	}
	calls := &[]routeCall{}
	record := func(ctx echo.Context) {
		body, _ := io.ReadAll(ctx.Request().Body)
		*calls = append(*calls, routeCall{method: ctx.Request().Method, path: ctx.Request().URL.Path, authorization: ctx.Request().Header.Get(echo.HeaderAuthorization), contentType: ctx.Request().Header.Get(echo.HeaderContentType), query: ctx.QueryParams(), body: body})
	}
	e := echo.New()
	api := e.Group("/api", auth)
	api.GET("/articles", func(ctx echo.Context) error {
		record(ctx)
		return ctx.JSON(http.StatusOK, map[string]any{"data": []map[string]any{{"id": "a", "title": "Orion"}, {"id": "b", "title": "Atlas"}}, "request_id": "r1", "count": 2, "pagination": map[string]any{"total": 9}})
	})
	api.GET("/articles/:id", func(ctx echo.Context) error {
		record(ctx)
		if ctx.Param("id") == "locked" {
			return respond.Error(ctx, http.StatusForbidden, "no claim reaches article locked")
		}
		return respond.JSON(ctx, http.StatusOK, map[string]any{"id": ctx.Param("id"), "title": "Orion"}, 1)
	})
	api.POST("/articles", func(ctx echo.Context) error {
		record(ctx)
		return respond.JSON(ctx, http.StatusCreated, map[string]any{"id": "new"}, 1)
	})
	api.GET("/articles/:id/comments/:comment", func(ctx echo.Context) error {
		record(ctx)
		return respond.JSON(ctx, http.StatusOK, map[string]any{"id": ctx.Param("comment")}, 1)
	})
	err := endpoint.Mount(e, endpoint.Set{Service: "demo", Version: "v1", MCP: &mcp.Server{Name: "demo", Version: "v1", Operations: operations}, MCPAccess: []echo.MiddlewareFunc{auth}, CLI: root})
	return e, calls, err
}

func normalized(t *testing.T, value any) map[string]any {
	encoded, err := json.Marshal(value)
	assert.NoError(t, err)
	var decoded map[string]any
	assert.NoError(t, json.Unmarshal(encoded, &decoded))
	return decoded
}

func callTool(t *testing.T, e *echo.Echo, name string, arguments map[string]any) map[string]any {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": arguments}})
	return result(t, post(e, "good", "application/json", string(body)))
}

func TestRemoteServesEveryTableCommandAsTheLocalTool(t *testing.T) {
	root := tableTree()
	e, _, err := tableService(root)
	assert.NoError(t, err)
	served := map[string]map[string]any{}
	for _, tool := range result(t, post(e, "good", "application/json", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))["tools"].([]any) {
		served[tool.(map[string]any)["name"].(string)] = tool.(map[string]any)
	}
	routed := []string{"api", "article_get", "article_create", "article_cover", "article_delete", "article_comment_get", "setting_get"}
	assert.ElementsMatch(t, routed, keys(served), "every routed command and nothing else, hello stays in the cli")
	for _, tool := range cli.Tools(root, "/bin/demo") {
		remote, ok := served[tool.Name]
		if !ok {
			continue
		}
		local := normalized(t, tool)
		assert.Equal(t, local["description"], remote["description"], tool.Name)
		assert.Equal(t, local["annotations"], remote["annotations"], tool.Name)
		assert.Equal(t, local["outputSchema"], remote["outputSchema"], tool.Name)
		localMeta, _ := local["_meta"].(map[string]any)
		remoteMeta, _ := remote["_meta"].(map[string]any)
		assert.Equal(t, localMeta[mcp.MetaUserInteraction], remoteMeta[mcp.MetaUserInteraction], tool.Name)
		localSchema := local["inputSchema"].(map[string]any)
		remoteSchema := remote["inputSchema"].(map[string]any)
		properties := localSchema["properties"].(map[string]any)
		for name := range cli.TerminalFlags {
			delete(properties, name)
		}
		assert.Equal(t, properties, remoteSchema["properties"], tool.Name)
		assert.Equal(t, localSchema["required"], remoteSchema["required"], tool.Name)
	}
	assert.Equal(t, "ui://demo/article", served["article_get"]["_meta"].(map[string]any)["ui"].(map[string]any)["resourceUri"])
	assert.Equal(t, true, served["article_delete"]["_meta"].(map[string]any)[mcp.MetaUserInteraction])
	assert.Contains(t, served["article_create"]["description"], "confirm with the user")
	for _, name := range []string{"endpoint", "app", "config", "env", "global", "yes", "plan", "json", "raw", "plain", "format"} {
		assert.NotContains(t, served["article_get"]["inputSchema"].(map[string]any)["properties"], name)
	}
	for _, name := range []string{"article_id", "fields", "jq", "limit", "cursor", "query", "stream"} {
		assert.Contains(t, served["article_get"]["inputSchema"].(map[string]any)["properties"], name)
	}
}

func keys(values map[string]map[string]any) []string {
	var names []string
	for name := range values {
		names = append(names, name)
	}
	return names
}

func TestRemoteTableToolsCallTheRouteAsTheCaller(t *testing.T) {
	e, calls, err := tableService(tableTree())
	assert.NoError(t, err)

	called := callTool(t, e, "article_get", map[string]any{"article_id": "42", "query": []string{"status=draft"}, "limit": 5, "fields": []string{"id"}})
	assert.Nil(t, called["isError"], called)
	assert.Equal(t, map[string]any{"id": "42", "title": "Orion"}, called["structuredContent"].(map[string]any)["data"])
	assert.JSONEq(t, `{"id":"42"}`, called["content"].([]any)[0].(map[string]any)["text"].(string))
	last := (*calls)[len(*calls)-1]
	assert.Equal(t, http.MethodGet, last.method)
	assert.Equal(t, "/api/articles/42", last.path)
	assert.Equal(t, "Bearer good", last.authorization)
	assert.Equal(t, url.Values{"status": {"draft"}, "limit": {"5"}}, last.query)

	called = callTool(t, e, "article_create", map[string]any{"set": []string{"title=Orion", "tags=[\"news\"]"}, "data": `{"draft":true}`})
	assert.Nil(t, called["isError"], called)
	last = (*calls)[len(*calls)-1]
	assert.Equal(t, http.MethodPost, last.method)
	assert.Equal(t, cli.ContentTypeJSON, last.contentType)
	assert.JSONEq(t, `{"title":"Orion","tags":["news"],"draft":true}`, string(last.body))

	called = callTool(t, e, "article_comment_get", map[string]any{"comment_id": "a b", "comment": "c1"})
	assert.Nil(t, called["isError"], called)
	assert.Equal(t, "/api/articles/a b/comments/c1", (*calls)[len(*calls)-1].path)

	called = callTool(t, e, "article_get", map[string]any{"jq": ".[].title"})
	assert.Equal(t, "Orion\nAtlas\n2 of 9 rows, or narrow with fields or limit\n", called["content"].([]any)[0].(map[string]any)["text"])

	called = callTool(t, e, "api", map[string]any{"method": "get", "path": "/api/articles/7"})
	assert.Nil(t, called["isError"], called)
	assert.Equal(t, "/api/articles/7", (*calls)[len(*calls)-1].path)

	called = callTool(t, e, "article_get", map[string]any{"article_id": "locked"})
	assert.Equal(t, true, called["isError"])
	assert.Equal(t, "no claim reaches article locked", called["content"].([]any)[0].(map[string]any)["text"])
}

func TestRemoteTableToolsRefuseWhatOnlyTheCliCanDo(t *testing.T) {
	e, calls, err := tableService(tableTree())
	assert.NoError(t, err)
	for _, refused := range []struct {
		name      string
		arguments map[string]any
	}{
		{"article_get", map[string]any{"article_id": "1", "stream": true}},
		{"article_get", map[string]any{"article_id": "1", "json": true}},
		{"article_get", map[string]any{"article_id": "1", "endpoint": "https://elsewhere.example.test"}},
		{"article_get", map[string]any{"article_id": "1", "nope": "x"}},
		{"article_create", map[string]any{"set": []string{"title=@/etc/passwd"}}},
		{"article_create", map[string]any{"file": "/etc/passwd"}},
		{"article_cover", map[string]any{"article_id": "1", "upload": "./cover.png"}},
		{"article_delete", map[string]any{}},
		{"setting_get", map[string]any{}},
		{"api", map[string]any{"method": "POST", "path": "/mcp"}},
		{"api", map[string]any{"method": "FETCH", "path": "/api/articles"}},
	} {
		called := callTool(t, e, refused.name, refused.arguments)
		assert.Equal(t, true, called["isError"], "%s %v must be refused, got %v", refused.name, refused.arguments, called)
	}
	assert.Empty(t, *calls, "a refused call never reaches a route")
	called := callTool(t, e, "setting_get", map[string]any{})
	assert.Contains(t, called["content"].([]any)[0].(map[string]any)["text"], "needs app")
}

func TestEndpointMountRefusesTwoToolsOfOneName(t *testing.T) {
	duplicate := mcp.Define(mcp.Spec{Name: "article_get", Description: "Read one article", Method: http.MethodGet, Path: "/articles/:id", Access: mcp.AccessRead},
		func(ctx echo.Context, in struct{}) (article, error) { return article{}, nil })
	_, _, err := tableService(tableTree(), duplicate)
	assert.EqualError(t, err, "/mcp serves two tools called article_get")
}
