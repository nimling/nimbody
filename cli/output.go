package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/nimling/nimbody/shape"
	"github.com/nimling/nimbody/stream"
	"github.com/spf13/cobra"
)

type Format int

const (
	FormatTable Format = iota
	FormatPlain
	FormatJSON
)

const (
	cellCeiling   = 42
	columnCeiling = 8
)

var LeadingColumns = []string{"id", "name", "title", "slug", "email", "date", "status", "type", "kind", "hours"}

type Output struct {
	JSON       bool
	Raw        bool
	Plain      bool
	Fields     []string
	Sort       string
	JQ         string
	Template   string
	Limit      int
	Cursor     string
	ServerSort bool
	Format     string
	Query      []string
}

const (
	FormatConcise  = "concise"
	FormatDetailed = "detailed"
)

func (o *Output) Flags(root *cobra.Command) {
	o.flags(root, false)
}

func (o *Output) flags(root *cobra.Command, formula bool) {
	flags := root.PersistentFlags()
	if formula {
		flags.Var(&choice{value: &o.Format, choices: []string{FormatConcise, FormatDetailed}}, "format", "concise keeps the leading scalar fields, about a third of the tokens, detailed keeps every field")
		flags.Var(&entries{values: &o.Query}, "query", "query parameter for the server's own filters as key=value, repeatable")
	}
	flags.BoolVar(&o.JSON, "json", false, "print the data as json instead of a table")
	flags.BoolVar(&o.Raw, "raw", false, "print the whole response envelope")
	flags.BoolVar(&o.Plain, "plain", false, "print tab separated lines")
	flags.StringSliceVar(&o.Fields, "fields", nil, "columns to keep, in order, dotted for nested keys: id,name,owner.email")
	flags.StringVar(&o.Sort, "sort", "", "sort rows by a field, field:desc for descending")
	flags.StringVar(&o.JQ, "jq", "", "filter the output with a jq expression, strings print raw")
	flags.StringVar(&o.Template, "template", "", "format the output with a Go template, or @file; helpers table, json, join, pluck, timeago, color, truncate")
	flags.IntVar(&o.Limit, "limit", 0, "rows per page, passed to the server")
	flags.StringVar(&o.Cursor, "cursor", "", "page cursor from a truncated list, passed to the server")
}

func (o Output) GetQuery() url.Values {
	values := url.Values{}
	if o.Limit > 0 {
		values.Set("limit", strconv.Itoa(o.Limit))
	}
	if o.Cursor != "" {
		values.Set("cursor", o.Cursor)
	}
	for _, entry := range o.Query {
		if key, value, found := strings.Cut(entry, "="); found && key != "" {
			values.Add(key, value)
		}
	}
	if o.ServerSort && o.Sort != "" && o.Cursor == "" {
		column, order, _ := strings.Cut(o.Sort, ":")
		values.Set("column", column)
		values.Set("order", "ASC")
		if order == "desc" {
			values.Set("order", "DESC")
		}
	}
	return values
}

type envelope struct {
	Data       json.RawMessage `json:"data"`
	RequestID  *string         `json:"request_id"`
	Count      int             `json:"count"`
	Pagination *struct {
		Total int     `json:"total"`
		Next  *string `json:"next"`
	} `json:"pagination"`
}

func (o Output) Print(out, errOut io.Writer, body []byte) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var wrapped envelope
	isEnvelope := json.Unmarshal(body, &wrapped) == nil && wrapped.RequestID != nil
	payload := body
	if isEnvelope && !o.Raw {
		payload = wrapped.Data
	}
	decoded, err := o.render(out, payload)
	if err != nil {
		return err
	}
	if isEnvelope {
		o.hint(errOut, decoded, wrapped)
	}
	return nil
}

func (o Output) PrintValue(out io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = o.render(out, payload)
	return err
}

