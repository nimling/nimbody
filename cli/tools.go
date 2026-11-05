package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
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
			if flag.Hidden || flag.Name == "help" || flag.Name == "version" || secrets[flag.Name] || len(flag.Annotations[AnnotationSecret]) > 0 {
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
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	server := &mcp.Server{Name: name, Version: version, Instructions: GetInstructions(root, name), Tools: Tools(root, executable), Pages: pages}
	return server.Serve(ctx, in, out)
}

type Remote func(ctx context.Context) (endpoint string, token string, err error)

var Bridge Remote

func MCPCommand(root *cobra.Command, name string, version func() string, pages ...func() mcp.Page) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve every command as a tool over the model context protocol on stdio",
		Long: "Serve every command as a tool over the model context protocol on stdio. With an endpoint and a\n" +
			"login, it bridges stdio to the service's own /mcp endpoint with the stored token, so the tools are\n" +
			"the backend's and reach exactly what the login's claims reach. Without one, a tool runs this binary\n" +
			"with the arguments and flags it is given, so it reads the same configuration as a terminal in the\n" +
			"directory the host was started in. Each tool reports its steps as progress, and a host that renders\n" +
			"MCP Apps shows the run live on ui://" + name + "/run, or the result on the page a command names.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if Bridge != nil {
				endpoint, token, err := Bridge(cmd.Context())
				if err != nil {
					return err
				}
				if endpoint != "" && token != "" {
					return Relay(cmd.Context(), name, Bridge, cmd.InOrStdin(), cmd.OutOrStdout())
				}
			}
			built := make([]mcp.Page, 0, len(pages))
			for _, page := range pages {
				built = append(built, page())
			}
			return Serve(cmd.Context(), root, name, version(), cmd.InOrStdin(), cmd.OutOrStdout(), built...)
		},
	}
}

type relayRequest struct {
	ID json.RawMessage `json:"id,omitempty"`
}

type relayError struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func Relay(ctx context.Context, name string, remote Remote, in io.Reader, out io.Writer) error {
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
			endpoint, token, err := remote(ctx)
			if err != nil || token == "" {
				fail(req.ID, "not signed in, run '"+name+" login'")
				return
			}
			post, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
			if err != nil {
				fail(req.ID, err.Error())
				return
			}
			post.Header.Set("Content-Type", "application/json")
			post.Header.Set("Accept", "application/json, text/event-stream")
			post.Header.Set("Authorization", "Bearer "+token)
			post.Header.Set(mcp.HeaderProtocol, mcp.ProtocolVersion)
			res, err := http.DefaultClient.Do(post)
			if err != nil {
				fail(req.ID, endpoint+" is unreachable: "+err.Error())
				return
			}
			defer res.Body.Close()
			switch {
			case res.StatusCode == http.StatusUnauthorized:
				fail(req.ID, endpoint+" refused the login, run '"+name+" login'")
				return
			case res.StatusCode == http.StatusAccepted:
				return
			case res.StatusCode >= 300:
				answer, _ := io.ReadAll(res.Body)
				fail(req.ID, endpoint+" answered "+res.Status+": "+strings.TrimSpace(string(answer)))
				return
			}
			if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
				answer, _ := io.ReadAll(res.Body)
				write(answer)
				return
			}
			frames := stream.Scanner(res)
			var data bytes.Buffer
			for frames.Scan() {
				line := frames.Text()
				switch {
				case strings.HasPrefix(line, "data: "):
					data.WriteString(strings.TrimPrefix(line, "data: "))
				case line == "" && data.Len() > 0:
					write(data.Bytes())
					data.Reset()
				}
			}
			if data.Len() > 0 {
				write(data.Bytes())
			}
		}()
	}
	pending.Wait()
	return lines.Err()
}

type Call func(cmd *cobra.Command, method, path string, query url.Values, body []byte) error

func OperationCommand(operation mcp.Operation, call Call) *cobra.Command {
	verb := operation.Name[strings.LastIndex(operation.Name, "_")+1:]
	var positional []string
	for _, segment := range strings.Split(operation.Path, "/") {
		if strings.HasPrefix(segment, ":") {
			positional = append(positional, strings.TrimPrefix(segment, ":"))
		}
	}
	use := verb
	for _, param := range positional {
		use += " <" + param + ">"
	}
	cmd := &cobra.Command{
		Use:         use,
		Short:       operation.Description,
		Example:     operation.Example,
		Args:        cobra.ExactArgs(len(positional)),
		Annotations: map[string]string{AnnotationMCP: operation.Access},
	}
	flags := cmd.Flags()
	for _, input := range operation.Inputs {
		if input.In == mcp.InPath {
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
		path := operation.Path
		for index, param := range positional {
			path = strings.Replace(path, ":"+param, url.PathEscape(args[index]), 1)
		}
		query := url.Values{}
		fields := map[string]any{}
		for _, input := range operation.Inputs {
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
		if len(fields) > 0 {
			encoded, err := json.Marshal(fields)
			if err != nil {
				return err
			}
			body = encoded
		}
		return call(cmd, operation.Method, path, query, body)
	}
	return cmd
}
