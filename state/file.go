package state

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/labstack/echo/v4"
)

type Summary struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Blobs int    `json:"blobs,omitempty"`
	Hash  string `json:"hash,omitempty"`
}

type Manifest struct {
	Format  string    `json:"format"`
	Service string    `json:"service"`
	Version string    `json:"version"`
	Scope   []Target  `json:"scope"`
	History bool      `json:"history,omitempty"`
	Blobs   bool      `json:"blobs,omitempty"`
	Groups  []Summary `json:"groups"`
}

type Line struct {
	Group string         `json:"group"`
	Key   string         `json:"key"`
	ID    string         `json:"id,omitempty"`
	Data  map[string]any `json:"data"`
	Refs  map[string]any `json:"refs,omitempty"`
}

type Blob struct {
	Name   string `json:"blob"`
	Group  string `json:"group"`
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	MIME   string `json:"mime"`
	Size   int    `json:"size"`
	Data   []byte `json:"data"`
}

type File struct {
	Manifest Manifest
	Records  []Line
	Blobs    []Blob
}

type probe struct {
	Format *string `json:"format"`
	Blob   *string `json:"blob"`
}

func Read(r io.Reader) (File, error) {
	var file File
	reader := bufio.NewReader(r)
	array := false
	for {
		next, err := reader.ReadByte()
		if err != nil {
			break
		}
		if next == ' ' || next == '\t' || next == '\r' || next == '\n' {
			continue
		}
		array = next == '['
		_ = reader.UnreadByte()
		break
	}
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	label := "line"
	var items []json.RawMessage
	if array {
		label = "element"
		if err := decoder.Decode(&items); err != nil {
			return File{}, fmt.Errorf("the array: %w", err)
		}
		file.Manifest.Format = FormatVersion
	}
	first := !array
	take := func(index int, raw json.RawMessage) error {
		var kind probe
		if err := json.Unmarshal(raw, &kind); err != nil {
			return fmt.Errorf("%s %d: %w", label, index, err)
		}
		switch {
		case first:
			if kind.Format == nil {
				return errors.New("line 1 is not a manifest: the first line carries format")
			}
			if err := json.Unmarshal(raw, &file.Manifest); err != nil {
				return fmt.Errorf("line 1: %w", err)
			}
			if file.Manifest.Format != FormatVersion {
				return fmt.Errorf("format %s is not read here, only %s", file.Manifest.Format, FormatVersion)
			}
			first = false
		case kind.Format != nil && array && index == 1:
			file.Manifest = Manifest{}
			if err := json.Unmarshal(raw, &file.Manifest); err != nil {
				return fmt.Errorf("element 1: %w", err)
			}
			if file.Manifest.Format != FormatVersion {
				return fmt.Errorf("format %s is not read here, only %s", file.Manifest.Format, FormatVersion)
			}
		case kind.Format != nil:
			return fmt.Errorf("%s %d is a second manifest", label, index)
		case kind.Blob != nil:
			var blob Blob
			if err := json.Unmarshal(raw, &blob); err != nil {
				return fmt.Errorf("%s %d: %w", label, index, err)
			}
			if sum(blob.Data) != blob.SHA256 {
				return fmt.Errorf("%s %d: blob %s of %s %s does not match its sha256", label, index, blob.Name, blob.Group, blob.Key)
			}
			file.Blobs = append(file.Blobs, blob)
		default:
			line := Line{}
			lineDecoder := json.NewDecoder(bytes.NewReader(raw))
			lineDecoder.UseNumber()
			if err := lineDecoder.Decode(&line); err != nil {
				return fmt.Errorf("%s %d: %w", label, index, err)
			}
			if name, ok := defined(line.ID); ok && line.Key == "" {
				line.Key = name
			}
			if line.Group == "" || line.Key == "" {
				return fmt.Errorf("%s %d: a record carries group and key", label, index)
			}
			if line.Data == nil {
				line.Data = map[string]any{}
			}
			file.Records = append(file.Records, line)
		}
		return nil
	}
	if array {
		for index, raw := range items {
			if err := take(index+1, raw); err != nil {
				return File{}, err
			}
		}
	} else {
		for index := 1; ; index++ {
			var raw json.RawMessage
			err := decoder.Decode(&raw)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return File{}, fmt.Errorf("line %d: %w", index, err)
			}
			if err := take(index, raw); err != nil {
				return File{}, err
			}
		}
	}
	if first {
		return File{}, errors.New("the file is empty: it starts with a manifest line")
	}
	for _, summary := range file.Manifest.Groups {
		if summary.Hash == "" {
			continue
		}
		hash, err := file.hash(summary.Name)
		if err != nil {
			return File{}, err
		}
		if hash != summary.Hash {
			return File{}, fmt.Errorf("group %s does not match the hash in the manifest", summary.Name)
		}
	}
	return file, nil
}

