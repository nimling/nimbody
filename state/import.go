package state

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/labstack/echo/v4"
)

type removal struct {
	key   string
	id    string
	owner string
}

type run struct {
	c       *Context
	mode    string
	dryRun  bool
	blobs   bool
	ids     map[string]map[string]string
	keys    map[string]map[string]string
	planned map[string]map[string]bool
	names   *placeholders
	out     ImportResponse
}

func (s *service) importFile(c *Context, in ImportRequest, file File) (ImportResponse, error) {
	mode := in.Mode
	if mode == "" {
		mode = ModeUpsert
	}
	if mode != ModeUpsert && mode != ModeCreate && mode != ModeSync && mode != ModeSkip && mode != ModeReplace && mode != ModeMerge && mode != ModeLatest {
		return ImportResponse{}, echo.NewHTTPError(http.StatusBadRequest, "mode is upsert, create, sync, skip, replace, merge or latest, not "+mode)
	}
	selected := map[string]bool{}
	if len(in.Targets) > 0 {
		if _, err := s.targets(in.Targets); err != nil {
			return ImportResponse{}, err
		}
		for _, target := range in.Targets {
			selected[target.Group] = true
		}
	}
	incoming := map[string][]Line{}
	for _, line := range file.Records {
		if len(selected) > 0 && !selected[line.Group] {
			continue
		}
		if _, ok := s.byName[line.Group]; !ok {
			return ImportResponse{}, echo.NewHTTPError(http.StatusBadRequest, "no group is called "+line.Group)
		}
		incoming[line.Group] = append(incoming[line.Group], line)
	}
	r := &run{
		c:       c,
		mode:    mode,
		dryRun:  in.DryRun,
		blobs:   file.Manifest.Blobs,
		ids:     map[string]map[string]string{},
		keys:    map[string]map[string]string{},
		planned: map[string]map[string]bool{},
		out:     ImportResponse{Mode: mode, DryRun: in.DryRun, Counts: map[string]int{}, Results: []Result{}},
	}
	for name, lines := range incoming {
		r.planned[name] = map[string]bool{}
		for _, line := range lines {
			r.planned[name][line.Key] = true
		}
	}
	removals := map[string][]removal{}
	for name, keys := range in.Delete {
		if _, ok := s.byName[name]; !ok {
			return ImportResponse{}, echo.NewHTTPError(http.StatusBadRequest, "no group is called "+name)
		}
		for _, key := range keys {
			if r.planned[name][key] {
				return ImportResponse{}, echo.NewHTTPError(http.StatusBadRequest, name+" "+key+" is both imported and deleted")
			}
			removals[name] = append(removals[name], removal{key: key})
		}
	}

	sequence, err := s.placeholders(r, incoming)
	if err != nil {
		return ImportResponse{}, err
	}

	if mode == ModeSync || mode == ModeReplace {
		if err := s.prune(r, in, file, incoming, removals); err != nil {
			return ImportResponse{}, err
		}
	}
	for i := len(s.groups) - 1; i >= 0; i-- {
		g := s.groups[i]
		if len(removals[g.name]) == 0 {
			continue
		}
		step := "delete:" + g.name
		c.Steps.Start(step, "delete "+g.name, "")
		items, err := s.owners(r, g, removals[g.name])
		if err != nil {
			c.Steps.Fail(step, err)
			return ImportResponse{}, err
		}
		for _, item := range items {
			r.report(step, "delete "+g.name+" "+item.key, s.deleteRecord(r, g, item))
		}
		c.Steps.Done(step, nil)
	}

	blobs := map[string]map[string][]Blob{}
	for _, blob := range file.Blobs {
		if blobs[blob.Group] == nil {
			blobs[blob.Group] = map[string][]Blob{}
		}
		blobs[blob.Group][blob.Key] = append(blobs[blob.Group][blob.Key], blob)
	}

	groups := map[string]group{}
	current := map[string]map[string]exported{}
	left := map[string]int{}
	for _, line := range sequence {
		left[line.Group]++
	}
	for _, line := range sequence {
		step := "import:" + line.Group
		g, started := groups[line.Group]
		if !started {
			g = s.byName[line.Group].resolved()
			groups[g.name] = g
			c.Steps.Start(step, "import "+g.name, "")
			rows, err := s.current(r, g, incoming[g.name])
			if err != nil {
				c.Steps.Fail(step, err)
				return ImportResponse{}, err
			}
			current[g.name] = rows
		}
		r.report(step, "import "+g.name+" "+line.Key, s.importLine(r, g, line, current[g.name][line.Key], blobs[g.name][line.Key]))
		left[g.name]--
		if left[g.name] == 0 {
			c.Steps.Done(step, nil)
		}
	}

	return r.out, nil
}

