package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

type Plugin struct {
	Name        string
	Owner       string
	Description string
	Skill       string
	Tree        fs.FS
	Version     func() string
	Hosts       []Host
}

type Host string

const (
	Claude   Host = "claude"
	Codex    Host = "codex"
	Gemini   Host = "gemini"
	OpenCode Host = "opencode"
)

type pluginManifest struct {
	Name        string                     `json:"name"`
	Version     string                     `json:"version"`
	Description string                     `json:"description"`
	Author      map[string]string          `json:"author"`
	Skills      string                     `json:"skills"`
	MCPServers  map[string]pluginMCPServer `json:"mcpServers"`
}

type pluginMCPServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func (p Plugin) id() string { return p.Name + "@" + p.Name }

func (p Plugin) Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "."+p.Name, "plugin"), nil
}

func (p Plugin) Manifest(executable string) pluginManifest {
	manifest := pluginManifest{
		Name:        p.Name,
		Description: p.Description,
		Author:      map[string]string{"name": p.Owner},
		Skills:      "./skills/",
		MCPServers:  map[string]pluginMCPServer{p.Name: {Command: executable, Args: []string{"mcp"}}},
	}
	sum := sha256.New()
	_ = json.NewEncoder(sum).Encode(manifest)
	sum.Write([]byte(p.Skill))
	if p.Tree != nil {
		_ = fs.WalkDir(p.Tree, ".", func(name string, item fs.DirEntry, err error) error {
			if err == nil && !item.IsDir() {
				body, _ := fs.ReadFile(p.Tree, name)
				sum.Write([]byte(name))
				sum.Write(body)
			}
			return nil
		})
	}
	manifest.Version = p.Version() + "-" + hex.EncodeToString(sum.Sum(nil))[:12]
	return manifest
}

func (p Plugin) Write(dir, executable string) (string, error) {
	manifest := p.Manifest(executable)
	marketplace := map[string]any{
		"name":        p.Name,
		"owner":       map[string]string{"name": p.Owner},
		"description": "The " + p.Name + " plugin this " + p.Name + " binary carries.",
		"plugins":     []map[string]string{{"name": p.Name, "source": "./", "description": p.Description, "version": manifest.Version}},
	}
	for name, value := range map[string]any{
		filepath.Join(".claude-plugin", "plugin.json"):      manifest,
		filepath.Join(".claude-plugin", "marketplace.json"): marketplace,
	} {
		body, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return "", err
		}
		if err := writeIn(dir, name, append(body, '\n')); err != nil {
			return "", err
		}
	}
	return manifest.Version, p.skills(dir)
}

func (p Plugin) skills(dir string) error {
	if p.Tree == nil {
		return writeIn(dir, filepath.Join("skills", p.Name, "SKILL.md"), []byte(p.Skill))
	}
	return fs.WalkDir(p.Tree, ".", func(name string, item fs.DirEntry, err error) error {
		if err != nil || item.IsDir() {
			return err
		}
		body, err := fs.ReadFile(p.Tree, name)
		if err != nil {
			return err
		}
		return writeIn(dir, filepath.Join("skills", filepath.FromSlash(name)), body)
	})
}

func writeIn(dir, name string, body []byte) error {
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

func Executable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return executable, nil
}

