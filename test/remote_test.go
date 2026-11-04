package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody/cli"
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
