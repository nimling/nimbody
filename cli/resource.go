package cli

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/nimling/nimbody/mcp"
	"github.com/spf13/cobra"
)

type Verb struct {
	Name    string
	Short   string
	Method  string
	Paths   []string
	Body    bool
	Upload  bool
	Stream  bool
	Access  string
	Example string
}

type Resource struct {
	Name     string
	Short    string
	Verbs    []Verb
	Children []Resource
}

type Table struct {
	Name      string
	Send      Send
	Output    *Output
	Resources []Resource
	Fill      map[string]func() (string, error)
}

func (t Table) Mount(root *cobra.Command) []*cobra.Command {
	var mounted []*cobra.Command
	for _, resource := range t.Resources {
		cmd := t.Command(resource, nil)
		root.AddCommand(cmd)
		mounted = append(mounted, cmd)
	}
	counts := map[string]int{}
	var count func(resources []Resource)
	count = func(resources []Resource) {
		for _, resource := range resources {
			counts[resource.Name]++
			count(resource.Children)
		}
	}
	count(t.Resources)
	taken := map[string]bool{}
	for _, cmd := range root.Commands() {
		taken[cmd.Name()] = true
	}
	var shortcut func(parents []string, resources []Resource)
	shortcut = func(parents []string, resources []Resource) {
		for _, resource := range resources {
			path := append(append([]string{}, parents...), resource.Name)
			if len(parents) > 0 && counts[resource.Name] == 1 && !taken[resource.Name] {
				cmd := t.Command(resource, path)
				cmd.Hidden = true
				root.AddCommand(cmd)
				taken[resource.Name] = true
			}
			shortcut(path, resource.Children)
		}
	}
	shortcut(nil, t.Resources)
	return mounted
}

func (t Table) Command(resource Resource, path []string) *cobra.Command {
	if path == nil {
		path = []string{resource.Name}
	}
	cmd := &cobra.Command{Use: resource.Name, Short: resource.Short}
	for _, verb := range resource.Verbs {
		cmd.AddCommand(t.verb(verb, path))
	}
	for _, child := range resource.Children {
		cmd.AddCommand(t.Command(child, append(append([]string{}, path...), child.Name)))
	}
	return cmd
}

func (t Table) output() *Output {
	if t.Output == nil {
		return &Output{}
	}
	return t.Output
}

func (t Table) verb(verb Verb, path []string) *cobra.Command {
	body := &Body{}
	least, most := verb.arities()
	access := verb.Access
	if access == "" {
		access = mcp.AccessWrite
		switch verb.Method {
		case http.MethodGet, http.MethodHead:
			access = mcp.AccessRead
		case http.MethodDelete:
			access = mcp.AccessDestructive
		}
	}
	cmd := &cobra.Command{
		Use:         verb.use(t.Fill),
		Short:       verb.Short,
		Example:     verb.Example,
		Args:        cobra.RangeArgs(least, most),
		Annotations: map[string]string{AnnotationMCP: access},
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := verb.path(args, t.Fill)
			if err != nil {
				return err
			}
			var payload []byte
			var contentType string
			if verb.Body || verb.Upload {
				payload, contentType, err = body.Get(cmd.InOrStdin())
				if err != nil {
					return err
				}
			}
			answer, err := t.Send(cmd, verb.Method, target, t.output().GetQuery(), contentType, payload)
			if err != nil {
				return err
			}
			return t.output().Print(cmd.OutOrStdout(), cmd.ErrOrStderr(), answer)
		},
	}
	if verb.Stream {
		cmd.Flags().Bool(FlagStream, false, StreamUsage)
	}
	if verb.Body || verb.Upload {
		body.Flags(cmd, verb.Upload)
	}
	if cmd.Example == "" {
		cmd.Example = verb.example(t.Name, path, t.Fill)
	}
	return cmd
}

func (v Verb) arities() (int, int) {
	least := -1
	for index, path := range v.Paths {
		if path != "" && least < 0 {
			least = index
		}
	}
	return max(least, 0), max(len(v.Paths)-1, 0)
}

func (v Verb) placeholders(path string, fill map[string]func() (string, error)) []string {
	var names []string
	rest := path
	for {
		open := strings.Index(rest, "{")
		if open < 0 {
			return names
		}
		closing := strings.Index(rest[open:], "}")
		if closing < 0 {
			return names
		}
		name := rest[open+1 : open+closing]
		if _, filled := fill[name]; !filled {
			names = append(names, name)
		}
		rest = rest[open+closing+1:]
	}
}

func (v Verb) use(fill map[string]func() (string, error)) string {
	least, _ := v.arities()
	parts := []string{v.Name}
	if len(v.Paths) > 0 {
		for index, name := range v.placeholders(v.Paths[len(v.Paths)-1], fill) {
			if index < least {
				parts = append(parts, "<"+name+">")
				continue
			}
			parts = append(parts, "["+name+"]")
		}
	}
	return strings.Join(parts, " ")
}

func (v Verb) path(args []string, fill map[string]func() (string, error)) (string, error) {
	if len(args) >= len(v.Paths) || v.Paths[len(args)] == "" {
		var arities []string
		for index, path := range v.Paths {
			if path != "" {
				arities = append(arities, strconv.Itoa(index))
			}
		}
		return "", Rejected(v.Name+" takes "+strings.Join(arities, " or ")+" arguments", "")
	}
	path := v.Paths[len(args)]
	for name, value := range fill {
		if !strings.Contains(path, "{"+name+"}") {
			continue
		}
		filled, err := value()
		if err != nil {
			return "", err
		}
		path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(filled))
	}
	for index, name := range v.placeholders(path, fill) {
		path = strings.Replace(path, "{"+name+"}", url.PathEscape(args[index]), 1)
	}
	return path, nil
}

func (v Verb) example(name string, path []string, fill map[string]func() (string, error)) string {
	command := strings.TrimSpace(name + " " + strings.Join(path, " ") + " " + v.Name)
	var lines []string
	for _, target := range v.Paths {
		if target == "" {
			continue
		}
		line := command
		for _, placeholder := range v.placeholders(target, fill) {
			line += " <" + placeholder + ">"
		}
		switch {
		case v.Upload:
			line += " --upload ./file.png"
		case v.Body && v.Method == http.MethodPatch:
			line += " --set name=Orion"
		case v.Body:
			line += " --set name=Orion --set tags='[\"news\"]'"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