func PluginCommand(p Plugin) *cobra.Command {
	var project bool
	picked := map[Host]*bool{}
	plugin := &cobra.Command{
		Use:   "plugin",
		Short: "The claude plugin that carries the skill and the " + p.Name + " mcp server",
	}
	install := &cobra.Command{
		Use:   "install",
		Short: "Write the plugin under ~/." + p.Name + "/plugin and install it into Claude Code for the user, or the project with --project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(p.Hosts) == 0 {
				return p.install(cmd, project)
			}
			hosts, err := p.pick(cmd, picked)
			if err != nil {
				return err
			}
			for _, host := range hosts {
				if err := p.installOn(cmd, host, project); err != nil {
					return err
				}
			}
			return nil
		},
	}
	install.Flags().BoolVar(&project, "project", false, "install into the project settings of the current directory instead of the user settings")
	for _, host := range p.Hosts {
		picked[host] = install.Flags().Bool(string(host), false, "install for "+string(host))
	}
	if len(p.Hosts) > 0 {
		plugin.Short = "The plugin that carries the skill and the " + p.Name + " mcp server to each host"
		install.Short = "Install the skill and the " + p.Name + " mcp server for the hosts picked by flag, or for the one found on the path"
	}
	desktop := &cobra.Command{
		Use:   "desktop",
		Short: "Add the " + p.Name + " mcp server to the Claude Desktop config, keeping every other entry",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := p.InstallDesktop()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "wrote mcpServers."+p.Name+" in "+path+", restart Claude Desktop to load it")
			return nil
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Say whether the installed plugin matches this binary",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			want, have, err := p.Drift()
			if err != nil {
				return err
			}
			if len(p.Hosts) > 0 {
				installed := p.installed()
				if have != "" {
					installed[Claude] = have
				}
				if len(installed) == 0 {
					return Refused("the plugin is installed for no host, this binary carries "+want, "Run '"+p.Name+" plugin install'.")
				}
				for _, host := range p.Hosts {
					version, found := installed[host]
					if !found {
						continue
					}
					if version != want {
						return Refused("the plugin installed for "+string(host)+" is "+version+", this binary carries "+want, "Run '"+p.Name+" plugin install --"+string(host)+"'.")
					}
					fmt.Fprintln(cmd.OutOrStdout(), "installed "+version+" for "+string(host))
				}
				return nil
			}
			if want != have {
				return Refused("the installed plugin is "+orNone(have)+", this binary carries "+want, "Run '"+p.Name+" plugin install'.")
			}
			fmt.Fprintln(cmd.OutOrStdout(), "installed "+have)
			return nil
		},
	}
	plugin.AddCommand(install, desktop, status)
	return plugin
}

func orNone(version string) string {
	if version == "" {
		return "missing"
	}
	return version
}

func (p Plugin) Drift() (string, string, error) {
	executable, err := Executable()
	if err != nil {
		return "", "", err
	}
	want := p.Manifest(executable).Version
	dir, err := p.Dir()
	if err != nil {
		return "", "", err
	}
	var installed pluginManifest
	if body, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json")); err == nil {
		_ = json.Unmarshal(body, &installed)
	}
	return want, installed.Version, nil
}

func (p Plugin) install(cmd *cobra.Command, project bool) error {
	if _, err := exec.LookPath("claude"); err != nil {
		return Refused("the claude cli is not on the path", "Install Claude Code, then run '"+p.Name+" plugin install' again.")
	}
	dir, err := p.Dir()
	if err != nil {
		return err
	}
	executable, err := Executable()
	if err != nil {
		return err
	}
	version, err := p.Write(dir, executable)
	if err != nil {
		return err
	}
	stderr := cmd.ErrOrStderr()
	fmt.Fprintln(stderr, "wrote "+dir+" version "+version)

	scope := "user"
	if project {
		scope = "project"
	}
	var marketplaces []struct {
		Name string `json:"name"`
	}
	if listed, err := exec.CommandContext(cmd.Context(), "claude", "plugin", "marketplace", "list", "--json").Output(); err == nil {
		_ = json.Unmarshal(listed, &marketplaces)
	}
	step := []string{"plugin", "marketplace", "add", dir, "--scope", scope}
	for _, marketplace := range marketplaces {
		if marketplace.Name == p.Name {
			step = []string{"plugin", "marketplace", "update", p.Name}
		}
	}
	if err := p.run(cmd, "claude", step...); err != nil {
		return err
	}
	var plugins []struct {
		ID    string `json:"id"`
		Scope string `json:"scope"`
	}
	if installed, err := exec.CommandContext(cmd.Context(), "claude", "plugin", "list", "--json").Output(); err == nil {
		_ = json.Unmarshal(installed, &plugins)
	}
	step = []string{"plugin", "install", p.id(), "--scope", scope}
	for _, plugin := range plugins {
		if plugin.ID == p.id() && plugin.Scope == scope {
			step = []string{"plugin", "update", p.id(), "--scope", scope}
		}
	}
	if err := p.run(cmd, "claude", step...); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "installed "+p.id()+" "+version+", restart claude to load it")
	return nil
}

