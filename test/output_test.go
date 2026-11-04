package test

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/nimling/nimbody/cli"
	"github.com/nimling/nimbody/shape"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

const listEnvelope = `{"data":[{"id":"a","name":"Orion","owner":{"email":"b@x.no"},"hours":3},{"id":"b","name":"Atlas","owner":{"email":"a@x.no"},"hours":12}],"request_id":"0b4c8a52-6f7e-4a59-9a58-0a3c2b2b8f10","count":2,"pagination":{"page":1,"limit":2,"total":7,"next":"/api/articles?cursor=Y3Vyc29y"}}`

func TestOutputFieldsSortAndPlain(t *testing.T) {
	var out, hint bytes.Buffer
	output := cli.Output{Plain: true, Fields: []string{"name", "owner.email"}, Sort: "owner.email"}
	assert.NoError(t, output.Print(&out, &hint, []byte(listEnvelope)))
	assert.Equal(t, "name\towner.email\nAtlas\ta@x.no\nOrion\tb@x.no\n", out.String())
	assert.Equal(t, "2 of 7 rows, next page with --cursor Y3Vyc29y, or narrow with --query, --limit or --fields\n", hint.String())

	out.Reset()
	output = cli.Output{Plain: true, Fields: []string{"id"}, Sort: "hours:desc"}
	assert.NoError(t, output.Print(&out, &bytes.Buffer{}, []byte(listEnvelope)))
	assert.Equal(t, "id\nb\na\n", out.String())
}

func TestOutputTableKeepsTheFieldOrder(t *testing.T) {
	var out bytes.Buffer
	output := cli.Output{Fields: []string{"hours", "name"}}
	assert.NoError(t, output.PrintValue(&out, []map[string]any{{"name": "Orion", "hours": 3, "id": "a"}}))
	assert.Contains(t, out.String(), `"hours": 3`, "a buffer is a pipe, so json with the selected fields")
	assert.NotContains(t, out.String(), `"id"`)
}

func TestOutputJQPrintsStringsRaw(t *testing.T) {
	var out bytes.Buffer
	output := cli.Output{JQ: ".[] | select(.hours > 5) | .name"}
	assert.NoError(t, output.Print(&out, &bytes.Buffer{}, []byte(listEnvelope)))
	assert.Equal(t, "Atlas\n", out.String())

	out.Reset()
	output = cli.Output{JQ: "map(.hours)"}
	assert.NoError(t, output.Print(&out, &bytes.Buffer{}, []byte(listEnvelope)))
	assert.Equal(t, "[3,12]\n", out.String())

	output = cli.Output{JQ: ".[ | nope"}
	assert.Equal(t, cli.ExitRejected, cli.ExitCode(output.Print(&out, &bytes.Buffer{}, []byte(listEnvelope))))
}

func TestOutputTemplateHelpers(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var out bytes.Buffer
	output := cli.Output{Template: `{{join ", " (pluck "name" .)}}|{{truncate 4 "Atlas"}}|{{color "red" "x"}}|{{json (index . 0).owner}}`}
	assert.NoError(t, output.Print(&out, &bytes.Buffer{}, []byte(listEnvelope)))
	assert.Equal(t, `Orion, Atlas|Atl…|x|{"email":"b@x.no"}`, out.String())

	out.Reset()
	output = cli.Output{Template: `{{table . "id" "name"}}`}
	assert.NoError(t, output.Print(&out, &bytes.Buffer{}, []byte(listEnvelope)))
	assert.Equal(t, "ID  NAME\na   Orion\nb   Atlas\n", out.String())

	out.Reset()
	file := filepath.Join(t.TempDir(), "row.tmpl")
	assert.NoError(t, os.WriteFile(file, []byte(`{{range .}}{{.id}} {{timeago "2001-01-01T00:00:00Z"}}{{"\n"}}{{end}}`), 0o644))
	output = cli.Output{Template: "@" + file}
	assert.NoError(t, output.Print(&out, &bytes.Buffer{}, []byte(listEnvelope)))
	assert.Contains(t, out.String(), "a ")
	assert.Contains(t, out.String(), "years ago")
}

func TestOutputColourHelperHonoursTheTerminal(t *testing.T) {
	var out bytes.Buffer
	assert.NoError(t, shape.Shape{Template: `{{color "green" "ok"}}`, Colour: true}.Write(&out, nil))
	assert.Equal(t, "\x1b[32mok\x1b[0m", out.String())
}

func TestOutputRawKeepsTheEnvelope(t *testing.T) {
	var out, hint bytes.Buffer
	output := cli.Output{Raw: true}
	assert.NoError(t, output.Print(&out, &hint, []byte(listEnvelope)))
	assert.Contains(t, out.String(), `"request_id"`)
	assert.Empty(t, hint.String())
}

func TestOutputQueryPassesPagination(t *testing.T) {
	output := cli.Output{Limit: 50, Cursor: "abc", Sort: "name:desc", ServerSort: true}
	assert.Equal(t, url.Values{"limit": {"50"}, "cursor": {"abc"}}, output.GetQuery(), "a cursor carries its own order")
	output.Cursor = ""
	assert.Equal(t, url.Values{"limit": {"50"}, "column": {"name"}, "order": {"DESC"}}, output.GetQuery())
	output.ServerSort = false
	assert.Equal(t, url.Values{"limit": {"50"}}, output.GetQuery())
}

func TestOutputFlagsReachMCPTools(t *testing.T) {
	root := &cobra.Command{Use: "demo"}
	var output cli.Output
	output.Flags(root)
	get := &cobra.Command{Use: "get", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
	article := &cobra.Command{Use: "article"}
	article.AddCommand(get)
	root.AddCommand(article)
	tools := cli.Tools(root, "/bin/demo")
	assert.Len(t, tools, 1)
	flags := tools[0].InputSchema["properties"].(map[string]any)["flags"].(map[string]any)["properties"].(map[string]any)
	for _, name := range []string{"json", "raw", "plain", "fields", "sort", "jq", "template", "limit", "cursor"} {
		assert.Contains(t, flags, name)
	}
}
