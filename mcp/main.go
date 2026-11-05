package mcp

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nimling/nimbody/respond"
	"github.com/nimling/nimbody/shape"
	"github.com/nimling/nimbody/stream"
)

//go:embed page.html
var page string

//go:embed frame.html
var framePage string

const (
	ProtocolVersion = "2026-07-28"
	pageMIME        = "text/html;profile=mcp-app"
	stepsTool       = "steps"
	runPage         = "run"
)

type Flag struct {
	Name       string
	Kind       string
	Usage      string
	Positional bool
	Required   bool
}

type Arguments struct {
	Args  []string       `json:"args"`
	Flags map[string]any `json:"flags"`
}

type Tool struct {
	Name         string                                                                                         `json:"name"`
	Description  string                                                                                         `json:"description"`
	InputSchema  map[string]any                                                                                 `json:"inputSchema"`
	OutputSchema map[string]any                                                                                 `json:"outputSchema,omitempty"`
	Annotations  map[string]any                                                                                 `json:"annotations,omitempty"`
	Meta         map[string]any                                                                                 `json:"_meta,omitempty"`
	Page         string                                                                                         `json:"-"`
	Image        string                                                                                         `json:"-"`
	Run          func(ctx context.Context, arguments json.RawMessage, onStep func(respond.Step)) (string, bool) `json:"-"`
}

type Page struct {
	Name    string
	Title   string
	HTML    string
	Frames  []string
	Connect []string
	Tools   []Tool
}

type Run struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Status    string          `json:"status"`
	StartedAt time.Time       `json:"started_at"`
	EndedAt   *time.Time      `json:"ended_at,omitempty"`
	Steps     []respond.Step  `json:"steps"`
}

type Server struct {
	Name                string
	Version             string
	Instructions        string
	Tools               []Tool
	Pages               []Page
	Operations          []Operation
	AuthorizationServer string
	mu                  sync.Mutex
	run                 Run
}

type request struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type call struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Meta      struct {
		ProgressToken json.RawMessage `json:"progressToken"`
	} `json:"_meta"`
}

type content struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	Data     string    `json:"data,omitempty"`
	MimeType string    `json:"mimeType,omitempty"`
	Resource *resource `json:"resource,omitempty"`
}

type resource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType"`
	Blob     string `json:"blob"`
}

type callResult struct {
	Content           []content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

func Command(executable string, path []string, description string, flags []Flag) Tool {
	properties := map[string]any{}
	for _, flag := range flags {
		if flag.Positional {
			continue
		}
		switch {
		case flag.Kind == "bool":
			properties[flag.Name] = map[string]any{"type": "boolean", "description": flag.Usage}
		case strings.HasSuffix(flag.Kind, "Slice") || strings.HasSuffix(flag.Kind, "Array"):
			properties[flag.Name] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": flag.Usage}
		default:
			properties[flag.Name] = map[string]any{"type": "string", "description": flag.Usage}
		}
	}
	return Tool{
		Name:        strings.ReplaceAll(strings.Join(path, "_"), "-", "_"),
		Description: description,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"args":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "positional arguments, in the order the usage line names them"},
				"flags": map[string]any{"type": "object", "properties": properties, "additionalProperties": false},
			},
			"additionalProperties": false,
		},
		Run: func(ctx context.Context, arguments json.RawMessage, onStep func(respond.Step)) (string, bool) {
			argv, err := argvOf(path, flags, arguments)
			if err != nil {
				return err.Error(), true
			}
			return Exec(ctx, executable, argv, onStep)
		},
	}
}

func GetInputSchema(flags []Flag) map[string]any {
	properties := map[string]any{}
	var required []string
	for _, flag := range flags {
		property := map[string]any{"type": jsonKind(flag.Kind)}
		if many(flag.Kind) {
			property["type"] = "array"
			property["items"] = map[string]any{"type": jsonKind(strings.TrimSuffix(strings.TrimSuffix(flag.Kind, "Slice"), "Array"))}
		}
		if flag.Usage != "" {
			property["description"] = flag.Usage
		}
		properties[flag.Name] = property
		if flag.Required {
			required = append(required, flag.Name)
		}
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		sort.Strings(required)
		schema["required"] = required
	}
	return schema
}

func many(kind string) bool {
	return strings.HasSuffix(kind, "Slice") || strings.HasSuffix(kind, "Array")
}

