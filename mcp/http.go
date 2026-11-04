package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody/respond"
	"github.com/nimling/nimbody/shape"
)

const (
	MountPath            = "/mcp"
	ResourceMetadataPath = "/.well-known/oauth-protected-resource"
	HeaderProtocol       = "MCP-Protocol-Version"
)

type ResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceName           string   `json:"resource_name,omitempty"`
}

type paging struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
}

type structured struct {
	Data any `json:"data"`
}

func base(ctx echo.Context) string {
	return ctx.Scheme() + "://" + ctx.Request().Host
}

func (s *Server) Mount(e *echo.Echo, middleware ...echo.MiddlewareFunc) {
	e.GET(ResourceMetadataPath, s.metadata)
	e.GET(ResourceMetadataPath+MountPath, s.metadata)
	group := e.Group(MountPath, append([]echo.MiddlewareFunc{challenge}, middleware...)...)
	group.POST("", s.handle)
	group.GET("", func(ctx echo.Context) error {
		return ctx.NoContent(http.StatusMethodNotAllowed)
	})
}

func (s *Server) metadata(ctx echo.Context) error {
	body, err := json.Marshal(ResourceMetadata{
		Resource:               base(ctx) + MountPath,
		AuthorizationServers:   []string{s.AuthorizationServer},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           s.Name,
	})
	if err != nil {
		return err
	}
	return ctx.Blob(http.StatusOK, echo.MIMEApplicationJSON, body)
}

func challenge(next echo.HandlerFunc) echo.HandlerFunc {
	return func(ctx echo.Context) error {
		header := `Bearer resource_metadata="` + base(ctx) + ResourceMetadataPath + MountPath + `"`
		res := ctx.Response()
		res.Before(func() {
			if res.Status == http.StatusUnauthorized {
				res.Header().Set(echo.HeaderWWWAuthenticate, header)
			}
		})
		if !strings.HasPrefix(ctx.Request().Header.Get(echo.HeaderAuthorization), "Bearer ") {
			return ctx.String(http.StatusUnauthorized, "Unauthorized")
		}
		return next(ctx)
	}
}

func (s *Server) remoteTools() []Tool {
	tools := make([]Tool, 0, len(s.Operations))
	for _, operation := range s.Operations {
		tool := operation.Tool()
		properties, _ := tool.InputSchema["properties"].(map[string]any)
		extended := make(map[string]any, len(properties)+6)
		for key, value := range properties {
			extended[key] = value
		}
		for key, value := range shape.Schema() {
			if _, taken := extended[key]; !taken {
				extended[key] = value
			}
		}
		if operation.Access == AccessRead {
			for key, value := range map[string]any{
				"limit":  map[string]any{"type": "integer", "description": "rows per page"},
				"cursor": map[string]any{"type": "string", "description": "page cursor from a truncated result"},
			} {
				if _, taken := extended[key]; !taken {
					extended[key] = value
				}
			}
		}
		schema := map[string]any{"type": "object", "properties": extended}
		if required, ok := tool.InputSchema["required"]; ok {
			schema["required"] = required
		}
		tool.InputSchema = schema
		if tool.Page != "" {
			tool.Meta = map[string]any{"ui": map[string]any{"resourceUri": s.pageURI(tool.Page)}}
		}
		tools = append(tools, tool)
	}
	for _, page := range s.Pages {
		for _, tool := range page.Tools {
			tool.Meta = map[string]any{"ui": map[string]any{"visibility": []string{"app"}}}
			tools = append(tools, tool)
		}
	}
	return tools
}

func (s *Server) remotePages() []Page {
	pages := append([]Page{}, s.Pages...)
	for i := range pages {
		pages[i].HTML = strings.NewReplacer("__NAME__", s.Name, "__VERSION__", s.Version).Replace(pages[i].HTML)
	}
	return pages
}

func reply(ctx echo.Context, id json.RawMessage, body map[string]any) error {
	body["jsonrpc"] = "2.0"
	body["id"] = id
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return ctx.Blob(http.StatusOK, echo.MIMEApplicationJSON, encoded)
}

