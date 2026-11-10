package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/nimling/nimbody/mcp"
	"github.com/nimling/nimbody/stream"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const AnnotationMCP = "mcp"

const AnnotationMCPPage = "mcp-page"

const AnnotationMCPImage = "mcp-image"

const AnnotationMCPInput = "mcp-input"

const AnnotationMCPOutput = "mcp-output"

const AnnotationMCPInstructions = "mcp-instructions"

const AnnotationSecret = "secret"

const InputTyped = "typed"

const AccessNone = "none"

var Tooling = map[string]bool{
	"mcp": true, "plugin": true, "skill": true, "completion": true, "config": true,
	"help": true, "version": true, "token": true, "login": true, "logout": true,
	"terraform": true,
}

var readVerbs = map[string]bool{"get": true, "list": true, "status": true, "show": true, "read": true, "check": true, "stat": true}

var destructiveVerbs = map[string]bool{"delete": true, "remove": true, "destroy": true, "revoke": true, "drop": true}

func Access(cmd *cobra.Command) string {
	for current := cmd; current != nil; current = current.Parent() {
		if access, ok := current.Annotations[AnnotationMCP]; ok {
			return access
		}
	}
	switch {
	case readVerbs[cmd.Name()]:
		return mcp.AccessRead
	case destructiveVerbs[cmd.Name()]:
		return mcp.AccessDestructive
	default:
		return mcp.AccessWrite
	}
}

func Tools(root *cobra.Command, executable string) []mcp.Tool {
	var tools []mcp.Tool
	var walk func(cmd *cobra.Command, path []string)
	walk = func(cmd *cobra.Command, path []string) {
		if cmd.Hidden || (len(path) > 0 && Tooling[path[0]]) {
			return
		}
		if cmd.HasAvailableSubCommands() {
			for _, child := range cmd.Commands() {
				if child.IsAvailableCommand() {
					walk(child, append(append([]string{}, path...), child.Name()))
				}
			}
			if !cmd.Runnable() {
				return
			}
		}
		if !cmd.Runnable() || len(path) == 0 {
			return
		}
		access := Access(cmd)
		if access == AccessNone {
			return
		}
		secrets := secretFlags(root)
		typed := annotation(cmd, AnnotationMCPInput) == InputTyped
		var flags []mcp.Flag
		names := map[string]bool{}
		add := func(flag *pflag.Flag) {
			if flag.Hidden || flag.Name == "help" || flag.Name == "version" || flag.Name == FlagStream || secrets[flag.Name] || len(flag.Annotations[AnnotationSecret]) > 0 {
				return
			}
			required := len(flag.Annotations[cobra.BashCompOneRequiredFlag]) > 0 && flag.Annotations[cobra.BashCompOneRequiredFlag][0] == "true"
			flags = append(flags, mcp.Flag{Name: flag.Name, Kind: flag.Value.Type(), Usage: flag.Usage, Required: required})
			names[flag.Name] = true
		}
		cmd.LocalFlags().VisitAll(add)
		cmd.InheritedFlags().VisitAll(add)
		if typed {
			flags = append(positionals(cmd, names), flags...)
		}
		description := mcp.Describe(cmd.CommandPath()+": "+cmd.Short+"\nusage: "+cmd.UseLine(), cmd.Example)
		tool := mcp.Command(executable, path, description, flags)
		if typed {
			tool.InputSchema = mcp.GetInputSchema(flags)
		}
		if schema := cmd.Annotations[AnnotationMCPOutput]; schema != "" {
			_ = json.Unmarshal([]byte(schema), &tool.OutputSchema)
		}
		tool = mcp.Annotate(tool, access)
		tool.Page = cmd.Annotations[AnnotationMCPPage]
		tool.Image = cmd.Annotations[AnnotationMCPImage]
		tools = append(tools, tool)
	}
	walk(root, nil)
	return tools
}

func annotation(cmd *cobra.Command, key string) string {
	for current := cmd; current != nil; current = current.Parent() {
		if value, ok := current.Annotations[key]; ok {
			return value
		}
	}
	return ""
}

func secretFlags(root *cobra.Command) map[string]bool {
	secrets := map[string]bool{}
	for _, name := range strings.Split(root.Annotations[AnnotationSecret], ",") {
		if name != "" {
			secrets[name] = true
		}
	}
	return secrets
}