func (r *run) report(parent, label string, result Result) {
	r.out.Results = append(r.out.Results, result)
	r.out.Counts[result.Outcome]++
	id := parent + ":" + result.Key
	r.c.Steps.Start(id, label, parent)
	if result.Outcome == OutcomeRefused {
		r.c.Steps.Fail(id, errors.New(result.Error))
	} else {
		r.c.Steps.Done(id, result)
	}
	if r.c.Progress != nil {
		r.c.Progress(r.c.Context, result)
	}
}

func (r *run) refuse(result Result, err error) Result {
	result.Outcome = OutcomeRefused
	var rejected *echo.HTTPError
	if errors.As(err, &rejected) {
		result.Error = message(rejected)
		return result
	}
	r.c.Logger().Error("state import " + result.Group + " " + result.Key + ": " + err.Error())
	result.Error = "the server failed on " + result.Group + " " + result.Key
	return result
}

func (r *run) id(group, key string) (string, error) {
	if id, ok := r.ids[group][key]; ok {
		return id, nil
	}
	if r.c.Keys == nil {
		return "", nil
	}
	return r.c.Keys.GetID(r.c.Request().Context(), group, key)
}

func (r *run) remember(group, key, id string) {
	if r.ids[group] == nil {
		r.ids[group] = map[string]string{}
	}
	r.ids[group][key] = id
	if id == "" {
		return
	}
	if r.keys[group] == nil {
		r.keys[group] = map[string]string{}
	}
	r.keys[group][id] = key
}

func (r *run) write(do func() error) error {
	if r.c.Transaction == nil {
		return do()
	}
	req := r.c.Request()
	defer r.c.SetRequest(req)
	return r.c.Transaction(req.Context(), func(tx context.Context) error {
		r.c.SetRequest(req.WithContext(tx))
		return do()
	})
}

func (s *service) current(r *run, g group, lines []Line) (map[string]exported, error) {
	keys := make([]string, 0, len(lines))
	for _, line := range lines {
		keys = append(keys, line.Key)
	}
	r.c.Organization = ""
	rows, err := g.export(r.c, Query{Keys: keys})
	if err != nil {
		return nil, err
	}
	out := make(map[string]exported, len(rows))
	for _, row := range rows {
		out[row.key] = row
		r.remember(g.name, row.key, row.id)
	}
	return out, nil
}