func (s *Server) handle(ctx echo.Context) error {
	var req request
	if err := json.NewDecoder(ctx.Request().Body).Decode(&req); err != nil {
		return reply(ctx, json.RawMessage("null"), map[string]any{"error": map[string]any{"code": -32700, "message": "parse error"}})
	}
	if len(req.ID) == 0 || req.Method == "" {
		return ctx.NoContent(http.StatusAccepted)
	}
	if req.Method != "tools/call" {
		return reply(ctx, req.ID, s.answer(req, s.remoteTools(), s.remotePages()))
	}
	var c call
	if err := json.Unmarshal(req.Params, &c); err != nil {
		return reply(ctx, req.ID, map[string]any{"result": callResult{Content: []content{{Type: "text", Text: err.Error()}}, IsError: true}})
	}
	operation, found := s.operation(c.Name)
	if !found {
		tool, _, app := s.tool(c.Name)
		if !app {
			return reply(ctx, req.ID, map[string]any{"result": callResult{Content: []content{{Type: "text", Text: "no tool is called " + c.Name}}, IsError: true}})
		}
		result, failed := tool.Run(ctx.Request().Context(), c.Arguments, nil)
		return reply(ctx, req.ID, map[string]any{"result": callResult{Content: []content{{Type: "text", Text: result}}, IsError: failed}})
	}

	res := ctx.Response()
	streaming := len(c.Meta.ProgressToken) > 0 && strings.Contains(ctx.Request().Header.Get(echo.HeaderAccept), respond.MIMEEventStream)
	seen := 0
	frame := func(body map[string]any) {
		body["jsonrpc"] = "2.0"
		encoded, _ := json.Marshal(body)
		if !res.Committed {
			res.Header().Set(echo.HeaderContentType, respond.MIMEEventStream)
			res.Header().Set("Cache-Control", "no-cache")
			res.Header().Set("X-Accel-Buffering", "no")
			res.WriteHeader(http.StatusOK)
		}
		fmt.Fprintf(res, "event: message\ndata: %s\n\n", encoded)
		res.Flush()
	}
	onStep := func(step respond.Step) {
		if streaming {
			seen++
			frame(progress(c.Meta.ProgressToken, seen, step))
		}
	}
	result := s.invoke(ctx, operation, c.Arguments, onStep)
	if streaming && res.Committed {
		frame(map[string]any{"id": req.ID, "result": result})
		return nil
	}
	return reply(ctx, req.ID, map[string]any{"result": result})
}

func (s *Server) operation(name string) (Operation, bool) {
	for _, operation := range s.Operations {
		if operation.Name == name {
			return operation, true
		}
	}
	return Operation{}, false
}

