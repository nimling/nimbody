package shape

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"text/template"
	"time"

	"github.com/itchyny/gojq"
)

type Shape struct {
	Fields   []string `json:"fields,omitempty"`
	Sort     string   `json:"sort,omitempty"`
	JQ       string   `json:"jq,omitempty"`
	Template string   `json:"template,omitempty"`
	Colour   bool     `json:"-"`
}

const jqTimeout = 30 * time.Second

func Schema() map[string]any {
	return map[string]any{
		"fields":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "columns to keep, in order, dotted for nested keys (b.c)"},
		"sort":     map[string]any{"type": "string", "description": "sort rows by a field, field:desc for descending"},
		"jq":       map[string]any{"type": "string", "description": "a jq expression over the result, strings print raw"},
		"template": map[string]any{"type": "string", "description": "a Go template over the result with the table, json, join, pluck, timeago, color and truncate helpers"},
	}
}

func Decode(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var plain any
	return plain, json.Unmarshal(raw, &plain)
}

func (s Shape) Text() bool {
	return s.JQ != "" || s.Template != ""
}

func (s Shape) Select(value any) (any, []string, error) {
	if s.Sort != "" {
		if err := Sort(value, s.Sort); err != nil {
			return nil, nil, err
		}
	}
	if len(s.Fields) == 0 {
		return value, nil, nil
	}
	switch shaped := value.(type) {
	case []any:
		rows := make([]any, len(shaped))
		for i, row := range shaped {
			rows[i] = pick(row, s.Fields)
		}
		return rows, s.Fields, nil
	case map[string]any:
		return pick(shaped, s.Fields), s.Fields, nil
	}
	return value, nil, nil
}

func (s Shape) Write(out io.Writer, value any) error {
	if s.JQ != "" {
		return jq(out, s.JQ, value)
	}
	source := s.Template
	if strings.HasPrefix(source, "@") {
		body, err := os.ReadFile(strings.TrimPrefix(source, "@"))
		if err != nil {
			return err
		}
		source = string(body)
	}
	parsed, err := template.New("output").Funcs(s.funcs()).Parse(source)
	if err != nil {
		return errors.New("the template does not parse: " + err.Error())
	}
	if err := parsed.Execute(out, value); err != nil {
		return errors.New("the template failed: " + err.Error())
	}
	return nil
}

func pick(row any, fields []string) any {
	object, ok := row.(map[string]any)
	if !ok {
		return row
	}
	picked := make(map[string]any, len(fields))
	for _, field := range fields {
		picked[field] = Field(object, field)
	}
	return picked
}

func Field(value any, path string) any {
	for _, key := range strings.Split(path, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[key]
	}
	return value
}

func Sort(value any, spec string) error {
	rows, ok := value.([]any)
	if !ok {
		return nil
	}
	field, order, _ := strings.Cut(spec, ":")
	if order != "" && order != "asc" && order != "desc" {
		return errors.New("--sort takes field, field:asc or field:desc, not " + spec)
	}
	descending := order == "desc"
	sort.SliceStable(rows, func(i, j int) bool {
		left, right := Field(rows[i], field), Field(rows[j], field)
		if descending {
			return less(right, left)
		}
		return less(left, right)
	})
	return nil
}

func less(left, right any) bool {
	switch {
	case left == nil:
		return false
	case right == nil:
		return true
	}
	leftNumber, leftIsNumber := left.(float64)
	rightNumber, rightIsNumber := right.(float64)
	if leftIsNumber && rightIsNumber {
		return leftNumber < rightNumber
	}
	return Cell(left) < Cell(right)
}

func jq(out io.Writer, expression string, value any) error {
	query, err := gojq.Parse(expression)
	if err != nil {
		return errors.New("the jq expression does not parse: " + err.Error())
	}
	code, err := gojq.Compile(query)
	if err != nil {
		return errors.New("the jq expression does not compile: " + err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), jqTimeout)
	defer cancel()
	results := code.RunWithContext(ctx, value)
	for {
		result, ok := results.Next()
		if !ok {
			return nil
		}
		if err, failed := result.(error); failed {
			var halt *gojq.HaltError
			if errors.As(err, &halt) && halt.Value() == nil {
				return nil
			}
			return errors.New("jq failed: " + err.Error())
		}
		if text, isText := result.(string); isText {
			fmt.Fprintln(out, text)
			continue
		}
		line, err := json.Marshal(result)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(line))
	}
}

