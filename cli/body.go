package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/nimling/nimbody/mcp"
	"github.com/spf13/cobra"
)

const ContentTypeJSON = "application/json"

type Body struct {
	Data   string
	File   string
	Sets   []string
	Upload string
}

func (b *Body) Flags(cmd *cobra.Command, upload bool) {
	flags := cmd.Flags()
	flags.StringArrayVar(&b.Sets, "set", nil, "body field as key=value, repeatable, dotted keys nest, json values decode, key=@path reads the value from a file")
	flags.StringVar(&b.Data, "data", "", "request body as a json object")
	flags.StringVarP(&b.File, "file", "f", "", "read the request body from a json file, or - for stdin")
	if upload {
		flags.StringVar(&b.Upload, "upload", "", "file to send as the multipart file part, --set entries become form fields")
	}
}

func (b *Body) Get(in io.Reader) ([]byte, string, error) {
	if b.Upload != "" {
		return b.multipart()
	}
	if b.Data != "" && b.File != "" {
		return nil, "", Rejected("pass either --data or --file, never both", "")
	}
	payload := map[string]any{}
	var raw []byte
	var err error
	switch {
	case b.File == "-":
		raw, err = io.ReadAll(in)
	case b.File != "":
		raw, err = os.ReadFile(b.File)
		if err != nil {
			return nil, "", Rejected("cannot read "+b.File, "")
		}
	case b.Data != "":
		raw = []byte(b.Data)
	}
	if err != nil {
		return nil, "", err
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, "", Rejected("the body is not a json object: "+err.Error(), "")
		}
	}
	for _, entry := range b.Sets {
		key, value, found := strings.Cut(entry, "=")
		if !found || key == "" {
			return nil, "", Rejected("set entries look like key=value, got "+entry, "")
		}
		value, err := getSetValue(value)
		if err != nil {
			return nil, "", err
		}
		assign(payload, strings.Split(key, "."), decodeValue(value))
	}
	if len(payload) == 0 {
		return nil, "", nil
	}
	body, err := json.Marshal(payload)
	return body, ContentTypeJSON, err
}

func (b *Body) multipart() ([]byte, string, error) {
	source, err := os.Open(b.Upload)
	if err != nil {
		return nil, "", Rejected("cannot read "+b.Upload, "")
	}
	defer source.Close()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for _, entry := range b.Sets {
		key, value, found := strings.Cut(entry, "=")
		if !found || key == "" {
			return nil, "", Rejected("set entries look like key=value, got "+entry, "")
		}
		value, err := getSetValue(value)
		if err != nil {
			return nil, "", err
		}
		if err := writer.WriteField(key, value); err != nil {
			return nil, "", err
		}
	}
	part, err := writer.CreateFormFile("file", filepath.Base(b.Upload))
	if err != nil {
		return nil, "", err
	}
	if _, err := io.Copy(part, source); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buffer.Bytes(), writer.FormDataContentType(), nil
}

func assign(target map[string]any, path []string, value any) {
	for _, key := range path[:len(path)-1] {
		next, ok := target[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			target[key] = next
		}
		target = next
	}
	target[path[len(path)-1]] = value
}

func getSetValue(value string) (string, error) {
	path, isFile := strings.CutPrefix(value, "@")
	if !isFile {
		return value, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", Rejected("cannot read "+path, "")
	}
	return string(raw), nil
}

func decodeValue(value string) any {
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err == nil {
		return decoded
	}
	return value
}

var apiMethods = map[string]string{
	http.MethodGet:    mcp.AccessRead,
	http.MethodHead:   mcp.AccessRead,
	http.MethodPost:   mcp.AccessWrite,
	http.MethodPut:    mcp.AccessWrite,
	http.MethodPatch:  mcp.AccessWrite,
	http.MethodDelete: mcp.AccessDestructive,
}

func APICommand(send Send, output *Output) *cobra.Command {
	if output == nil {
		output = &Output{}
	}
	body := &Body{}
	cmd := &cobra.Command{
		Use:         "api <method> <path>",
		Short:       "Send a request to any api path the resource commands do not model",
		Example:     "api GET /api/status\napi POST /api/articles --set title=Orion --set tags='[\"news\"]'",
		Annotations: map[string]string{AnnotationMCP: mcp.AccessWrite, AnnotationRoute: route{Body: true, Upload: true}.annotation()},
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.ExactArgs(2)(cmd, args); err != nil {
				return err
			}
			access, known := apiMethods[strings.ToUpper(args[0])]
			if !known {
				return Rejected("api takes GET, HEAD, POST, PUT, PATCH or DELETE, got "+args[0], "")
			}
			if !strings.HasPrefix(args[1], "/") {
				return Rejected("the path starts with a slash, for example /api/status", "")
			}
			cmd.Annotations[AnnotationMCP] = access
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			payload, contentType, err := body.Get(cmd.InOrStdin())
			if err != nil {
				return err
			}
			answer, err := send(cmd, strings.ToUpper(args[0]), args[1], output.GetQuery(), contentType, payload)
			if err != nil {
				return err
			}
			return output.Print(cmd.OutOrStdout(), cmd.ErrOrStderr(), answer)
		},
	}
	body.Flags(cmd, true)
	return cmd
}