func jsonKind(kind string) string {
	switch kind {
	case "bool", "boolean":
		return "boolean"
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "count", "integer":
		return "integer"
	case "float32", "float64", "number":
		return "number"
	}
	return "string"
}

func Argv(path []string, kinds map[string]string, raw json.RawMessage) ([]string, error) {
	flags := make([]Flag, 0, len(kinds))
	for name, kind := range kinds {
		flags = append(flags, Flag{Name: name, Kind: kind})
	}
	return argvOf(path, flags, raw)
}

func argvOf(path []string, flags []Flag, raw json.RawMessage) ([]string, error) {
	values := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, err
		}
	}
	declared := map[string]Flag{}
	for _, flag := range flags {
		declared[flag.Name] = flag
	}
	_, argsDeclared := declared["args"]
	_, flagsDeclared := declared["flags"]
	_, hasArgs := values["args"]
	_, hasFlags := values["flags"]
	if !argsDeclared && !flagsDeclared && (hasArgs || hasFlags) {
		var arguments Arguments
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, err
		}
		values = arguments.Flags
		for name, flag := range declared {
			if flag.Positional {
				delete(declared, name)
			}
		}
		argv, err := flagArgv(path, declared, values, "flag --")
		if err != nil {
			return nil, err
		}
		return append(argv, arguments.Args...), nil
	}
	argv, err := flagArgv(path, declared, values, "input ")
	if err != nil {
		return nil, err
	}
	var positional []string
	dash := false
	for _, flag := range flags {
		if !flag.Positional {
			continue
		}
		switch value := values[flag.Name].(type) {
		case nil:
		case []any:
			for _, item := range value {
				positional = append(positional, fmt.Sprint(item))
			}
		default:
			positional = append(positional, fmt.Sprint(value))
		}
	}
	for _, value := range positional {
		dash = dash || strings.HasPrefix(value, "-")
	}
	if dash {
		argv = append(argv, "--")
	}
	return append(argv, positional...), nil
}

func flagArgv(path []string, declared map[string]Flag, values map[string]any, unknown string) ([]string, error) {
	argv := append([]string{}, path...)
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		flag, known := declared[name]
		if !known {
			return nil, fmt.Errorf("%s takes no %s%s", strings.Join(path, " "), unknown, name)
		}
		if flag.Positional {
			continue
		}
		switch value := values[name].(type) {
		case bool:
			argv = append(argv, fmt.Sprintf("--%s=%t", name, value))
		case []any:
			for _, item := range value {
				argv = append(argv, fmt.Sprintf("--%s=%v", name, item))
			}
		case nil:
		default:
			argv = append(argv, fmt.Sprintf("--%s=%v", name, value))
		}
	}
	return argv, nil
}

func Exec(ctx context.Context, executable string, argv []string, onStep func(respond.Step)) (string, bool) {
	var out, rest bytes.Buffer
	run := exec.CommandContext(ctx, executable, argv...)
	run.Env = append(os.Environ(), "NO_COLOR=1", stream.EnvMCP+"=1")
	run.Stdout = &out
	errPipe, err := run.StderrPipe()
	if err != nil {
		return err.Error(), true
	}
	if err := run.Start(); err != nil {
		return err.Error(), true
	}
	lines := bufio.NewScanner(errPipe)
	lines.Buffer(make([]byte, 0, 1<<16), math.MaxInt)
	for lines.Scan() {
		if step, ok := stream.Parse(lines.Text()); ok {
			if onStep != nil {
				onStep(step)
			}
			continue
		}
		rest.WriteString(lines.Text())
		rest.WriteByte('\n')
	}
	err = run.Wait()
	out.Write(rest.Bytes())
	result := out.String()
	if result == "" {
		result = "(no output)"
	}
	return result, err != nil
}

func (s *Server) pageURI(name string) string {
	if name == "" {
		name = runPage
	}
	return "ui://" + s.Name + "/" + name
}

func Frame(name string, title string, frames []string, sign *Tool) Page {
	signName := ""
	var tools []Tool
	if sign != nil {
		signName = sign.Name
		tools = append(tools, *sign)
	}
	html := strings.NewReplacer("__TITLE__", title, "__SIGN__", signName).Replace(framePage)
	return Page{Name: name, Title: title, HTML: html, Frames: frames, Tools: tools}
}