func (p Plugin) run(cmd *cobra.Command, name string, args ...string) error {
	run := exec.CommandContext(cmd.Context(), name, args...)
	run.Stdout = cmd.ErrOrStderr()
	run.Stderr = cmd.ErrOrStderr()
	if err := run.Run(); err != nil {
		return errors.New(name + " " + strings.Join(args, " ") + " failed: " + err.Error())
	}
	return nil
}

func (p Plugin) pick(cmd *cobra.Command, picked map[Host]*bool) ([]Host, error) {
	var asked, found []Host
	var flags []string
	for _, host := range p.Hosts {
		flags = append(flags, "--"+string(host))
		if *picked[host] {
			asked = append(asked, host)
		}
		if _, err := exec.LookPath(string(host)); err == nil {
			found = append(found, host)
		}
	}
	if len(asked) > 0 {
		return asked, nil
	}
	hint := "Pass " + strings.Join(flags, ", ") + " to pick."
	if len(found) == 0 {
		return nil, Refused("none of the hosts this plugin installs for is on the path", hint)
	}
	if len(found) == 1 {
		return found, nil
	}
	var names []string
	for _, host := range found {
		names = append(names, string(host))
	}
	stderr := cmd.ErrOrStderr()
	if in, ok := cmd.InOrStdin().(*os.File); !ok || !Terminal(in) || !Terminal(stderr) {
		return nil, Rejected(strings.Join(names, ", ")+" are installed", hint)
	}
	for at, name := range names {
		fmt.Fprintf(stderr, "%d  %s\n", at+1, name)
	}
	fmt.Fprint(stderr, "install for which, numbers separated by spaces, or all: ")
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	var hosts []Host
	for _, word := range strings.Fields(strings.ReplaceAll(line, ",", " ")) {
		if word == "all" {
			return found, nil
		}
		at, err := strconv.Atoi(word)
		if err != nil || at < 1 || at > len(found) {
			return nil, Rejected("no host is "+word, hint)
		}
		hosts = append(hosts, found[at-1])
	}
	if len(hosts) == 0 {
		return nil, Rejected("no host was picked", hint)
	}
	return hosts, nil
}

