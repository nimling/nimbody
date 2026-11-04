package test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nimling/nimbody/mcp"
	"github.com/nimling/nimbody/respond"
	"github.com/stretchr/testify/assert"
)

func mcpSession(t *testing.T, server *mcp.Server, requests ...string) []map[string]any {
	out, in := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(context.Background(), strings.NewReader(strings.Join(requests, "\n")+"\n"), in)
		in.Close()
	}()
	var replies []map[string]any
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<24)
	for scanner.Scan() {
		var reply map[string]any
		assert.NoError(t, json.Unmarshal(scanner.Bytes(), &reply))
		replies = append(replies, reply)
	}
	assert.NoError(t, <-done)
	return replies
}

func TestMCPArgv(t *testing.T) {
	argv, err := mcp.Argv([]string{"claim", "replace"}, map[string]string{"set": "stringSlice", "yes": "bool"}, json.RawMessage(`{"args":["id"],"flags":{"set":["a=1","b=2"],"yes":true}}`))
	assert.NoError(t, err)
	assert.Equal(t, []string{"claim", "replace", "--set=a=1", "--set=b=2", "--yes=true", "id"}, argv)
	_, err = mcp.Argv([]string{"claim"}, map[string]string{}, json.RawMessage(`{"flags":{"nope":1}}`))
	assert.EqualError(t, err, "claim takes no flag --nope")
}

func TestMCPServeToolsResourcesAndProgress(t *testing.T) {
	server := &mcp.Server{Name: "sauth", Version: "v1", Tools: []mcp.Tool{{
		Name:        "claim_get",
		Description: "get a claim",
		InputSchema: map[string]any{"type": "object"},
		Run: func(ctx context.Context, arguments json.RawMessage, onStep func(respond.Step)) (string, bool) {
			now := time.Now().UTC()
			onStep(respond.Step{ID: "load", Label: "Load", Status: "running", StartedAt: now})
			onStep(respond.Step{ID: "load", Label: "Load", Status: "done", StartedAt: now, EndedAt: &now})
			return `[{"id":"1"}]`, false
		},
	}}}
	replies := mcpSession(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"ui://sauth/run"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"claim_get","arguments":{},"_meta":{"progressToken":"p1"}}}`,
	)
	byID := map[float64]map[string]any{}
	var notifications []map[string]any
	for _, reply := range replies {
		if id, ok := reply["id"].(float64); ok {
			byID[id] = reply
			continue
		}
		notifications = append(notifications, reply)
	}

	capabilities := byID[1]["result"].(map[string]any)["capabilities"].(map[string]any)
	assert.Contains(t, capabilities, "resources")

	tools := byID[2]["result"].(map[string]any)["tools"].([]any)
	assert.Len(t, tools, 2)
	first := tools[0].(map[string]any)
	assert.Equal(t, "claim_get", first["name"])
	assert.Equal(t, "ui://sauth/run", first["_meta"].(map[string]any)["ui"].(map[string]any)["resourceUri"])
	assert.Equal(t, "steps", tools[1].(map[string]any)["name"])

	resources := byID[3]["result"].(map[string]any)["resources"].([]any)
	assert.Equal(t, "text/html;profile=mcp-app", resources[0].(map[string]any)["mimeType"])

	contents := byID[4]["result"].(map[string]any)["contents"].([]any)
	html := contents[0].(map[string]any)["text"].(string)
	assert.Contains(t, html, "<title>sauth run</title>")
	assert.Contains(t, html, "ui/initialize")
	assert.NotContains(t, html, "__NAME__")

	result := byID[5]["result"].(map[string]any)
	assert.Nil(t, result["isError"])
	assert.Equal(t, `[{"id":"1"}]`, result["content"].([]any)[0].(map[string]any)["text"])

	assert.Len(t, notifications, 2)
	assert.Equal(t, "notifications/progress", notifications[0]["method"])
	params := notifications[1]["params"].(map[string]any)
	assert.Equal(t, "p1", params["progressToken"])
	assert.Equal(t, float64(2), params["progress"])
	assert.Contains(t, params["message"], "Load  done")

	after := mcpSession(t, server, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"steps","arguments":{}}}`)
	var run mcp.Run
	assert.NoError(t, json.Unmarshal([]byte(after[0]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)), &run))
	assert.Equal(t, "claim_get", run.Tool)
	assert.Equal(t, "done", run.Status)
	assert.Len(t, run.Steps, 1)
	assert.Equal(t, "done", run.Steps[0].Status)
}