func (o Output) render(out io.Writer, payload []byte) (any, error) {
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, printJSON(out, payload)
	}
	if o.Format == FormatConcise && len(o.Fields) == 0 {
		decoded = concise(decoded)
		if reduced, err := json.Marshal(decoded); err == nil {
			payload = reduced
		}
	}
	shaper := shape.Shape{Fields: o.Fields, Sort: o.Sort, JQ: o.JQ, Template: o.Template, Colour: Colour(out)}
	if o.ServerSort {
		shaper.Sort = ""
	}
	format := FormatFor(out, o.JSON || o.Raw)
	if o.Plain && !o.JSON && !o.Raw {
		format = FormatPlain
	}
	if format == FormatJSON && shaper.Sort == "" && len(shaper.Fields) == 0 && !shaper.Text() {
		return decoded, printJSON(out, payload)
	}
	selected, columns, err := shaper.Select(decoded)
	if err != nil {
		return nil, Rejected(err.Error(), "")
	}
	if shaper.Text() {
		if err := shaper.Write(out, selected); err != nil {
			return nil, Rejected(err.Error(), "")
		}
		return decoded, nil
	}
	return decoded, render(out, selected, format, columns)
}

func (o Output) hint(errOut io.Writer, decoded any, wrapped envelope) {
	rows, isList := decoded.([]any)
	if !isList || o.Raw {
		return
	}
	total := wrapped.Count
	next := ""
	if wrapped.Pagination != nil {
		if wrapped.Pagination.Total > total {
			total = wrapped.Pagination.Total
		}
		if wrapped.Pagination.Next != nil {
			if link, err := url.Parse(*wrapped.Pagination.Next); err == nil {
				next = link.Query().Get("cursor")
			}
		}
	}
	if next == "" && total <= len(rows) {
		return
	}
	line := strconv.Itoa(len(rows)) + " of " + strconv.Itoa(total) + " rows"
	if next != "" {
		line += ", next page with --cursor " + next
	}
	fmt.Fprintln(errOut, line+", or narrow with --query, --limit or --fields")
}

func FormatFor(out io.Writer, asJSON bool) Format {
	switch {
	case asJSON, stream.Machine(), !Terminal(out):
		return FormatJSON
	default:
		return FormatTable
	}
}

func Print(out io.Writer, payload []byte, format Format) error {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil
	}
	if format == FormatJSON {
		return printJSON(out, payload)
	}
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return printJSON(out, payload)
	}
	return render(out, decoded, format, nil)
}

func PrintValue(out io.Writer, value any, format Format) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return Print(out, payload, format)
}

func render(out io.Writer, decoded any, format Format, columns []string) error {
	if format == FormatJSON {
		payload, err := json.Marshal(decoded)
		if err != nil {
			return err
		}
		return printJSON(out, payload)
	}
	style := Style{}
	if format == FormatTable {
		style = StyleFor(out)
	}
	switch shaped := decoded.(type) {
	case []any:
		return printRows(out, shaped, format, style, columns)
	case map[string]any:
		return printFields(out, shaped, format, style, columns)
	case nil:
		return nil
	default:
		_, err := fmt.Fprintln(out, cellText(shaped, format))
		return err
	}
}

func printJSON(out io.Writer, payload []byte) error {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, payload, "", "  "); err != nil {
		_, writeErr := out.Write(append(bytes.TrimRight(payload, "\n"), '\n'))
		return writeErr
	}
	pretty.WriteByte('\n')
	_, err := out.Write(pretty.Bytes())
	return err
}

func printRows(out io.Writer, rows []any, format Format, style Style, columns []string) error {
	objects := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		object, ok := row.(map[string]any)
		if !ok {
			fmt.Fprintln(out, cellText(row, format))
			continue
		}
		objects = append(objects, object)
	}
	if len(objects) == 0 {
		if len(rows) == 0 && format == FormatTable {
			fmt.Fprintln(out, style.Dim("no rows"))
		}
		return nil
	}

	if len(columns) == 0 {
		columns = tableColumns(objects)
	}
	if format == FormatPlain {
		fmt.Fprintln(out, strings.Join(columns, "\t"))
		for _, object := range objects {
			cells := make([]string, len(columns))
			for index, column := range columns {
				cells[index] = cellText(object[column], format)
			}
			fmt.Fprintln(out, strings.Join(cells, "\t"))
		}
		return nil
	}

	widths := make([]int, len(columns))
	for index, column := range columns {
		widths[index] = len(column)
	}
	cells := make([][]string, len(objects))
	for rowIndex, object := range objects {
		cells[rowIndex] = make([]string, len(columns))
		for index, column := range columns {
			text := cellText(object[column], format)
			cells[rowIndex][index] = text
			if width := len([]rune(text)); width > widths[index] {
				widths[index] = width
			}
		}
	}
	header := make([]string, len(columns))
	for index, column := range columns {
		header[index] = pad(strings.ToUpper(column), widths[index])
	}
	fmt.Fprintln(out, style.Bold(strings.TrimRight(strings.Join(header, "  "), " ")))
	for _, row := range cells {
		padded := make([]string, len(row))
		for index, text := range row {
			padded[index] = pad(text, widths[index])
		}
		fmt.Fprintln(out, strings.TrimRight(strings.Join(padded, "  "), " "))
	}
	fmt.Fprintln(out, style.Dim(strconv.Itoa(len(objects))+" rows"))
	return nil
}