func positionals(cmd *cobra.Command, taken map[string]bool) []mcp.Flag {
	var flags []mcp.Flag
	fields := strings.Fields(cmd.Use)
	for _, field := range fields[min(1, len(fields)):] {
		variadic := strings.HasSuffix(field, "...")
		field = strings.TrimSuffix(field, "...")
		required := strings.HasPrefix(field, "<")
		if !required && !strings.HasPrefix(field, "[") {
			continue
		}
		inner := strings.Trim(field, "<>[]")
		variadic = variadic || strings.HasSuffix(inner, "...")
		name := strings.Trim(strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
				return r
			case r >= 'A' && r <= 'Z':
				return r + 'a' - 'A'
			}
			return '_'
		}, strings.TrimSuffix(inner, "...")), "_")
		if name == "" || name == "flags" {
			continue
		}
		if name == "id" && cmd.HasParent() && cmd.Parent().HasParent() {
			name = strings.ReplaceAll(cmd.Parent().Name(), "-", "_") + "_id"
		}
		if taken[name] {
			name = "arg_" + name
		}
		taken[name] = true
		kind := "string"
		if variadic {
			kind = "stringSlice"
		}
		flags = append(flags, mcp.Flag{Name: name, Kind: kind, Usage: "positional " + strings.TrimSuffix(inner, "..."), Positional: true, Required: required})
	}
	return flags
}

func PutOutput[Out any](cmd *cobra.Command) *cobra.Command {
	schema, _ := json.Marshal(mcp.GetOutputSchema(reflect.TypeFor[Out]()))
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[AnnotationMCPOutput] = string(schema)
	return cmd
}

func GetInstructions(root *cobra.Command, name string) string {
	if text := root.Annotations[AnnotationMCPInstructions]; text != "" {
		return text
	}
	text := "The " + name + " tools"
	if root.Short != "" {
		text += ": " + strings.TrimSuffix(root.Short, ".")
	}
	return text + ". Reach for them to read or change what " + name + " manages. Every tool runs as the signed in identity and reaches only what it may; a tool that changes data needs the user's confirmation first."
}

func Serve(ctx context.Context, root *cobra.Command, name, version string, in io.Reader, out io.Writer, pages ...mcp.Page) error {
	server, err := localServer(root, name, version, pages)
	if err != nil {
		return err
	}
	return server.Serve(ctx, in, out)
}

func localServer(root *cobra.Command, name, version string, pages []mcp.Page) (*mcp.Server, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &mcp.Server{Name: name, Version: version, Instructions: GetInstructions(root, name), Tools: Tools(root, executable), Pages: pages}, nil
}

type Remote func(ctx context.Context) (endpoint string, token string, err error)

var Bridge Remote

func MCPCommand(root *cobra.Command, name string, version func() string, pages ...func() mcp.Page) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve every command as a tool over the model context protocol on stdio",
		Long: "Serve every command as a tool over the model context protocol on stdio. With an endpoint and a\n" +
			"login, it bridges stdio to the service's own /mcp endpoint with the stored token, so the tools are\n" +
			"the backend's and reach exactly what the login's claims reach; a command of this binary the backend\n" +
			"serves no tool for stays a tool that runs here. Without one, a tool runs this binary\n" +
			"with the arguments and flags it is given, so it reads the same configuration as a terminal in the\n" +
			"directory the host was started in. Each tool reports its steps as progress, and a host that renders\n" +
			"MCP Apps shows the run live on ui://" + name + "/run, or the result on the page a command names.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			built := make([]mcp.Page, 0, len(pages))
			for _, page := range pages {
				built = append(built, page())
			}
			server, err := localServer(root, name, version(), built)
			if err != nil {
				return err
			}
			if Bridge != nil {
				endpoint, token, err := Bridge(cmd.Context())
				if err != nil {
					return err
				}
				if endpoint != "" && token != "" {
					return Relay(cmd.Context(), name, Bridge, cmd.InOrStdin(), cmd.OutOrStdout(), server)
				}
			}
			return server.Serve(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

type relayRequest struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
	} `json:"params"`
}

type relayError struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type relayList struct {
	method string
	field  string
	key    string
}

var (
	relayTools     = relayList{method: "tools/list", field: "tools", key: "name"}
	relayResources = relayList{method: "resources/list", field: "resources", key: "uri"}
)

type relayLines struct {
	pending []byte
	emit    func([]byte)
}

func (l *relayLines) Write(p []byte) (int, error) {
	l.pending = append(l.pending, p...)
	for {
		end := bytes.IndexByte(l.pending, '\n')
		if end < 0 {
			return len(p), nil
		}
		l.emit(l.pending[:end])
		l.pending = l.pending[end+1:]
	}
}