func (p Plugin) installOn(cmd *cobra.Command, host Host, project bool) error {
	if host == Claude {
		return p.install(cmd, project)
	}
	if project {
		return Rejected("--project installs for claude only", "Run '"+p.Name+" plugin install --"+string(host)+"' without it.")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	executable, err := Executable()
	if err != nil {
		return err
	}
	version := p.Manifest(executable).Version
	server := pluginMCPServer{Command: executable, Args: []string{"mcp"}}
	missing := Refused("the "+string(host)+" cli is not on the path", "Install it, then run '"+p.Name+" plugin install --"+string(host)+"' again.")
	stderr := cmd.ErrOrStderr()
	switch host {
	case Codex:
		if _, err := exec.LookPath("codex"); err != nil {
			return missing
		}
		dir := filepath.Join(home, ".agents")
		if err := p.skills(dir); err != nil {
			return err
		}
		fmt.Fprintln(stderr, "wrote the skills under "+filepath.Join(dir, "skills"))
		_ = exec.CommandContext(cmd.Context(), "codex", "mcp", "remove", p.Name).Run()
		if err := p.run(cmd, "codex", "mcp", "add", p.Name, "--", executable, "mcp"); err != nil {
			return err
		}
	case Gemini:
		if _, err := exec.LookPath("gemini"); err != nil {
			return missing
		}
		dir := filepath.Join(home, "."+p.Name, "gemini")
		body, err := json.MarshalIndent(map[string]any{
			"name":        p.Name,
			"version":     strings.TrimPrefix(version, "v"),
			"description": p.Description,
			"mcpServers":  map[string]pluginMCPServer{p.Name: server},
		}, "", "  ")
		if err != nil {
			return err
		}
		if err := writeIn(dir, "gemini-extension.json", append(body, '\n')); err != nil {
			return err
		}
		if err := p.skills(dir); err != nil {
			return err
		}
		fmt.Fprintln(stderr, "wrote "+dir)
		_ = exec.CommandContext(cmd.Context(), "gemini", "extensions", "uninstall", p.Name).Run()
		if err := p.run(cmd, "gemini", "extensions", "install", dir, "--consent"); err != nil {
			return err
		}
	case OpenCode:
		dir := filepath.Join(home, ".config", "opencode")
		if err := p.skills(dir); err != nil {
			return err
		}
		fmt.Fprintln(stderr, "wrote the skills under "+filepath.Join(dir, "skills"))
		path := filepath.Join(dir, "opencode.json")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if _, err := os.Stat(path + "c"); err == nil {
				path += "c"
			}
		}
		entry := map[string]any{"type": "local", "command": []string{executable, "mcp"}}
		if err := putServer(path, []string{"mcp", "servers", p.Name}, entry); err != nil {
			return err
		}
		fmt.Fprintln(stderr, "wrote mcp.servers."+p.Name+" in "+path)
	default:
		return Rejected("no host is "+string(host), "The hosts are claude, codex, gemini and opencode.")
	}
	installed := p.installed()
	installed[host] = version
	body, err := json.MarshalIndent(installed, "", "  ")
	if err != nil {
		return err
	}
	if err := writeIn(filepath.Join(home, "."+p.Name), "hosts.json", append(body, '\n')); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "installed "+p.Name+" "+version+" for "+string(host)+", restart "+string(host)+" to load it")
	return nil
}

func (p Plugin) installed() map[Host]string {
	installed := map[Host]string{}
	if home, err := os.UserHomeDir(); err == nil {
		if body, err := os.ReadFile(filepath.Join(home, "."+p.Name, "hosts.json")); err == nil {
			_ = json.Unmarshal(body, &installed)
		}
	}
	return installed
}

func putServer(path string, keys []string, entry any) error {
	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return Refused(path+" is not plain json and is left as it is", "Add "+strings.Join(keys, ".")+" there by hand.")
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	at := root
	for _, key := range keys[:len(keys)-1] {
		next, ok := at[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			at[key] = next
		}
		at = next
	}
	at[keys[len(keys)-1]] = entry
	body, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return writeIn(filepath.Dir(path), filepath.Base(path), append(body, '\n'))
}

func DesktopConfigPath() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", errors.New("APPDATA is not set")
		}
		return filepath.Join(appData, "Claude", "claude_desktop_config.json"), nil
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"), nil
	}
}

func (p Plugin) InstallDesktop() (string, error) {
	path, err := DesktopConfigPath()
	if err != nil {
		return "", err
	}
	executable, err := Executable()
	if err != nil {
		return "", err
	}
	content := map[string]json.RawMessage{}
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &content); err != nil {
			return "", errors.New(path + " is not a json object: " + err.Error())
		}
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	servers := map[string]json.RawMessage{}
	if raw, found := content["mcpServers"]; found && len(raw) > 0 {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return "", errors.New("mcpServers in " + path + " is not a json object: " + err.Error())
		}
	}
	entry, _ := json.Marshal(pluginMCPServer{Command: executable, Args: []string{"mcp"}})
	servers[p.Name] = entry
	content["mcpServers"], _ = json.Marshal(servers)
	body, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, append(body, '\n'), 0o644)
}