func (p Page) meta() map[string]any {
	ui := map[string]any{"prefersBorder": false}
	csp := map[string]any{}
	if len(p.Frames) > 0 {
		csp["frameDomains"] = p.Frames
	}
	if len(p.Connect) > 0 {
		csp["connectDomains"] = p.Connect
	}
	if len(csp) > 0 {
		ui["csp"] = csp
	}
	return map[string]any{"ui": ui}
}

func (s *Server) pages() []Page {
	pages := []Page{{Name: runPage, Title: "run", HTML: page}}
	pages = append(pages, s.Pages...)
	for i := range pages {
		pages[i].HTML = strings.NewReplacer("__NAME__", s.Name, "__VERSION__", s.Version).Replace(pages[i].HTML)
	}
	return pages
}

func (s *Server) tools() []Tool {
	tools := make([]Tool, 0, len(s.Tools)+1)
	for _, tool := range s.Tools {
		tool.Meta = withMeta(tool.Meta, "ui", map[string]any{"resourceUri": s.pageURI(tool.Page)})
		tools = append(tools, tool)
	}
	for _, page := range s.Pages {
		for _, tool := range page.Tools {
			tool.Meta = map[string]any{"ui": map[string]any{"visibility": []string{"app"}}}
			tools = append(tools, tool)
		}
	}
	tools = append(tools, Tool{
		Name:        stepsTool,
		Description: "The steps of the tool call running now, for the run page",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		Meta:        map[string]any{"ui": map[string]any{"visibility": []string{"app"}}},
	})
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
}

func (s *Server) begin(name string, arguments json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.run = Run{Tool: name, Arguments: arguments, Status: "running", StartedAt: time.Now().UTC(), Steps: []respond.Step{}}
}

func (s *Server) step(step respond.Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, row := range s.run.Steps {
		if row.ID == step.ID {
			s.run.Steps[i] = step
			return
		}
	}
	s.run.Steps = append(s.run.Steps, step)
}

func (s *Server) end(failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.run.EndedAt = &now
	s.run.Status = "done"
	if failed {
		s.run.Status = "failed"
	}
}

func (s *Server) current() Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run
}

func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	tools := s.tools()
	pages := s.pages()

	var writing sync.Mutex
	var pending sync.WaitGroup
	write := func(body map[string]any) {
		body["jsonrpc"] = "2.0"
		line, _ := json.Marshal(body)
		writing.Lock()
		defer writing.Unlock()
		fmt.Fprintln(out, string(line))
	}
	reply := func(id json.RawMessage, body map[string]any) {
		body["id"] = id
		write(body)
	}
	fail := func(id json.RawMessage, code int, message string) {
		reply(id, map[string]any{"error": map[string]any{"code": code, "message": message}})
	}

	lines := bufio.NewScanner(in)
	lines.Buffer(make([]byte, 0, 1<<16), 1<<24)
	for lines.Scan() {
		if strings.TrimSpace(lines.Text()) == "" {
			continue
		}
		var req request
		if err := json.Unmarshal(lines.Bytes(), &req); err != nil {
			fail(json.RawMessage("null"), -32700, "parse error")
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		if req.Method != "tools/call" {
			reply(req.ID, s.answer(req, tools, pages))
			continue
		}
		var c call
		if err := json.Unmarshal(req.Params, &c); err != nil {
			reply(req.ID, map[string]any{"result": callResult{Content: []content{{Type: "text", Text: err.Error()}}, IsError: true}})
			continue
		}
		if c.Name == stepsTool {
			body, _ := json.Marshal(s.current())
			reply(req.ID, map[string]any{"result": callResult{Content: []content{{Type: "text", Text: string(body)}}}})
			continue
		}
		tool, app, found := s.tool(c.Name)
		if !found {
			reply(req.ID, map[string]any{"result": callResult{Content: []content{{Type: "text", Text: "no tool is called " + c.Name}}, IsError: true}})
			continue
		}
		pending.Add(1)
		if app {
			go func(req request, c call, tool Tool) {
				defer pending.Done()
				result, failed := tool.Run(ctx, c.Arguments, nil)
				reply(req.ID, map[string]any{"result": callResult{Content: []content{{Type: "text", Text: result}}, IsError: failed}})
			}(req, c, tool)
			continue
		}
		go func(req request, c call, tool Tool) {
			defer pending.Done()
			s.begin(c.Name, c.Arguments)
			seen := 0
			onStep := func(step respond.Step) {
				s.step(step)
				if len(c.Meta.ProgressToken) == 0 {
					return
				}
				seen++
				write(progress(c.Meta.ProgressToken, seen, step))
			}
			result, failed := tool.Run(ctx, c.Arguments, onStep)
			s.end(failed)
			reply(req.ID, map[string]any{"result": callResult{Content: shown(result, failed, tool.Image), StructuredContent: structuredOf(tool, result, failed, c.Arguments), IsError: failed}})
		}(req, c, tool)
	}
	pending.Wait()
	return lines.Err()
}