func (s *Server) invoke(ctx echo.Context, operation Operation, arguments json.RawMessage, onStep func(respond.Step)) callResult {
	var shaper shape.Shape
	var page paging
	values := map[string]any{}
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &values); err != nil {
			return callResult{Content: []content{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		_ = json.Unmarshal(arguments, &shaper)
		_ = json.Unmarshal(arguments, &page)
	}
	for _, input := range operation.Inputs {
		switch input.Name {
		case "fields":
			shaper.Fields = nil
		case "sort":
			shaper.Sort = ""
		case "jq":
			shaper.JQ = ""
		case "template":
			shaper.Template = ""
		case "limit":
			page.Limit = 0
		case "cursor":
			page.Cursor = ""
		}
	}
	if strings.HasPrefix(shaper.Template, "@") {
		return callResult{Content: []content{{Type: "text", Text: "a template file is read by the cli only, pass the template itself"}}, IsError: true}
	}

	value, total, err := s.dispatch(ctx, operation, arguments, values, page, onStep)
	var rejected *echo.HTTPError
	switch {
	case errors.As(err, &rejected):
		return callResult{Content: []content{{Type: "text", Text: message(rejected)}}, IsError: true}
	case err != nil:
		ctx.Logger().Error(operation.Name + ": " + err.Error())
		return callResult{Content: []content{{Type: "text", Text: "the server failed on " + operation.Name}}, IsError: true}
	}

	decoded, err := shape.Decode(value)
	if err != nil {
		return callResult{Content: []content{{Type: "text", Text: err.Error()}}, IsError: true}
	}
	selected, _, err := shaper.Select(decoded)
	if err != nil {
		return callResult{Content: []content{{Type: "text", Text: err.Error()}}, IsError: true}
	}
	var out bytes.Buffer
	if shaper.Text() {
		if err := shaper.Write(&out, selected); err != nil {
			return callResult{Content: []content{{Type: "text", Text: err.Error()}}, IsError: true}
		}
	} else {
		encoded, _ := json.MarshalIndent(selected, "", "  ")
		out.Write(encoded)
		out.WriteByte('\n')
	}
	if rows, isList := decoded.([]any); isList && total > len(rows) {
		envelope := respond.ReturnObject{}
		envelope.WithPagination(ctx, len(rows), total)
		line := strconv.Itoa(len(rows)) + " of " + strconv.Itoa(total) + " rows"
		if envelope.Pagination != nil && envelope.Pagination.Next != nil {
			if link, err := url.Parse(*envelope.Pagination.Next); err == nil && link.Query().Get("cursor") != "" {
				line += ", next page with cursor " + link.Query().Get("cursor")
			}
		}
		out.WriteString(line + ", or narrow with fields or limit\n")
	}
	return callResult{Content: []content{{Type: "text", Text: out.String()}}, StructuredContent: structured{Data: value}}
}

func (s *Server) dispatch(ctx echo.Context, operation Operation, arguments json.RawMessage, values map[string]any, page paging, onStep func(respond.Step)) (any, int, error) {
	path := operation.Path
	var names, params []string
	query := url.Values{}
	for _, input := range operation.Inputs {
		value, present := values[input.Name]
		if !present || value == nil {
			continue
		}
		switch input.In {
		case InPath:
			path = strings.Replace(path, ":"+input.Param, url.PathEscape(shape.Cell(value)), 1)
			names = append(names, input.Param)
			params = append(params, shape.Cell(value))
		case InQuery:
			if items, many := value.([]any); many {
				for _, item := range items {
					query.Add(input.Param, shape.Cell(item))
				}
				continue
			}
			query.Set(input.Param, shape.Cell(value))
		}
	}
	if page.Limit > 0 {
		query.Set("limit", strconv.Itoa(page.Limit))
	}
	if page.Cursor != "" {
		query.Set("cursor", page.Cursor)
	}

	request, response, route := ctx.Request(), ctx.Response(), ctx.Path()
	savedNames, savedParams := ctx.ParamNames(), ctx.ParamValues()
	cached := ctx.QueryParams()
	savedQuery := url.Values{}
	for key, value := range cached {
		savedQuery[key] = value
	}
	clone := request.Clone(request.Context())
	clone.Method = operation.Method
	clone.URL = &url.URL{Path: path, RawQuery: query.Encode()}
	clone.Body = http.NoBody
	clone.Header.Set(echo.HeaderAccept, respond.MIMEEventStream)
	recorder := &capture{header: http.Header{}, onStep: onStep}
	ctx.SetRequest(clone)
	ctx.SetResponse(echo.NewResponse(recorder, ctx.Echo()))
	ctx.SetPath(operation.Path)
	ctx.SetParamNames(names...)
	ctx.SetParamValues(params...)
	clear(cached)
	for key, value := range query {
		cached[key] = value
	}
	defer func() {
		ctx.SetRequest(request)
		ctx.SetResponse(response)
		ctx.SetPath(route)
		ctx.SetParamNames(savedNames...)
		ctx.SetParamValues(savedParams...)
		clear(cached)
		for key, value := range savedQuery {
			cached[key] = value
		}
	}()

	var value any
	var total int
	var err error
	reached := false
	handler := func(inner echo.Context) error {
		reached = true
		value, total, err = operation.call(inner, arguments)
		return nil
	}
	for i := len(operation.Middleware) - 1; i >= 0; i-- {
		handler = operation.Middleware[i](handler)
	}
	if failed := handler(ctx); !reached {
		if failed != nil {
			return nil, 0, failed
		}
		return nil, 0, echo.NewHTTPError(recorder.code(), recorder.message())
	}
	return value, total, err
}

type capture struct {
	header http.Header
	status int
	body   bytes.Buffer
	frames bytes.Buffer
	result *respond.ReturnResult
	onStep func(respond.Step)
}

func (c *capture) Header() http.Header { return c.header }

func (c *capture) WriteHeader(status int) { c.status = status }

func (c *capture) Flush() {}

func (c *capture) Write(body []byte) (int, error) {
	if !strings.HasPrefix(c.header.Get(echo.HeaderContentType), respond.MIMEEventStream) {
		return c.body.Write(body)
	}
	c.frames.Write(body)
	for {
		raw, rest, complete := bytes.Cut(c.frames.Bytes(), []byte("\n\n"))
		if !complete {
			return len(body), nil
		}
		c.frame(string(raw))
		remaining := append([]byte{}, rest...)
		c.frames.Reset()
		c.frames.Write(remaining)
	}
}

func (c *capture) frame(raw string) {
	var event, data string
	for _, line := range strings.Split(raw, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data += strings.TrimPrefix(line, "data: ")
		}
	}
	switch event {
	case "step":
		var step respond.Step
		if json.Unmarshal([]byte(data), &step) == nil && c.onStep != nil {
			c.onStep(step)
		}
	case "result":
		var result respond.ReturnResult
		if json.Unmarshal([]byte(data), &result) == nil {
			c.result = &result
		}
	}
}

func (c *capture) code() int {
	switch {
	case c.result != nil:
		return c.result.Status
	case c.status != 0:
		return c.status
	}
	return http.StatusForbidden
}

func (c *capture) message() string {
	if c.result != nil && c.result.Body.Error != nil {
		return c.result.Body.Error.Message
	}
	if text := strings.TrimSpace(c.body.String()); text != "" {
		return text
	}
	return http.StatusText(c.code())
}