func printFields(out io.Writer, object map[string]any, format Format, style Style, keys []string) error {
	if len(keys) == 0 {
		keys = fieldOrder(object)
	}
	if format == FormatPlain {
		for _, key := range keys {
			fmt.Fprintln(out, key+"\t"+cellText(object[key], format))
		}
		return nil
	}
	width := 0
	for _, key := range keys {
		if len(key) > width {
			width = len(key)
		}
	}
	for _, key := range keys {
		fmt.Fprintln(out, style.Dim(pad(key, width))+"  "+cellText(object[key], format))
	}
	return nil
}

func tableColumns(objects []map[string]any) []string {
	seen := map[string]bool{}
	for _, object := range objects {
		for key, value := range object {
			if scalar(value) {
				seen[key] = true
			}
		}
	}
	var columns []string
	for _, preferred := range LeadingColumns {
		if seen[preferred] {
			columns = append(columns, preferred)
			delete(seen, preferred)
		}
	}
	var rest []string
	for key := range seen {
		if !strings.HasPrefix(key, "sa_") {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	columns = append(columns, rest...)
	if len(columns) > columnCeiling {
		columns = columns[:columnCeiling]
	}
	return columns
}

func fieldOrder(object map[string]any) []string {
	var keys []string
	seen := map[string]bool{}
	for _, preferred := range LeadingColumns {
		if _, ok := object[preferred]; ok {
			keys = append(keys, preferred)
			seen[preferred] = true
		}
	}
	var rest []string
	for key := range object {
		if !seen[key] {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

func scalar(value any) bool {
	switch value.(type) {
	case map[string]any, []any:
		return false
	}
	return true
}

func cellText(value any, format Format) string {
	text := strings.ReplaceAll(strings.ReplaceAll(shape.Cell(value), "\n", " "), "\t", " ")
	if format == FormatPlain {
		return text
	}
	runes := []rune(text)
	if len(runes) <= cellCeiling {
		return text
	}
	return string(runes[:cellCeiling-1]) + "…"
}

func pad(text string, width int) string {
	gap := width - len([]rune(text))
	if gap <= 0 {
		return text
	}
	return text + strings.Repeat(" ", gap)
}

func concise(decoded any) any {
	switch shaped := decoded.(type) {
	case []any:
		objects := make([]map[string]any, 0, len(shaped))
		for _, row := range shaped {
			if object, ok := row.(map[string]any); ok {
				objects = append(objects, object)
			}
		}
		if len(objects) < len(shaped) {
			return shaped
		}
		columns := tableColumns(objects)
		rows := make([]any, 0, len(objects))
		for _, object := range objects {
			kept := map[string]any{}
			for _, column := range columns {
				if value, ok := object[column]; ok {
					kept[column] = value
				}
			}
			rows = append(rows, kept)
		}
		return rows
	case map[string]any:
		kept := map[string]any{}
		for key, value := range shaped {
			if scalar(value) && !strings.HasPrefix(key, "sa_") {
				kept[key] = value
			}
		}
		return kept
	}
	return decoded
}

type choice struct {
	value   *string
	choices []string
}

func (c *choice) String() string { return *c.value }

func (c *choice) Type() string { return "string" }

func (c *choice) Set(value string) error {
	for _, allowed := range c.choices {
		if value == allowed {
			*c.value = value
			return nil
		}
	}
	return fmt.Errorf("takes %s", strings.Join(c.choices, " or "))
}

type entries struct {
	values *[]string
}

func (e *entries) String() string { return strings.Join(*e.values, ",") }

func (e *entries) Type() string { return "stringArray" }

func (e *entries) Set(value string) error {
	if key, _, found := strings.Cut(value, "="); !found || key == "" {
		return fmt.Errorf("entries look like key=value")
	}
	*e.values = append(*e.values, value)
	return nil
}