func (s *service) importLine(r *run, g group, line Line, existing exported, blobs []Blob) Result {
	result := Result{Key: line.Key, Group: g.name}
	if r.names != nil {
		data, err := r.names.fill(g, line.Data)
		if err != nil {
			return r.refuse(result, err)
		}
		line.Data = data
	}
	id, err := r.id(g.name, line.Key)
	if err != nil {
		return r.refuse(result, err)
	}
	owner := g.named(line.Data)
	if id != "" {
		owner = g.named(existing.data)
	}
	if err := r.c.gate(owner); err != nil {
		return r.refuse(result, err)
	}
	if id != "" && r.mode == ModeSkip {
		result.ID = id
		r.remember(g.name, line.Key, id)
		result.Outcome = OutcomeUnchanged
		return result
	}
	if id != "" && r.mode == ModeLatest && existing.key != "" {
		newer, err := changedLast(g, line, existing)
		if err != nil {
			return r.refuse(result, err)
		}
		if !newer {
			result.ID = id
			r.remember(g.name, line.Key, id)
			result.Outcome = OutcomeUnchanged
			return result
		}
	}
	data, err := s.resolve(r, g, line)
	if err != nil {
		return r.refuse(result, err)
	}
	if id == "" {
		result.Outcome = OutcomeCreated
		if r.dryRun {
			r.remember(g.name, line.Key, "")
			return result
		}
		err := r.write(func() error {
			written, put, err := g.write(r.c, "", line.ID, line.Key, data)
			if err != nil {
				return err
			}
			id = written
			return putBlobs(r.c, put, blobs)
		})
		if err != nil {
			return r.refuse(result, err)
		}
		r.remember(g.name, line.Key, id)
		result.ID = id
		return result
	}
	result.ID = id
	r.remember(g.name, line.Key, id)

	same, immutable := false, ""
	if existing.key != "" {
		current, err := s.line(r.c, g, existing, r.keys)
		if err != nil {
			return r.refuse(result, err)
		}
		if r.mode == ModeMerge {
			merged := make(map[string]any, len(existing.data)+len(data))
			for field, value := range existing.data {
				merged[field] = value
			}
			for field, value := range data {
				merged[field] = value
			}
			data = merged
			line = Line{Group: line.Group, Key: line.Key, ID: line.ID, Data: overlay(current.Data, line.Data), Refs: overlay(current.Refs, line.Refs)}
		}
		same, immutable = compare(g, line, current)
	}
	var stored []Blob
	if r.blobs && existing.get != nil {
		got, err := existing.get(r.c)
		if err != nil {
			return r.refuse(result, err)
		}
		stored = got
	}
	changed := changedBlobs(blobs, stored, r.blobs)
	switch {
	case same && len(changed) == 0:
		result.Outcome = OutcomeUnchanged
		return result
	case r.mode == ModeCreate:
		return r.refuse(result, echo.NewHTTPError(http.StatusConflict, g.name+" "+line.Key+" exists and differs, import with mode upsert to update it"))
	case immutable != "":
		return r.refuse(result, echo.NewHTTPError(http.StatusConflict, g.name+"."+immutable+" cannot change, delete "+g.name+":"+line.Key+" and import it again"))
	}
	result.Outcome = OutcomeUpdated
	if r.dryRun {
		return result
	}
	err = r.write(func() error {
		put := existing.put
		if !same {
			_, written, err := g.write(r.c, id, "", line.Key, data)
			if err != nil {
				return err
			}
			put = written
		}
		return putBlobs(r.c, put, changed)
	})
	if err != nil {
		return r.refuse(result, err)
	}
	return result
}

func (s *service) resolve(r *run, g group, line Line) (map[string]any, error) {
	data := make(map[string]any, len(line.Data)+len(line.Refs))
	for field, value := range line.Data {
		data[field] = value
	}
	for field, value := range line.Refs {
		target, ok := g.refs[field]
		if !ok {
			return nil, echo.NewHTTPError(http.StatusBadRequest, g.name+"."+field+" is not a reference")
		}
		switch keyed := value.(type) {
		case string:
			id, err := s.reference(r, g, field, target, keyed)
			if err != nil {
				return nil, err
			}
			data[field] = id
		case []any:
			ids := make([]any, 0, len(keyed))
			for _, item := range keyed {
				key, ok := item.(string)
				if !ok {
					return nil, echo.NewHTTPError(http.StatusBadRequest, g.name+"."+field+" holds a reference that is not a key")
				}
				id, err := s.reference(r, g, field, target, key)
				if err != nil {
					return nil, err
				}
				ids = append(ids, id)
			}
			data[field] = ids
		default:
			return nil, echo.NewHTTPError(http.StatusBadRequest, g.name+"."+field+" holds a reference that is not a key")
		}
	}
	return data, nil
}