func TestMCPServeUnknownToolAndResource(t *testing.T) {
	server := &mcp.Server{Name: "x", Version: "v"}
	replies := mcpSession(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"ui://x/other"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"prompts/list"}`,
	)
	assert.Equal(t, true, replies[0]["result"].(map[string]any)["isError"])
	assert.Equal(t, float64(-32002), replies[1]["error"].(map[string]any)["code"])
	assert.Equal(t, float64(-32601), replies[2]["error"].(map[string]any)["code"])
}

func TestMCPExecRunsCommandAndParsesSteps(t *testing.T) {
	text, failed := mcp.Exec(context.Background(), "sh", []string{"-c", `echo out; echo 'step {"id":"a","label":"A","status":"done","started_at":"2026-10-16T10:00:00Z","ended_at":"2026-10-16T10:00:01Z"}' >&2; echo note >&2`}, nil)
	assert.False(t, failed)
	assert.Equal(t, "out\nnote\n", text)
	var seen []respond.Step
	_, failed = mcp.Exec(context.Background(), "sh", []string{"-c", `echo 'step {"id":"a","label":"A","status":"running","started_at":"2026-10-16T10:00:00Z"}' >&2; exit 3`}, func(step respond.Step) { seen = append(seen, step) })
	assert.True(t, failed)
	assert.Len(t, seen, 1)
}

func TestMCPServePagesAndTheirAppTools(t *testing.T) {
	sign := mcp.Tool{
		Name:        "open_frame",
		Description: "sign a frame address",
		InputSchema: map[string]any{"type": "object"},
		Run: func(ctx context.Context, arguments json.RawMessage, onStep func(respond.Step)) (string, bool) {
			return "https://app.example.test/view/1?guest=t", false
		},
	}
	server := &mcp.Server{Name: "demo", Version: "v1", Pages: []mcp.Page{mcp.Frame("open", "open", []string{"https://app.example.test"}, &sign)}, Tools: []mcp.Tool{
		{Name: "view_open", Description: "open a view", InputSchema: map[string]any{"type": "object"}, Page: "open", Run: func(ctx context.Context, arguments json.RawMessage, onStep func(respond.Step)) (string, bool) {
			return `{"id":"1","name":"v","url":"https://app.example.test/view/1","frame":"https://app.example.test/view/1"}`, false
		}},
		{Name: "view_get", Description: "read a view", InputSchema: map[string]any{"type": "object"}, Run: func(ctx context.Context, arguments json.RawMessage, onStep func(respond.Step)) (string, bool) {
			return `{}`, false
		}},
	}}
	replies := mcpSession(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"ui://demo/open"}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"open_frame","arguments":{"frame":"https://app.example.test/view/1"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"steps","arguments":{}}}`,
	)
	byID := map[float64]map[string]any{}
	for _, reply := range replies {
		byID[reply["id"].(float64)] = reply
	}

	ui := func(tool any) map[string]any {
		return tool.(map[string]any)["_meta"].(map[string]any)["ui"].(map[string]any)
	}
	tools := map[string]any{}
	for _, tool := range byID[1]["result"].(map[string]any)["tools"].([]any) {
		tools[tool.(map[string]any)["name"].(string)] = tool
	}
	assert.Len(t, tools, 4)
	assert.Equal(t, "ui://demo/open", ui(tools["view_open"])["resourceUri"])
	assert.Equal(t, "ui://demo/run", ui(tools["view_get"])["resourceUri"])
	assert.Equal(t, []any{"app"}, ui(tools["open_frame"])["visibility"])

	resources := byID[2]["result"].(map[string]any)["resources"].([]any)
	assert.Len(t, resources, 2)
	assert.Equal(t, "ui://demo/run", resources[0].(map[string]any)["uri"])
	open := resources[1].(map[string]any)
	assert.Equal(t, "ui://demo/open", open["uri"])
	assert.Equal(t, "text/html;profile=mcp-app", open["mimeType"])
	assert.Equal(t, []any{"https://app.example.test"}, open["_meta"].(map[string]any)["ui"].(map[string]any)["csp"].(map[string]any)["frameDomains"])

	content := byID[3]["result"].(map[string]any)["contents"].([]any)[0].(map[string]any)
	html := content["text"].(string)
	assert.Contains(t, html, `const sign = "open_frame"`)
	assert.Contains(t, html, "<title>demo open</title>")
	assert.Contains(t, html, "ui/open-link")
	assert.NotContains(t, html, "__")
	assert.NotNil(t, content["_meta"].(map[string]any)["ui"].(map[string]any)["csp"])

	assert.Equal(t, "https://app.example.test/view/1?guest=t", byID[4]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"])
	var run mcp.Run
	assert.NoError(t, json.Unmarshal([]byte(byID[5]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)), &run))
	assert.Empty(t, run.Tool, "an app tool of a page never becomes the run the run page shows")
}

func TestMCPAnImageToolAnswersThePictureInline(t *testing.T) {
	folder := t.TempDir()
	png := folder + "/shot.png"
	picture := []byte("\x89PNG\r\n\x1a\nfake")
	assert.NoError(t, os.WriteFile(png, picture, 0o644))
	run := func(answer string) func(context.Context, json.RawMessage, func(respond.Step)) (string, bool) {
		return func(context.Context, json.RawMessage, func(respond.Step)) (string, bool) { return answer, false }
	}
	server := &mcp.Server{Name: "demo", Version: "1", Tools: []mcp.Tool{
		{Name: "shot", Image: "path", Run: run(png + "\n")},
		{Name: "shot_json", Image: "path", Run: run(`{"path":"` + png + `"}`)},
		{Name: "plain", Run: run(png)},
		{Name: "missing", Image: "path", Run: run(folder + "/gone.png")},
	}}
	replies := mcpSession(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shot","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"shot_json","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"plain","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"missing","arguments":{}}}`,
	)
	content := map[float64][]any{}
	for _, reply := range replies {
		content[reply["id"].(float64)] = reply["result"].(map[string]any)["content"].([]any)
	}
	for _, id := range []float64{1, 2} {
		if assert.Len(t, content[id], 2) {
			image := content[id][1].(map[string]any)
			assert.Equal(t, "image", image["type"])
			assert.Equal(t, "image/png", image["mimeType"])
			assert.Equal(t, base64.StdEncoding.EncodeToString(picture), image["data"])
		}
	}
	assert.Len(t, content[3], 1, "a tool that does not declare an image answers text only")
	assert.Len(t, content[4], 1, "a path that is not there answers the text it printed")
}