type relayLocal struct {
	ctx     context.Context
	servers []*mcp.Server
	mu      sync.Mutex
	local   map[relayList]map[string]localItem
	remote  map[relayList]map[string]bool
}

type localItem struct {
	server *mcp.Server
	raw    json.RawMessage
	order  int
}

func (r *relayLocal) serve(server *mcp.Server, line []byte, emit func([]byte)) {
	lines := &relayLines{emit: emit}
	_ = server.Serve(r.ctx, bytes.NewReader(append(append([]byte{}, line...), '\n')), lines)
}

func (r *relayLocal) items(list relayList) map[string]localItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	if found, ok := r.local[list]; ok {
		return found
	}
	items := map[string]localItem{}
	for _, server := range r.servers {
		r.serve(server, []byte(`{"jsonrpc":"2.0","id":1,"method":"`+list.method+`"}`), func(message []byte) {
			listed, _ := relayItems(message, list)
			for _, item := range listed {
				if _, taken := items[item.key]; !taken {
					items[item.key] = localItem{server: server, raw: item.raw, order: len(items)}
				}
			}
		})
	}
	r.local[list] = items
	return items
}

func (r *relayLocal) learn(list relayList, names map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.remote[list] = names
}

func (r *relayLocal) served(list relayList) (map[string]bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	names, known := r.remote[list]
	return names, known
}

type relayItem struct {
	key string
	raw json.RawMessage
}

func relayItems(message []byte, list relayList) ([]relayItem, bool) {
	var reply struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	if json.Unmarshal(message, &reply) != nil || reply.Result == nil {
		return nil, false
	}
	var raws []json.RawMessage
	if json.Unmarshal(reply.Result[list.field], &raws) != nil {
		return nil, false
	}
	items := make([]relayItem, 0, len(raws))
	for _, raw := range raws {
		var item map[string]any
		_ = json.Unmarshal(raw, &item)
		key, _ := item[list.key].(string)
		items = append(items, relayItem{key: key, raw: raw})
	}
	return items, true
}

func (r *relayLocal) merge(message []byte, list relayList) []byte {
	remote, listed := relayItems(message, list)
	if !listed {
		return message
	}
	names := map[string]bool{}
	for _, item := range remote {
		names[item.key] = true
	}
	r.learn(list, names)
	local := r.items(list)
	missing := make([]localItem, 0, len(local))
	for key, item := range local {
		if !names[key] {
			missing = append(missing, item)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].order < missing[j].order })
	merged := make([]json.RawMessage, 0, len(remote)+len(missing))
	for _, item := range remote {
		merged = append(merged, item.raw)
	}
	for _, item := range missing {
		merged = append(merged, item.raw)
	}
	var reply map[string]json.RawMessage
	var result map[string]json.RawMessage
	_ = json.Unmarshal(message, &reply)
	_ = json.Unmarshal(reply["result"], &result)
	result[list.field], _ = json.Marshal(merged)
	reply["result"], _ = json.Marshal(result)
	encoded, err := json.Marshal(reply)
	if err != nil {
		return message
	}
	return encoded
}