func sum(data []byte) string {
	hashed := sha256.Sum256(data)
	return hex.EncodeToString(hashed[:])
}

func encode(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (f File) group(name string) ([]Line, []Blob) {
	var records []Line
	for _, record := range f.Records {
		if record.Group == name {
			records = append(records, record)
		}
	}
	var blobs []Blob
	for _, blob := range f.Blobs {
		if blob.Group == name {
			blobs = append(blobs, blob)
		}
	}
	return records, blobs
}

func (f File) hash(name string) (string, error) {
	records, blobs := f.group(name)
	sum := sha256.New()
	for _, record := range records {
		line, err := encode(record)
		if err != nil {
			return "", err
		}
		sum.Write(line)
	}
	for _, blob := range blobs {
		line, err := encode(blob)
		if err != nil {
			return "", err
		}
		sum.Write(line)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func (f File) Lines() ([]json.RawMessage, error) {
	var out bytes.Buffer
	if err := f.Write(&out); err != nil {
		return nil, err
	}
	var lines []json.RawMessage
	for _, line := range bytes.Split(bytes.TrimRight(out.Bytes(), "\n"), []byte("\n")) {
		lines = append(lines, json.RawMessage(line))
	}
	return lines, nil
}

func (f File) Write(w io.Writer) error {
	line, err := encode(f.Manifest)
	if err != nil {
		return err
	}
	if _, err := w.Write(line); err != nil {
		return err
	}
	written := map[string]bool{}
	names := make([]string, 0, len(f.Manifest.Groups))
	for _, summary := range f.Manifest.Groups {
		names = append(names, summary.Name)
		written[summary.Name] = true
	}
	for _, record := range f.Records {
		if !written[record.Group] {
			names = append(names, record.Group)
			written[record.Group] = true
		}
	}
	for _, name := range names {
		records, blobs := f.group(name)
		for _, record := range records {
			line, err := encode(record)
			if err != nil {
				return err
			}
			if _, err := w.Write(line); err != nil {
				return err
			}
		}
		for _, blob := range blobs {
			line, err := encode(blob)
			if err != nil {
				return err
			}
			if _, err := w.Write(line); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *File) summarize(names []string) error {
	f.Manifest.Groups = []Summary{}
	for _, name := range names {
		records, blobs := f.group(name)
		hash, err := f.hash(name)
		if err != nil {
			return err
		}
		f.Manifest.Groups = append(f.Manifest.Groups, Summary{Name: name, Count: len(records), Blobs: len(blobs), Hash: hash})
	}
	return nil
}

func ParseTarget(raw string) (Target, error) {
	name, filter, _ := strings.Cut(raw, ":")
	if name == "" {
		return Target{}, echo.NewHTTPError(http.StatusBadRequest, "a target is group or group:field=value,field=value, not "+raw)
	}
	target := Target{Group: name}
	if filter == "" {
		return target, nil
	}
	target.Filter = map[string]string{}
	for _, pair := range strings.Split(filter, ",") {
		field, value, ok := strings.Cut(pair, "=")
		if !ok || field == "" {
			return Target{}, echo.NewHTTPError(http.StatusBadRequest, "a target filter is field=value, not "+pair)
		}
		target.Filter[field] = value
	}
	return target, nil
}

func ParseKey(raw string) (string, string, error) {
	name, key, _ := strings.Cut(raw, ":")
	if name == "" || key == "" {
		return "", "", echo.NewHTTPError(http.StatusBadRequest, "a key is group:key, not "+raw)
	}
	return name, key, nil
}

func sortLines(lines []Line) {
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Key < lines[j].Key })
}

var number = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

func ReadMarkdown(r io.Reader, group, document string) (Line, error) {
	if document == "" {
		return Line{}, errors.New("group " + group + " takes no markdown document")
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return Line{}, err
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	data := map[string]any{}
	body := text
	if opening, rest, ok := strings.Cut(text, "\n"); ok && strings.TrimRight(opening, " \t\r") == "---" {
		var front []string
		closed := false
		for {
			var row string
			row, rest, ok = strings.Cut(rest, "\n")
			if t := strings.TrimRight(row, " \t\r"); t == "---" || t == "..." {
				closed = true
				break
			}
			front = append(front, strings.TrimRight(row, "\r"))
			if !ok {
				break
			}
		}
		if !closed {
			return Line{}, errors.New("the frontmatter opens with --- and never closes")
		}
		data, err = parseFrontmatter(front)
		if err != nil {
			return Line{}, fmt.Errorf("frontmatter: %w", err)
		}
		body = rest
	}
	line := Line{Group: group, Data: data}
	if id, ok := data["id"].(string); ok {
		if name, ok := defined(id); ok {
			delete(data, "id")
			line.ID, line.Key = id, name
		}
	}
	if value, ok := data["key"]; ok {
		delete(data, "key")
		switch keyed := value.(type) {
		case string:
			line.Key = keyed
		case json.Number:
			line.Key = keyed.String()
		default:
			return Line{}, errors.New("frontmatter: key is a string")
		}
	}
	data[document] = body
	return line, nil
}

type source struct {
	indent int
	text   string
	number int
}

func parseFrontmatter(rows []string) (map[string]any, error) {
	var lines []source
	for i, row := range rows {
		trimmed := strings.TrimLeft(row, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "\t") {
			return nil, fmt.Errorf("line %d: a tab indents nothing, use spaces", i+1)
		}
		lines = append(lines, source{indent: len(row) - len(trimmed), text: strings.TrimRight(trimmed, " "), number: i + 1})
	}
	if len(lines) == 0 {
		return map[string]any{}, nil
	}
	value, next, err := parseBlock(lines, 0, lines[0].indent)
	if err != nil {
		return nil, err
	}
	if next < len(lines) {
		return nil, fmt.Errorf("line %d: unexpected indentation", lines[next].number)
	}
	out, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("the frontmatter is fields, key: value, not a list")
	}
	return out, nil
}

func parseBlock(lines []source, at, indent int) (any, int, error) {
	if lines[at].text == "-" || strings.HasPrefix(lines[at].text, "- ") {
		list := []any{}
		for at < len(lines) && lines[at].indent == indent && (lines[at].text == "-" || strings.HasPrefix(lines[at].text, "- ")) {
			item := strings.TrimSpace(strings.TrimPrefix(lines[at].text, "-"))
			current := lines[at]
			at++
			if item == "" {
				if at < len(lines) && lines[at].indent > indent {
					nested, next, err := parseBlock(lines, at, lines[at].indent)
					if err != nil {
						return nil, 0, err
					}
					list = append(list, nested)
					at = next
					continue
				}
				list = append(list, nil)
				continue
			}
			if _, _, isField := splitField(item); isField && !strings.HasPrefix(item, "[") && !strings.HasPrefix(item, "{") && !strings.HasPrefix(item, "\"") && !strings.HasPrefix(item, "'") {
				return nil, 0, fmt.Errorf("line %d: a list of fields is not read here, a list item is a value", current.number)
			}
			value, err := parseScalar(item, current.number)
			if err != nil {
				return nil, 0, err
			}
			list = append(list, value)
		}
		return list, at, nil
	}
	fields := map[string]any{}
	for at < len(lines) && lines[at].indent == indent {
		current := lines[at]
		name, rest, ok := splitField(current.text)
		if !ok {
			return nil, 0, fmt.Errorf("line %d: %q is not key: value", current.number, current.text)
		}
		if _, taken := fields[name]; taken {
			return nil, 0, fmt.Errorf("line %d: %s is given twice", current.number, name)
		}
		at++
		if rest == "" {
			if at < len(lines) && (lines[at].indent > indent || (lines[at].indent == indent && (lines[at].text == "-" || strings.HasPrefix(lines[at].text, "- ")))) {
				nested, next, err := parseBlock(lines, at, lines[at].indent)
				if err != nil {
					return nil, 0, err
				}
				fields[name] = nested
				at = next
				continue
			}
			fields[name] = nil
			continue
		}
		if rest == "|" || rest == ">" || strings.HasPrefix(rest, "|-") || strings.HasPrefix(rest, ">-") || strings.HasPrefix(rest, "|+") || strings.HasPrefix(rest, ">+") {
			return nil, 0, fmt.Errorf("line %d: a block scalar is not read here, the body of the file is the document", current.number)
		}
		value, err := parseScalar(rest, current.number)
		if err != nil {
			return nil, 0, err
		}
		fields[name] = value
	}
	if at < len(lines) && lines[at].indent > indent {
		return nil, 0, fmt.Errorf("line %d: unexpected indentation", lines[at].number)
	}
	return fields, at, nil
}

func splitField(text string) (string, string, bool) {
	if strings.HasPrefix(text, "\"") || strings.HasPrefix(text, "'") {
		quote := text[:1]
		end := 1
		for end < len(text) {
			if text[end] == '\\' && quote == "\"" {
				end += 2
				continue
			}
			if text[end] == quote[0] {
				break
			}
			end++
		}
		if end >= len(text) {
			return "", "", false
		}
		name, err := parseScalar(text[:end+1], 0)
		after := text[end+1:]
		if err != nil || !strings.HasPrefix(after, ":") {
			return "", "", false
		}
		label, _ := name.(string)
		return label, strings.TrimSpace(after[1:]), true
	}
	for i := 0; i < len(text); i++ {
		if text[i] == ':' && (i == len(text)-1 || text[i+1] == ' ') {
			return strings.TrimSpace(text[:i]), strings.TrimSpace(text[i+1:]), true
		}
	}
	return "", "", false
}

func parseScalar(text string, row int) (any, error) {
	switch {
	case strings.HasPrefix(text, "\""):
		end := 1
		for end < len(text) && text[end] != '"' {
			if text[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(text) {
			return nil, fmt.Errorf("line %d: a quoted value never closes", row)
		}
		if after := strings.TrimSpace(text[end+1:]); after != "" && !strings.HasPrefix(after, "#") {
			return nil, fmt.Errorf("line %d: text after a quoted value", row)
		}
		var out string
		if err := json.Unmarshal([]byte(text[:end+1]), &out); err != nil {
			return nil, fmt.Errorf("line %d: %w", row, err)
		}
		return out, nil
	case strings.HasPrefix(text, "'"):
		var out strings.Builder
		end := 1
		for end < len(text) {
			if text[end] == '\'' {
				if end+1 < len(text) && text[end+1] == '\'' {
					out.WriteByte('\'')
					end += 2
					continue
				}
				break
			}
			out.WriteByte(text[end])
			end++
		}
		if end >= len(text) {
			return nil, fmt.Errorf("line %d: a quoted value never closes", row)
		}
		if after := strings.TrimSpace(text[end+1:]); after != "" && !strings.HasPrefix(after, "#") {
			return nil, fmt.Errorf("line %d: text after a quoted value", row)
		}
		return out.String(), nil
	case strings.HasPrefix(text, "[") || strings.HasPrefix(text, "{"):
		return parseFlow(text, row)
	}
	if cut := strings.Index(text, " #"); cut >= 0 {
		text = strings.TrimSpace(text[:cut])
	}
	switch text {
	case "", "~", "null", "Null", "NULL":
		return nil, nil
	case "true", "True", "TRUE":
		return true, nil
	case "false", "False", "FALSE":
		return false, nil
	}
	if number.MatchString(text) {
		return json.Number(text), nil
	}
	return text, nil
}

func parseFlow(text string, row int) (any, error) {
	closing := map[byte]byte{'[': ']', '{': '}'}[text[0]]
	if text[len(text)-1] != closing {
		return nil, fmt.Errorf("line %d: %q is not closed", row, text)
	}
	inner := text[1 : len(text)-1]
	var parts []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, inner[start:i])
			start = i + 1
		}
	}
	if strings.TrimSpace(inner[start:]) != "" {
		parts = append(parts, inner[start:])
	}
	if text[0] == '[' {
		list := make([]any, 0, len(parts))
		for _, part := range parts {
			value, err := parseScalar(strings.TrimSpace(part), row)
			if err != nil {
				return nil, err
			}
			list = append(list, value)
		}
		return list, nil
	}
	fields := make(map[string]any, len(parts))
	for _, part := range parts {
		name, rest, ok := splitField(strings.TrimSpace(part))
		if !ok {
			return nil, fmt.Errorf("line %d: %q is not key: value", row, strings.TrimSpace(part))
		}
		value, err := parseScalar(rest, row)
		if err != nil {
			return nil, err
		}
		fields[name] = value
	}
	return fields, nil
}