func Cell(value any) string {
	switch shaped := value.(type) {
	case nil:
		return ""
	case string:
		return shaped
	case bool:
		return strconv.FormatBool(shaped)
	case float64:
		if shaped == float64(int64(shaped)) {
			return strconv.FormatInt(int64(shaped), 10)
		}
		return strconv.FormatFloat(shaped, 'f', -1, 64)
	default:
		encoded, _ := json.Marshal(shaped)
		return string(encoded)
	}
}

var colours = map[string]string{
	"bold": "1", "dim": "2", "underline": "4",
	"red": "31", "green": "32", "yellow": "33", "blue": "34", "magenta": "35", "cyan": "36", "white": "37",
}

func (s Shape) funcs() template.FuncMap {
	return template.FuncMap{
		"json": func(value any) (string, error) {
			raw, err := json.Marshal(value)
			return string(raw), err
		},
		"join": func(separator string, list any) string {
			items, _ := list.([]any)
			texts := make([]string, len(items))
			for i, item := range items {
				texts[i] = Cell(item)
			}
			return strings.Join(texts, separator)
		},
		"pluck": func(field string, list any) []any {
			items, _ := list.([]any)
			plucked := make([]any, len(items))
			for i, item := range items {
				plucked[i] = Field(item, field)
			}
			return plucked
		},
		"timeago": func(value any) string {
			at, err := time.Parse(time.RFC3339Nano, Cell(value))
			if err != nil {
				return Cell(value)
			}
			return ago(time.Since(at))
		},
		"color": func(style string, value any) string {
			text := Cell(value)
			code, known := colours[style]
			if !s.Colour || !known || text == "" {
				return text
			}
			return "\x1b[" + code + "m" + text + "\x1b[0m"
		},
		"truncate": func(length int, value any) string {
			runes := []rune(Cell(value))
			if len(runes) <= length {
				return string(runes)
			}
			if length < 1 {
				return ""
			}
			return string(runes[:length-1]) + "…"
		},
		"table": func(list any, columns ...string) string {
			return table(list, columns)
		},
	}
}

func ago(since time.Duration) string {
	future := since < 0
	if future {
		since = -since
	}
	var amount string
	switch {
	case since < time.Minute:
		return "just now"
	case since < time.Hour:
		amount = plural(int(since/time.Minute), "minute")
	case since < 24*time.Hour:
		amount = plural(int(since/time.Hour), "hour")
	case since < 30*24*time.Hour:
		amount = plural(int(since/(24*time.Hour)), "day")
	case since < 365*24*time.Hour:
		amount = plural(int(since/(30*24*time.Hour)), "month")
	default:
		amount = plural(int(since/(365*24*time.Hour)), "year")
	}
	if future {
		return "in " + amount
	}
	return amount + " ago"
}

func plural(count int, unit string) string {
	if count == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(count) + " " + unit + "s"
}

func table(list any, columns []string) string {
	rows, _ := list.([]any)
	if len(columns) == 0 {
		seen := map[string]bool{}
		for _, row := range rows {
			if object, ok := row.(map[string]any); ok {
				for key := range object {
					if !seen[key] {
						seen[key] = true
						columns = append(columns, key)
					}
				}
			}
		}
		sort.Strings(columns)
	}
	var out strings.Builder
	writer := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
	header := make([]string, len(columns))
	for i, column := range columns {
		header[i] = strings.ToUpper(column)
	}
	fmt.Fprintln(writer, strings.Join(header, "\t"))
	for _, row := range rows {
		cells := make([]string, len(columns))
		for i, column := range columns {
			cells[i] = strings.ReplaceAll(Cell(Field(row, column)), "\t", " ")
		}
		fmt.Fprintln(writer, strings.Join(cells, "\t"))
	}
	writer.Flush()
	return out.String()
}