func Relay(ctx context.Context, name string, remote Remote, in io.Reader, out io.Writer, local ...*mcp.Server) error {
	var writing sync.Mutex
	var pending sync.WaitGroup
	write := func(line []byte) {
		var compact bytes.Buffer
		if json.Compact(&compact, line) != nil {
			return
		}
		writing.Lock()
		defer writing.Unlock()
		compact.WriteByte('\n')
		out.Write(compact.Bytes())
	}
	fail := func(id json.RawMessage, message string) {
		if len(id) == 0 {
			return
		}
		failure := relayError{JSONRPC: "2.0", ID: id}
		failure.Error.Code = -32001
		failure.Error.Message = message
		line, _ := json.Marshal(failure)
		write(line)
	}
	forward := func(body []byte, emit func([]byte)) error {
		endpoint, token, err := remote(ctx)
		if err != nil || token == "" {
			return errors.New("not signed in, run '" + name + " login'")
		}
		post, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		post.Header.Set("Content-Type", "application/json")
		post.Header.Set("Accept", "application/json, text/event-stream")
		post.Header.Set("Authorization", "Bearer "+token)
		post.Header.Set(mcp.HeaderProtocol, mcp.ProtocolVersion)
		res, err := http.DefaultClient.Do(post)
		if err != nil {
			return errors.New(endpoint + " is unreachable: " + err.Error())
		}
		defer res.Body.Close()
		switch {
		case res.StatusCode == http.StatusUnauthorized:
			return errors.New(endpoint + " refused the login, run '" + name + " login'")
		case res.StatusCode == http.StatusAccepted:
			return nil
		case res.StatusCode >= 300:
			answer, _ := io.ReadAll(res.Body)
			return errors.New(endpoint + " answered " + res.Status + ": " + strings.TrimSpace(string(answer)))
		}
		if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
			answer, _ := io.ReadAll(res.Body)
			emit(answer)
			return nil
		}
		frames := stream.Scanner(res)
		var data bytes.Buffer
		for frames.Scan() {
			line := frames.Text()
			switch {
			case strings.HasPrefix(line, "data: "):
				data.WriteString(strings.TrimPrefix(line, "data: "))
			case line == "" && data.Len() > 0:
				emit(append([]byte{}, data.Bytes()...))
				data.Reset()
			}
		}
		if data.Len() > 0 {
			emit(data.Bytes())
		}
		return nil
	}
	merger := &relayLocal{ctx: ctx, servers: local, local: map[relayList]map[string]localItem{}, remote: map[relayList]map[string]bool{}}
	owner := func(list relayList, key string) *mcp.Server {
		item, found := merger.items(list)[key]
		if !found {
			return nil
		}
		names, known := merger.served(list)
		if !known {
			_ = forward([]byte(`{"jsonrpc":"2.0","id":"`+name+`-relay","method":"`+list.method+`"}`), func(message []byte) {
				merger.merge(message, list)
			})
			names, known = merger.served(list)
		}
		if !known || names[key] {
			return nil
		}
		return item.server
	}
	lines := bufio.NewScanner(in)
	lines.Buffer(make([]byte, 0, 1<<16), 1<<24)
	for lines.Scan() {
		body := append([]byte{}, bytes.TrimSpace(lines.Bytes())...)
		if len(body) == 0 {
			continue
		}
		var req relayRequest
		_ = json.Unmarshal(body, &req)
		pending.Add(1)
		go func() {
			defer pending.Done()
			emit := write
			if len(local) > 0 {
				var server *mcp.Server
				switch req.Method {
				case "tools/call":
					server = owner(relayTools, req.Params.Name)
				case "resources/read":
					server = owner(relayResources, req.Params.URI)
				case relayTools.method, relayResources.method:
					list := relayTools
					if req.Method == relayResources.method {
						list = relayResources
					}
					emit = func(message []byte) {
						write(merger.merge(message, list))
					}
				}
				if server != nil {
					merger.serve(server, body, write)
					return
				}
			}
			if err := forward(body, emit); err != nil {
				fail(req.ID, err.Error())
			}
		}()
	}
	pending.Wait()
	return lines.Err()
}

type Call func(cmd *cobra.Command, method, path string, query url.Values, contentType string, body []byte) error

