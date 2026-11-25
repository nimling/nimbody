package state

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"

	"github.com/labstack/echo/v4"
)

const (
	placeholderOpen  = "{{"
	placeholderClose = "}}"
)

type object map[string]any

func (o object) String() string {
	id, _ := o["id"].(string)
	return id
}

type use struct {
	name   string
	fields []string
}

type placeholder struct {
	uses   []use
	single bool
	text   *template.Template
}

type definition struct {
	group string
	key   string
}

type placeholders struct {
	s       *service
	r       *run
	names   map[string]definition
	host    map[string]any
	lenient bool
	parsed  map[string]*placeholder
	set     *template.Template
	objects map[string]object
	group   group
	field   string
}

func parsePlaceholder(text string) (*parse.Tree, []use, bool, error) {
	refused := echo.NewHTTPError(http.StatusBadRequest, text+" is not a placeholder: a placeholder is {{name}} or {{name.field}}")
	tree := parse.New("state")
	tree.Mode = parse.SkipFuncCheck
	if _, err := tree.Parse(text, "", "", map[string]*parse.Tree{}); err != nil {
		return nil, nil, false, refused
	}
	var uses []use
	for _, node := range tree.Root.Nodes {
		action, ok := node.(*parse.ActionNode)
		if !ok {
			if _, text := node.(*parse.TextNode); text {
				continue
			}
			return nil, nil, false, refused
		}
		if len(action.Pipe.Decl) != 0 || len(action.Pipe.Cmds) != 1 || len(action.Pipe.Cmds[0].Args) != 1 {
			return nil, nil, false, refused
		}
		switch arg := action.Pipe.Cmds[0].Args[0].(type) {
		case *parse.IdentifierNode:
			uses = append(uses, use{name: arg.Ident})
		case *parse.ChainNode:
			named, ok := arg.Node.(*parse.IdentifierNode)
			if !ok {
				return nil, nil, false, refused
			}
			uses = append(uses, use{name: named.Ident, fields: arg.Field})
		case *parse.StringNode:
		default:
			return nil, nil, false, refused
		}
	}
	return tree, uses, len(tree.Root.Nodes) == 1 && len(uses) == 1, nil
}

func defined(id string) (string, bool) {
	if !strings.Contains(id, placeholderOpen) {
		return "", false
	}
	_, uses, single, err := parsePlaceholder(id)
	if err != nil || !single || len(uses[0].fields) != 0 {
		return "", false
	}
	return uses[0].name, true
}

func (p *placeholders) parse(text string) (*placeholder, error) {
	if held, ok := p.parsed[text]; ok {
		return held, nil
	}
	source := text
	if p.lenient {
		literal, active := p.literal(text)
		if !active {
			p.parsed[text] = &placeholder{}
			return p.parsed[text], nil
		}
		source = literal
	}
	tree, uses, single, err := parsePlaceholder(source)
	if err != nil {
		return nil, err
	}
	held := &placeholder{uses: uses, single: single}
	if !single {
		held.text, err = p.set.AddParseTree("p"+strconv.Itoa(len(p.parsed)), tree)
		if err != nil {
			return nil, err
		}
	}
	p.parsed[text] = held
	return held, nil
}

func (p *placeholders) literal(text string) (string, bool) {
	var out strings.Builder
	active := false
	rest := text
	for {
		start := strings.Index(rest, placeholderOpen)
		if start < 0 {
			out.WriteString(rest)
			return out.String(), active
		}
		out.WriteString(rest[:start])
		rest = rest[start:]
		end := strings.Index(rest, placeholderClose)
		if end < 0 {
			out.WriteString(placeholderOpen + strconv.Quote(rest) + placeholderClose)
			return out.String(), active
		}
		segment := rest[:end+len(placeholderClose)]
		rest = rest[len(segment):]
		_, uses, single, err := parsePlaceholder(segment)
		if err == nil && single && p.hosted(uses[0].name) {
			out.WriteString(segment)
			active = true
			continue
		}
		out.WriteString(placeholderOpen + strconv.Quote(segment) + placeholderClose)
	}
}

func (p *placeholders) hosted(name string) bool {
	_, ok := p.host[name]
	return ok
}

func (p *placeholders) supplied(lines map[string][]Line) error {
	if p.r.c.Names == nil {
		return nil
	}
	if len(p.names) == 0 {
		used := false
		for _, held := range lines {
			for _, line := range held {
				used = used || braced(line.Data) || braced(line.Refs)
			}
		}
		if !used {
			return nil
		}
	}
	supplied, err := p.r.c.Names(p.r.c.Context)
	if err != nil {
		return err
	}
	body, err := json.Marshal(supplied)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, &p.host); err != nil {
		return err
	}
	for name := range p.host {
		if held, ok := defined(placeholderOpen + name + placeholderClose); !ok || held != name {
			return echo.NewHTTPError(http.StatusInternalServerError, "the service supplies the name "+name+", which {{name}} cannot write")
		}
	}
	clashes := map[string]bool{}
	for name := range p.names {
		if p.hosted(name) {
			clashes[name] = true
		}
	}
	for _, name := range sorted(clashes) {
		taken := p.names[name]
		return echo.NewHTTPError(http.StatusBadRequest, "{{"+name+"}} is defined by "+taken.group+" "+taken.key+" and is a name the service supplies")
	}
	return nil
}

