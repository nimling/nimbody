package state

import (
	"net/http"
	"sort"

	"github.com/labstack/echo/v4"
)

func (s *service) targets(in []Target) ([]Target, error) {
	if len(in) == 0 {
		out := make([]Target, 0, len(s.groups))
		for _, g := range s.groups {
			out = append(out, Target{Group: g.name})
		}
		return out, nil
	}
	rank := map[string]int{}
	for index, g := range s.groups {
		rank[g.name] = index
	}
	for _, target := range in {
		g, ok := s.byName[target.Group]
		if !ok {
			return nil, echo.NewHTTPError(http.StatusBadRequest, "no group is called "+target.Group)
		}
		for field := range target.Filter {
			if !contains(g.filters, field) {
				return nil, echo.NewHTTPError(http.StatusBadRequest, "group "+g.name+" takes no filter "+field)
			}
		}
	}
	out := append([]Target{}, in...)
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Group] < rank[out[j].Group] })
	return out, nil
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func (s *service) export(c *Context, in ExportRequest) (File, error) {
	targets, err := s.targets(in.Targets)
	if err != nil {
		return File{}, err
	}
	scope := in.Targets
	if scope == nil {
		scope = []Target{}
	}
	file := File{Manifest: Manifest{Format: FormatVersion, Service: c.Service, Version: c.Version, Scope: scope, History: in.History, Blobs: in.Blobs}}
	keys := map[string]map[string]string{}
	taken := map[string]map[string]bool{}
	var names []string
	for _, target := range targets {
		g := s.byName[target.Group].resolved()
		if taken[g.name] == nil {
			taken[g.name] = map[string]bool{}
			keys[g.name] = map[string]string{}
			names = append(names, g.name)
		}
		step := "export:" + g.name
		c.Steps.Start(step, "export "+g.name, "")
		rows, err := g.export(c, Query{Filter: target.Filter, Keys: target.Keys, History: in.History})
		if err != nil {
			c.Steps.Fail(step, err)
			return File{}, err
		}
		var kept []exported
		for _, row := range rows {
			if taken[g.name][row.key] {
				continue
			}
			taken[g.name][row.key] = true
			keys[g.name][row.id] = row.key
			kept = append(kept, row)
		}
		var lines []Line
		var blobs []Blob
		for _, row := range kept {
			line, err := s.line(c, g, row, keys)
			if err != nil {
				c.Steps.Fail(step, err)
				return File{}, err
			}
			lines = append(lines, line)
			if !in.Blobs || row.get == nil {
				continue
			}
			got, err := row.get(c)
			if err != nil {
				c.Steps.Fail(step, err)
				return File{}, err
			}
			for _, blob := range got {
				blob.Group, blob.Key = g.name, row.key
				blob.SHA256, blob.Size = sum(blob.Data), len(blob.Data)
				blobs = append(blobs, blob)
			}
		}
		sortLines(lines)
		sort.SliceStable(blobs, func(i, j int) bool {
			if blobs[i].Key != blobs[j].Key {
				return blobs[i].Key < blobs[j].Key
			}
			return blobs[i].Name < blobs[j].Name
		})
		file.Records = append(file.Records, lines...)
		file.Blobs = append(file.Blobs, blobs...)
		c.Steps.Done(step, Summary{Name: g.name, Count: len(lines), Blobs: len(blobs)})
	}
	if err := file.summarize(names); err != nil {
		return File{}, err
	}
	return file, nil
}

func (s *service) line(c *Context, g group, row exported, keys map[string]map[string]string) (Line, error) {
	data := make(map[string]any, len(row.data))
	for field, value := range row.data {
		data[field] = value
	}
	for _, pointer := range g.secrets(row.data) {
		data = strip(data, pointer)
	}
	refs := map[string]any{}
	for field, target := range g.refs {
		switch value := data[field].(type) {
		case string:
			if value == "" {
				continue
			}
			key, err := s.key(c, target, value, keys)
			if err != nil {
				return Line{}, err
			}
			refs[field] = key
			delete(data, field)
		case []any:
			list := make([]any, 0, len(value))
			for _, item := range value {
				id, ok := item.(string)
				if !ok {
					return Line{}, echo.NewHTTPError(http.StatusInternalServerError, "reference "+g.name+"."+field+" holds a value that is not an id")
				}
				key, err := s.key(c, target, id, keys)
				if err != nil {
					return Line{}, err
				}
				list = append(list, key)
			}
			refs[field] = list
			delete(data, field)
		}
	}
	line := Line{Group: g.name, Key: row.key, ID: row.id, Data: data}
	if len(refs) > 0 {
		line.Refs = refs
	}
	return line, nil
}

func (s *service) key(c *Context, group, id string, keys map[string]map[string]string) (string, error) {
	if key, ok := keys[group][id]; ok {
		return key, nil
	}
	if c.Keys != nil {
		key, err := c.Keys.GetKey(c.Request().Context(), group, id)
		if err != nil {
			return "", err
		}
		if key != "" {
			if keys[group] == nil {
				keys[group] = map[string]string{}
			}
			keys[group][id] = key
			return key, nil
		}
	}
	return id, nil
}