func (s *service) reference(r *run, g group, field, target, key string) (string, error) {
	id, err := r.id(target, key)
	if err != nil {
		return "", err
	}
	if id == "" && !(r.dryRun && r.planned[target][key]) {
		return "", echo.NewHTTPError(http.StatusUnprocessableEntity, g.name+"."+field+" names "+target+" "+key+", which does not exist")
	}
	return id, nil
}

func compare(g group, incoming, current Line) (bool, string) {
	want := map[string]any{}
	have := map[string]any{}
	same := true
	for field, value := range incoming.Data {
		want[field] = value
	}
	for _, pointer := range g.secrets(incoming.Data) {
		if _, sent := lookup(want, pointer); sent {
			same = false
			want = strip(want, pointer)
		}
	}
	for field, value := range current.Data {
		have[field] = value
	}
	for field, value := range incoming.Refs {
		want[field] = value
	}
	for field, value := range current.Refs {
		have[field] = value
	}
	for _, pointer := range g.fields.ServerDefault {
		if _, sent := lookup(want, pointer); !sent {
			have = strip(have, pointer)
		}
	}
	immutable := ""
	for _, pointer := range g.fields.Immutable {
		wanted, sent := lookup(want, pointer)
		if !sent {
			continue
		}
		held, _ := lookup(have, pointer)
		if !equal(wanted, held) {
			immutable = dotted(pointer)
			break
		}
	}
	return same && equal(want, have), immutable
}

