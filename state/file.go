package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	decoder := json.NewDecoder(r)
	decoder.UseNumber()
	first := true
	for index := 1; ; index++ {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return File{}, fmt.Errorf("line %d: %w", index, err)
		}
		var kind probe
		if err := json.Unmarshal(raw, &kind); err != nil {
			return File{}, fmt.Errorf("line %d: %w", index, err)
		}
		switch {
		case first:
			if kind.Format == nil {
				return File{}, errors.New("line 1 is not a manifest: the first line carries format")
			}
			if err := json.Unmarshal(raw, &file.Manifest); err != nil {
				return File{}, fmt.Errorf("line 1: %w", err)
			}
			if file.Manifest.Format != FormatVersion {
				return File{}, fmt.Errorf("format %s is not read here, only %s", file.Manifest.Format, FormatVersion)
			}
			first = false
		case kind.Format != nil:
			return File{}, fmt.Errorf("line %d is a second manifest", index)
		case kind.Blob != nil:
			var blob Blob
			if err := json.Unmarshal(raw, &blob); err != nil {
				return File{}, fmt.Errorf("line %d: %w", index, err)
			}
			if sum(blob.Data) != blob.SHA256 {
				return File{}, fmt.Errorf("line %d: blob %s of %s %s does not match its sha256", index, blob.Name, blob.Group, blob.Key)
			}
			file.Blobs = append(file.Blobs, blob)
		default:
			line := Line{}
			lineDecoder := json.NewDecoder(bytes.NewReader(raw))
			lineDecoder.UseNumber()
			if err := lineDecoder.Decode(&line); err != nil {
				return File{}, fmt.Errorf("line %d: %w", index, err)
			}
			if line.Group == "" || line.Key == "" {
				return File{}, fmt.Errorf("line %d: a record carries group and key", index)
			}
			if line.Data == nil {
				line.Data = map[string]any{}
			}
			file.Records = append(file.Records, line)
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