func progress(token json.RawMessage, seen int, step respond.Step) map[string]any {
	return map[string]any{"method": "notifications/progress", "params": map[string]any{
		"progressToken": token,
		"progress":      seen,
		"message":       stream.Line(step),
	}}
}

func (s *Server) answer(req request, tools []Tool, pages []Page) map[string]any {
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		if params.ProtocolVersion == "" {
			params.ProtocolVersion = ProtocolVersion
		}
		result := map[string]any{
			"protocolVersion": params.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}, "resources": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		}
		if s.Instructions != "" {
			result["instructions"] = s.Instructions
		}
		return map[string]any{"result": result}
	case "ping":
		return map[string]any{"result": map[string]any{}}
	case "tools/list":
		return map[string]any{"result": map[string]any{"tools": tools}}
	case "resources/list":
		resources := make([]map[string]any, 0, len(pages))
		for _, p := range pages {
			resources = append(resources, map[string]any{
				"uri":      s.pageURI(p.Name),
				"name":     p.Name,
				"title":    s.Name + " " + p.Title,
				"mimeType": pageMIME,
				"_meta":    p.meta(),
			})
		}
		return map[string]any{"result": map[string]any{"resources": resources}}
	case "resources/templates/list":
		return map[string]any{"result": map[string]any{"resourceTemplates": []any{}}}
	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(req.Params, &params)
		for _, p := range pages {
			if s.pageURI(p.Name) == params.URI {
				return map[string]any{"result": map[string]any{"contents": []map[string]any{{
					"uri":      params.URI,
					"mimeType": pageMIME,
					"text":     p.HTML,
					"_meta":    p.meta(),
				}}}}
			}
		}
		return map[string]any{"error": map[string]any{"code": -32002, "message": "no resource is " + params.URI}}
	}
	return map[string]any{"error": map[string]any{"code": -32601, "message": "method not found: " + req.Method}}
}

func (s *Server) tool(name string) (Tool, bool, bool) {
	for _, tool := range s.Tools {
		if tool.Name == name {
			return tool, false, true
		}
	}
	for _, page := range s.Pages {
		for _, tool := range page.Tools {
			if tool.Name == name {
				return tool, true, true
			}
		}
	}
	return Tool{}, false, false
}

const (
	AccessRead        = "read"
	AccessWrite       = "write"
	AccessDestructive = "destructive"
)

const confirmNote = "\nChanges data: confirm with the user before calling."

func Annotate(tool Tool, access string) Tool {
	switch access {
	case AccessRead:
		tool.Annotations = map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": true}
	case AccessDestructive:
		tool.Annotations = map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": true}
		tool.Meta = withMeta(tool.Meta, MetaUserInteraction, true)
		tool.Description += confirmNote
	default:
		tool.Annotations = map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": true}
		tool.Description += confirmNote
	}
	return tool
}

const MetaUserInteraction = "anthropic/requiresUserInteraction"

func withMeta(meta map[string]any, key string, value any) map[string]any {
	merged := make(map[string]any, len(meta)+1)
	for name, existing := range meta {
		merged[name] = existing
	}
	merged[key] = value
	return merged
}

func structuredOf(tool Tool, result string, failed bool, arguments json.RawMessage) any {
	if failed || tool.OutputSchema == nil {
		return nil
	}
	var values map[string]any
	_ = json.Unmarshal(arguments, &values)
	if nested, ok := values["flags"].(map[string]any); ok {
		values = nested
	}
	for name := range values {
		if _, shaping := shape.Schema()[name]; shaping || name == "raw" || name == "plain" || name == "format" {
			return nil
		}
	}
	var data any
	if json.NewDecoder(strings.NewReader(result)).Decode(&data) != nil {
		return nil
	}
	return structured{Data: data}
}

const DescriptionLimit = 4096

func Describe(description, example string) string {
	if example != "" {
		description += "\nexample: " + example
	}
	if runes := []rune(description); len(runes) > DescriptionLimit-len(confirmNote) {
		description = string(runes[:DescriptionLimit-len(confirmNote)-1]) + "…"
	}
	return description
}