func overlay(held, sent map[string]any) map[string]any {
	out := make(map[string]any, len(held)+len(sent))
	for field, value := range held {
		out[field] = value
	}
	for field, value := range sent {
		out[field] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func changedLast(g group, incoming Line, existing exported) (bool, error) {
	if g.updated == "" {
		return false, echo.NewHTTPError(http.StatusBadRequest, "group "+g.name+" declares no update time, so mode latest cannot compare its records")
	}
	sent, ok := lookup(incoming.Data, g.updated)
	if !ok || sent == nil {
		return false, nil
	}
	incomingTime, err := parseTime(g, sent)
	if err != nil {
		return false, err
	}
	held, ok := lookup(existing.data, g.updated)
	if !ok || held == nil {
		return true, nil
	}
	existingTime, err := parseTime(g, held)
	if err != nil {
		return false, err
	}
	return incomingTime.After(existingTime), nil
}

func parseTime(g group, value any) (time.Time, error) {
	text, ok := value.(string)
	if ok {
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, echo.NewHTTPError(http.StatusBadRequest, g.name+"."+dotted(g.updated)+" is not an RFC 3339 time")
}

func equal(left, right any) bool {
	a, errA := encode(left)
	b, errB := encode(right)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func changedBlobs(incoming, stored []Blob, inline bool) []Blob {
	if !inline {
		return nil
	}
	have := map[string]string{}
	for _, blob := range stored {
		have[blob.Name] = blob.SHA256
		if blob.SHA256 == "" {
			have[blob.Name] = sum(blob.Data)
		}
	}
	var changed []Blob
	for _, blob := range incoming {
		if have[blob.Name] != blob.SHA256 {
			changed = append(changed, blob)
		}
	}
	return changed
}

func putBlobs(c *Context, put func(ctx *Context, blob Blob) error, blobs []Blob) error {
	if len(blobs) == 0 {
		return nil
	}
	if put == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "this group takes no blobs")
	}
	for _, blob := range blobs {
		if err := put(c, blob); err != nil {
			return err
		}
	}
	return nil
}

func (s *service) prune(r *run, in ImportRequest, file File, incoming map[string][]Line, removals map[string][]removal) error {
	scope := in.Targets
	if r.mode == ModeReplace {
		scope = make([]Target, 0, len(s.groups))
		for _, g := range s.groups {
			scope = append(scope, Target{Group: g.name})
		}
	}
	if len(scope) == 0 {
		scope = file.Manifest.Scope
	}
	if len(scope) == 0 {
		seen := map[string]bool{}
		for _, summary := range file.Manifest.Groups {
			if _, ok := s.byName[summary.Name]; ok && !seen[summary.Name] {
				seen[summary.Name] = true
				scope = append(scope, Target{Group: summary.Name})
			}
		}
		for name := range incoming {
			if !seen[name] {
				seen[name] = true
				scope = append(scope, Target{Group: name})
			}
		}
	}
	targets, err := s.targets(scope)
	if err != nil {
		return err
	}
	for _, target := range targets {
		g := s.byName[target.Group]
		r.c.Organization = ""
		rows, err := g.export(r.c, Query{Filter: target.Filter, Keys: target.Keys})
		if err != nil {
			return err
		}
		listed := map[string]bool{}
		for _, item := range removals[g.name] {
			listed[item.key] = true
		}
		for _, row := range rows {
			if r.planned[g.name][row.key] || listed[row.key] {
				continue
			}
			listed[row.key] = true
			removals[g.name] = append(removals[g.name], removal{key: row.key, id: row.id, owner: g.named(row.data)})
		}
	}
	return nil
}

func (s *service) owners(r *run, g group, items []removal) ([]removal, error) {
	if g.owner == "" {
		return items, nil
	}
	var keys []string
	for _, item := range items {
		if item.id == "" {
			keys = append(keys, item.key)
		}
	}
	if len(keys) == 0 {
		return items, nil
	}
	r.c.Organization = ""
	rows, err := g.export(r.c, Query{Keys: keys})
	if err != nil {
		return nil, err
	}
	named := make(map[string]string, len(rows))
	for _, row := range rows {
		named[row.key] = g.named(row.data)
	}
	out := make([]removal, 0, len(items))
	for _, item := range items {
		if item.id == "" {
			item.owner = named[item.key]
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *service) deleteRecord(r *run, g group, item removal) Result {
	result := Result{Key: item.key, Group: g.name, ID: item.id}
	if err := r.c.gate(item.owner); err != nil {
		return r.refuse(result, err)
	}
	if result.ID == "" {
		id, err := r.id(g.name, item.key)
		if err != nil {
			return r.refuse(result, err)
		}
		if id == "" {
			return r.refuse(result, echo.NewHTTPError(http.StatusNotFound, g.name+" "+item.key+" does not exist"))
		}
		result.ID = id
	}
	if g.delete == nil {
		return r.refuse(result, echo.NewHTTPError(http.StatusBadRequest, "group "+g.name+" deletes nothing"))
	}
	result.Outcome = OutcomeDeleted
	if r.dryRun {
		return result
	}
	if err := r.write(func() error { return g.delete(r.c, result.ID) }); err != nil {
		return r.refuse(result, err)
	}
	return result
}

func orderLines(g group, lines []Line) []Line {
	fields := make([]string, 0)
	for field, target := range g.refs {
		if target == g.name {
			fields = append(fields, field)
		}
	}
	if len(fields) == 0 {
		return lines
	}
	sort.Strings(fields)
	index := make(map[string]int, len(lines))
	for i, line := range lines {
		index[line.Key] = i
	}
	visited := make([]bool, len(lines))
	out := make([]Line, 0, len(lines))
	var visit func(i int)
	visit = func(i int) {
		if visited[i] {
			return
		}
		visited[i] = true
		for _, field := range fields {
			for _, key := range refKeys(lines[i].Refs[field]) {
				if j, ok := index[key]; ok {
					visit(j)
				}
			}
		}
		out = append(out, lines[i])
	}
	for i := range lines {
		visit(i)
	}
	return out
}

func refKeys(value any) []string {
	switch keyed := value.(type) {
	case string:
		return []string{keyed}
	case []any:
		out := make([]string, 0, len(keyed))
		for _, item := range keyed {
			if key, ok := item.(string); ok {
				out = append(out, key)
			}
		}
		return out
	}
	return nil
}