func OperationCommand(operation mcp.Operation, call Call) *cobra.Command {
	verb := operation.Name[strings.LastIndex(operation.Name, "_")+1:]
	paths := map[int]string{}
	var positional []string
	fewest := -1
	for _, path := range append([]string{operation.Path}, operation.Paths...) {
		var params []string
		for _, segment := range strings.Split(path, "/") {
			if strings.HasPrefix(segment, ":") {
				params = append(params, strings.TrimPrefix(segment, ":"))
			}
		}
		if _, taken := paths[len(params)]; !taken {
			paths[len(params)] = path
		}
		if len(params) > len(positional) {
			positional = params
		}
		if fewest < 0 || len(params) < fewest {
			fewest = len(params)
		}
	}
	use := verb
	for index, param := range positional {
		if index < fewest {
			use += " <" + param + ">"
			continue
		}
		use += " [" + param + "]"
	}
	cmd := &cobra.Command{
		Use:         use,
		Short:       operation.Description,
		Example:     operation.Example,
		Args:        cobra.RangeArgs(fewest, len(positional)),
		Annotations: map[string]string{AnnotationMCP: operation.Access},
	}
	flags := cmd.Flags()
	raw, declared := &Body{Raw: true}, false
	uploads := map[string]string{}
	for _, input := range operation.Inputs {
		if input.In == mcp.InFile {
			name := "upload"
			if len(uploads) > 0 {
				name += "-" + input.Param
			}
			uploads[input.Name] = name
			if input.Kind == "object" {
				flags.String(name, "", "file to send as the multipart part "+input.Param+", the other flags become form fields")
				continue
			}
			flags.StringArray(name, nil, "file to send as a multipart part "+input.Param+", repeatable, the other flags become form fields")
			continue
		}
		if input.In == mcp.InHeader && strings.EqualFold(input.Param, "Content-Type") {
			flags.StringVar(&raw.ContentType, "content-type", "", firstSet(input.Usage, "the media type of the body, sent as given"))
			continue
		}
		if input.In == mcp.InPath || input.In == mcp.InHeader {
			continue
		}
		if input.In == mcp.InBody && input.Param == "*" {
			raw.Flags(cmd, false)
			declared = true
			if input.Required {
				cmd.MarkFlagsOneRequired("data", "file")
			}
			continue
		}
		if input.In == mcp.InQuery && strings.HasSuffix(input.Param, "*") {
			flags.StringArray(input.Name, nil, input.Usage)
			continue
		}
		switch input.Kind {
		case "boolean":
			flags.Bool(input.Param, false, input.Usage)
		case "integer":
			flags.Int(input.Param, 0, input.Usage)
		case "number":
			flags.Float64(input.Param, 0, input.Usage)
		case "array":
			flags.StringSlice(input.Param, nil, input.Usage)
		default:
			flags.String(input.Param, "", input.Usage)
		}
		if input.Required {
			_ = cmd.MarkFlagRequired(input.Param)
		}
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		path, found := paths[len(args)]
		if !found {
			return Rejected(fmt.Sprintf("%s takes %d to %d arguments", operation.Name, fewest, len(positional)), "")
		}
		for index, param := range positional[:len(args)] {
			path = strings.Replace(path, ":"+param, url.PathEscape(args[index]), 1)
		}
		query := url.Values{}
		fields := map[string]any{}
		for _, input := range operation.Inputs {
			if prefix, open := strings.CutSuffix(input.Param, "*"); input.In == mcp.InQuery && open {
				entries, _ := cmd.Flags().GetStringArray(input.Name)
				for _, entry := range entries {
					key, value, paired := strings.Cut(entry, "=")
					if !paired {
						return Rejected("--"+input.Name+" takes key=value, not "+entry, "")
					}
					query.Add(prefix+key, value)
				}
				continue
			}
			if input.In != mcp.InBody && input.In != mcp.InQuery {
				continue
			}
			flag := cmd.Flags().Lookup(input.Param)
			if flag == nil || !flag.Changed {
				continue
			}
			var value any
			switch input.Kind {
			case "boolean":
				value, _ = cmd.Flags().GetBool(input.Param)
			case "integer":
				value, _ = cmd.Flags().GetInt(input.Param)
			case "number":
				value, _ = cmd.Flags().GetFloat64(input.Param)
			case "array":
				value, _ = cmd.Flags().GetStringSlice(input.Param)
			default:
				value = flag.Value.String()
			}
			if input.In == mcp.InQuery {
				if items, many := value.([]string); many {
					query[input.Param] = items
					continue
				}
				query.Set(input.Param, fmt.Sprint(value))
				continue
			}
			fields[input.Name] = value
		}
		var body []byte
		contentType := ""
		if len(fields) > 0 {
			encoded, err := json.Marshal(fields)
			if err != nil {
				return err
			}
			body, contentType = encoded, ContentTypeJSON
		}
		if declared {
			given, kind, err := raw.Get(cmd.InOrStdin())
			if err != nil {
				return err
			}
			body, contentType = given, kind
		}
		var parts [][2]string
		for _, input := range operation.Inputs {
			if input.In != mcp.InFile || !cmd.Flags().Changed(uploads[input.Name]) {
				continue
			}
			given, _ := cmd.Flags().GetStringArray(uploads[input.Name])
			if input.Kind == "object" {
				given = []string{cmd.Flags().Lookup(uploads[input.Name]).Value.String()}
			}
			for _, upload := range given {
				parts = append(parts, [2]string{input.Param, upload})
			}
		}
		if len(parts) > 0 {
			names := make([]string, 0, len(fields))
			for name := range fields {
				names = append(names, name)
			}
			sort.Strings(names)
			form := make([][2]string, 0, len(names))
			for _, name := range names {
				value, text := fields[name].(string)
				if !text {
					encoded, err := json.Marshal(fields[name])
					if err != nil {
						return err
					}
					value = string(encoded)
				}
				form = append(form, [2]string{name, value})
			}
			encoded, kind, err := writeMultipart(form, parts)
			if err != nil {
				return err
			}
			body, contentType = encoded, kind
		}
		return call(cmd, operation.Method, path, query, contentType, body)
	}
	return cmd
}