func braced(value any) bool {
	switch held := value.(type) {
	case string:
		return strings.Contains(held, placeholderOpen)
	case map[string]any:
		for _, item := range held {
			if braced(item) {
				return true
			}
		}
	case []any:
		for _, item := range held {
			if braced(item) {
				return true
			}
		}
	}
	return false
}

func (p *placeholders) key(text string) (string, bool, error) {
	if !strings.Contains(text, placeholderOpen) {
		return "", false, nil
	}
	held, err := p.parse(text)
	if err != nil {
		return "", false, err
	}
	if !held.single || len(held.uses[0].fields) > 1 || (len(held.uses[0].fields) == 1 && held.uses[0].fields[0] != "id") {
		return "", false, nil
	}
	if p.hosted(held.uses[0].name) {
		return "", false, nil
	}
	if _, ok := p.names[held.uses[0].name]; !ok {
		return "", false, echo.NewHTTPError(http.StatusBadRequest, text+" names a record that no record defines")
	}
	return held.uses[0].name, true, nil
}

func (p *placeholders) needs(value any, into map[string]bool) error {
	switch held := value.(type) {
	case string:
		if !strings.Contains(held, placeholderOpen) {
			return nil
		}
		parsed, err := p.parse(held)
		if err != nil {
			return err
		}
		for _, used := range parsed.uses {
			if !p.hosted(used.name) {
				into[used.name] = true
			}
		}
	case map[string]any:
		for _, item := range held {
			if err := p.needs(item, into); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range held {
			if err := p.needs(item, into); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *service) placeholders(r *run, incoming map[string][]Line) ([]Line, error) {
	p := &placeholders{s: s, r: r, names: map[string]definition{}, parsed: map[string]*placeholder{}, objects: map[string]object{}}
	for _, g := range s.groups {
		for index, line := range incoming[g.name] {
			if !strings.Contains(line.ID, placeholderOpen) {
				continue
			}
			name, ok := defined(line.ID)
			if !ok {
				return nil, echo.NewHTTPError(http.StatusBadRequest, g.name+" "+line.Key+" carries the id "+line.ID+", which is not {{name}}")
			}
			if line.Key != name {
				return nil, echo.NewHTTPError(http.StatusBadRequest, g.name+" "+line.Key+" defines {{"+name+"}}, so its key is "+name)
			}
			if taken, ok := p.names[name]; ok {
				return nil, echo.NewHTTPError(http.StatusBadRequest, "{{"+name+"}} is defined by "+taken.group+" "+taken.key+" and again by "+g.name+" "+line.Key)
			}
			p.names[name] = definition{group: g.name, key: line.Key}
			incoming[g.name][index].ID = ""
		}
	}
	if err := p.supplied(incoming); err != nil {
		return nil, err
	}
	p.lenient = len(p.names) == 0
	var base []Line
	if len(p.names) == 0 && len(p.host) == 0 {
		for _, g := range s.groups {
			base = append(base, orderLines(g, incoming[g.name])...)
		}
		return base, nil
	}
	funcs := template.FuncMap{}
	for name := range p.names {
		funcs[name] = func() (object, error) { return p.object(name) }
	}
	for name, value := range p.host {
		funcs[name] = func() any { return value }
	}
	p.set = template.New("state").Option("missingkey=error").Funcs(funcs)
	for _, g := range s.groups {
		for index, line := range incoming[g.name] {
			data, refs := line.Data, line.Refs
			moved := false
			move := func(field, name string) {
				if !moved {
					data, refs, moved = overlay(line.Data, nil), overlay(line.Refs, nil), true
					if data == nil {
						data = map[string]any{}
					}
					if refs == nil {
						refs = map[string]any{}
					}
				}
				delete(data, field)
				refs[field] = name
			}
			for field := range g.refs {
				if text, ok := line.Data[field].(string); ok {
					name, ok, err := p.key(text)
					if err != nil {
						return nil, err
					}
					if ok {
						move(field, name)
					}
				}
				switch value := line.Refs[field].(type) {
				case string:
					name, ok, err := p.key(value)
					if err != nil {
						return nil, err
					}
					if ok {
						move(field, name)
					}
				case []any:
					keys := make([]any, 0, len(value))
					for _, item := range value {
						text, _ := item.(string)
						name, ok, err := p.key(text)
						if err != nil {
							return nil, err
						}
						if ok {
							item = name
						}
						keys = append(keys, item)
					}
					move(field, "")
					refs[field] = keys
				}
			}
			incoming[g.name][index].Data, incoming[g.name][index].Refs = data, refs
		}
	}
	var needs []map[string]bool
	for _, g := range s.groups {
		for _, line := range orderLines(g, incoming[g.name]) {
			needed := map[string]bool{}
			if err := p.needs(line.Data, needed); err != nil {
				return nil, err
			}
			for field, value := range line.Refs {
				if g.refs[field] == "" {
					continue
				}
				for _, key := range refKeys(value) {
					if held, ok := p.names[key]; ok && held.group == g.refs[field] {
						needed[key] = true
					}
				}
			}
			for name := range needed {
				if _, ok := p.names[name]; !ok {
					return nil, echo.NewHTTPError(http.StatusBadRequest, g.name+" "+line.Key+" names {{"+name+"}}, which no record defines")
				}
			}
			base = append(base, line)
			needs = append(needs, needed)
		}
	}
	at := map[definition]int{}
	for index, line := range base {
		at[definition{group: line.Group, key: line.Key}] = index
	}
	state := make([]int, len(base))
	out := make([]Line, 0, len(base))
	var visit func(index int, path []string) error
	visit = func(index int, path []string) error {
		line := base[index]
		path = append(path, line.Group+" "+line.Key)
		switch state[index] {
		case 1:
			return echo.NewHTTPError(http.StatusBadRequest, "placeholders need each other in a cycle: "+strings.Join(path, " -> "))
		case 2:
			return nil
		}
		state[index] = 1
		for _, name := range sorted(needs[index]) {
			if err := visit(at[p.names[name]], path); err != nil {
				return err
			}
		}
		state[index] = 2
		out = append(out, line)
		return nil
	}
	for index := range base {
		if err := visit(index, nil); err != nil {
			return nil, err
		}
	}
	r.names = p
	return out, nil
}

func (p *placeholders) id(name string) (string, error) {
	held := p.names[name]
	return p.s.reference(p.r, p.group, p.field, held.group, held.key)
}

func (p *placeholders) object(name string) (object, error) {
	if held, ok := p.objects[name]; ok {
		return held, nil
	}
	id, err := p.id(name)
	if err != nil || id == "" {
		return nil, err
	}
	held := p.names[name]
	p.r.c.Organization = ""
	rows, err := p.s.byName[held.group].export(p.r.c, Query{Keys: []string{held.key}})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.key != held.key {
			continue
		}
		found := object{}
		for field, value := range row.data {
			found[field] = value
		}
		found["id"] = row.id
		p.objects[name] = found
		return found, nil
	}
	return nil, echo.NewHTTPError(http.StatusUnprocessableEntity, p.group.name+"."+p.field+" names "+held.group+" "+held.key+", which does not exist")
}

func (p *placeholders) fill(g group, data map[string]any) (map[string]any, error) {
	p.group = g
	out := make(map[string]any, len(data))
	for field, value := range data {
		p.field = field
		filled, err := p.value(value)
		if err != nil {
			return nil, err
		}
		out[field] = filled
	}
	return out, nil
}

func (p *placeholders) value(value any) (any, error) {
	switch held := value.(type) {
	case string:
		if !strings.Contains(held, placeholderOpen) {
			return held, nil
		}
		parsed, err := p.parse(held)
		if err != nil {
			return nil, err
		}
		if !parsed.single && parsed.text == nil {
			return held, nil
		}
		for _, used := range parsed.uses {
			if p.hosted(used.name) {
				continue
			}
			id, err := p.id(used.name)
			if err != nil {
				return nil, err
			}
			if id == "" {
				return "", nil
			}
		}
		if !parsed.single {
			var text bytes.Buffer
			if err := parsed.text.Execute(&text, nil); err != nil {
				return nil, echo.NewHTTPError(http.StatusUnprocessableEntity, p.group.name+"."+p.field+": "+held+" names a field the record does not carry")
			}
			return text.String(), nil
		}
		used := parsed.uses[0]
		var at any
		switch {
		case p.hosted(used.name):
			at = p.host[used.name]
		case len(used.fields) == 0:
			return p.id(used.name)
		default:
			found, err := p.object(used.name)
			if err != nil {
				return nil, err
			}
			at = map[string]any(found)
		}
		for _, field := range used.fields {
			node, ok := at.(map[string]any)
			if !ok {
				return nil, echo.NewHTTPError(http.StatusUnprocessableEntity, p.group.name+"."+p.field+": "+held+" names a field the record does not carry")
			}
			if at, ok = node[field]; !ok {
				return nil, echo.NewHTTPError(http.StatusUnprocessableEntity, p.group.name+"."+p.field+": "+held+" names a field the record does not carry")
			}
		}
		return at, nil
	case map[string]any:
		out := make(map[string]any, len(held))
		for field, item := range held {
			filled, err := p.value(item)
			if err != nil {
				return nil, err
			}
			out[field] = filled
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(held))
		for _, item := range held {
			filled, err := p.value(item)
			if err != nil {
				return nil, err
			}
			out = append(out, filled)
		}
		return out, nil
	}
	return value, nil
}
