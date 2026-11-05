package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joho/godotenv"
	"github.com/nimling/nimbody/mcp"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

const (
	ConfigFile    = "config.yaml"
	KeyProtected  = "protected"
	keyUse        = "use"
	keyActive     = "active"
	LayerFlag     = "flag"
	LayerEnv      = "environment"
	LayerEnvFile  = "env file"
	LayerLocal    = "local"
	LayerConfig   = "config"
	defaultConfig = "default"
)

type Setting struct {
	Key    string
	Env    string
	Flag   string
	Secret bool
}

type Source struct {
	Key   string `json:"key"`
	Env   string `json:"env"`
	Value string `json:"value"`
	Layer string `json:"layer"`
}

type Loaded struct {
	Config    string   `json:"config"`
	Protected bool     `json:"protected"`
	Home      string   `json:"home"`
	Local     string   `json:"local,omitempty"`
	EnvFiles  []string `json:"env_files,omitempty"`
	Values    []Source `json:"values"`
}

type Settings struct {
	Name     string
	Prefix   string
	Keys     []Setting
	Loaded   Loaded
	values   map[string]string
	config   string
	envFiles []string
	global   bool
	yes      bool
	plan     bool
	set      *cobra.Command
}

type homeFile struct {
	Default string                       `yaml:"default"`
	Configs map[string]map[string]string `yaml:"configs"`
}

func (s *Settings) Flags(root *cobra.Command) {
	var secrets []string
	for _, key := range s.Keys {
		if key.Secret && key.Flag != "" {
			secrets = append(secrets, key.Flag)
		}
	}
	if len(secrets) > 0 {
		if root.Annotations == nil {
			root.Annotations = map[string]string{}
		}
		root.Annotations[AnnotationSecret] = strings.Join(secrets, ",")
	}
	flags := root.PersistentFlags()
	flags.StringVar(&s.config, "config", "", "named config from ~/."+s.Name+"/"+ConfigFile+", defaults to "+s.Prefix+"CONFIG, the local ."+s.Name+" use, then the default")
	flags.StringArrayVar(&s.envFiles, "env", nil, "dotenv file loaded as a layer above the configs, repeatable")
	flags.BoolVar(&s.global, "global", false, "ignore the local ."+s.Name+" directory for this command")
	flags.BoolVar(&s.yes, "yes", false, "confirm a write on a protected config without a prompt")
	flags.BoolVar(&s.plan, "plan", false, "print what a write would do and stop")
}

func (s *Settings) Home() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "."+s.Name), nil
}

func (s *Settings) HomePath() (string, error) {
	dir, err := s.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ConfigFile), nil
}

func (s *Settings) LocalPath() string {
	homes := map[string]bool{}
	bases := []string{}
	if home, err := os.UserHomeDir(); err == nil {
		bases = append(bases, home)
	}
	if account, err := user.Current(); err == nil {
		bases = append(bases, account.HomeDir)
	}
	for _, base := range bases {
		home := filepath.Join(base, "."+s.Name)
		if resolved, err := filepath.EvalSymlinks(home); err == nil {
			home = resolved
		}
		homes[home] = true
	}
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for {
		candidate := filepath.Join(dir, "."+s.Name)
		if !homes[candidate] {
			path := filepath.Join(candidate, ConfigFile)
			if _, err := os.Stat(path); err == nil {
				return path
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func readHome(path string) (homeFile, error) {
	file := homeFile{Configs: map[string]map[string]string{}}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return file, nil
	}
	if err != nil {
		return file, err
	}
	if err := yaml.Unmarshal(body, &file); err != nil {
		return file, Rejected("cannot read "+path+": "+err.Error(), "")
	}
	if file.Configs == nil {
		file.Configs = map[string]map[string]string{}
	}
	return file, nil
}

func writeYAML(path string, value any) error {
	body, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

func readLocal(path string) (map[string]string, error) {
	values := map[string]string{}
	if path == "" {
		return values, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return values, err
	}
	if err := yaml.Unmarshal(body, &values); err != nil {
		return values, Rejected("cannot read "+path+": "+err.Error(), "")
	}
	if values == nil {
		values = map[string]string{}
	}
	return values, nil
}

func (s *Settings) setting(name string) (Setting, bool) {
	for _, key := range s.Keys {
		if strings.EqualFold(name, key.Key) || strings.EqualFold(name, key.Env) {
			return key, true
		}
	}
	return Setting{}, false
}

func (s *Settings) localActive() (string, map[string]string, error) {
	path := s.LocalPath()
	if s.global || path == "" {
		return "", map[string]string{}, nil
	}
	values, err := readLocal(path)
	if err != nil {
		return "", nil, err
	}
	if values[keyActive] == "false" {
		return "", map[string]string{}, nil
	}
	return path, values, nil
}

func (s *Settings) Load(cmd *cobra.Command) error {
	homePath, err := s.HomePath()
	if err != nil {
		return err
	}
	home, err := readHome(homePath)
	if err != nil {
		return err
	}
	localPath, local, err := s.localActive()
	if err != nil {
		return err
	}
	for _, key := range s.Keys {
		if key.Secret && local[key.Key] != "" {
			return Unauthenticated(key.Key+" is in "+localPath+", and a secret never lives in a local ."+s.Name+" because that directory sits in a repository and gets committed", "Remove the line, then set it with '"+s.Name+" config set "+key.Key+"=...' in ~/."+s.Name+".")
		}
	}

	name := firstSet(s.config, os.Getenv(s.Prefix+"CONFIG"), local[keyUse], home.Default, defaultConfig)
	block, known := home.Configs[name]
	if !known && (cmd == nil || cmd != s.set) && (s.config != "" || os.Getenv(s.Prefix+"CONFIG") != "" || local[keyUse] != "") {
		return Rejected("no config named "+name+" in "+homePath, "Run '"+s.Name+" config list' to see the configs, or '"+s.Name+" config set --config "+name+" key=value' to add it.")
	}

	process := map[string]string{}
	for _, key := range s.Keys {
		if value, ok := os.LookupEnv(key.Env); ok && value != "" {
			process[key.Env] = value
		}
	}
	files := map[string]string{}
	for _, path := range s.envFiles {
		values, err := godotenv.Read(path)
		if err != nil {
			return Rejected("cannot read the env file "+path+": "+err.Error(), "")
		}
		for env, value := range values {
			files[env] = value
		}
	}
	for env, value := range files {
		if _, set := os.LookupEnv(env); set {
			delete(files, env)
			continue
		}
		if err := os.Setenv(env, value); err != nil {
			return err
		}
	}

	s.values = map[string]string{}
	s.Loaded = Loaded{Config: name, Protected: block[KeyProtected] == "true", Home: homePath, Local: localPath, EnvFiles: s.envFiles}
	for _, key := range s.Keys {
		source := Source{Key: key.Key, Env: key.Env}
		switch {
		case cmd != nil && flagChanged(cmd, key.Flag):
			source.Layer = LayerFlag
			source.Value = cmd.Flags().Lookup(key.Flag).Value.String()
		case process[key.Env] != "":
			source.Layer, source.Value = LayerEnv, process[key.Env]
		case files[key.Env] != "":
			source.Layer, source.Value = LayerEnvFile, files[key.Env]
		case local[key.Key] != "":
			source.Layer, source.Value = LayerLocal, local[key.Key]
		case block[key.Key] != "":
			source.Layer, source.Value = LayerConfig, block[key.Key]
		}
		if source.Value != "" && source.Layer != LayerFlag && process[key.Env] == "" {
			if err := os.Setenv(key.Env, source.Value); err != nil {
				return err
			}
		}
		s.values[key.Key] = source.Value
		if key.Secret {
			source.Value = Mask(source.Value)
		}
		s.Loaded.Values = append(s.Loaded.Values, source)
	}
	return nil
}

func (s *Settings) Get(key string) string {
	if value := s.values[key]; value != "" {
		return value
	}
	if setting, ok := s.setting(key); ok {
		return os.Getenv(setting.Env)
	}
	return os.Getenv(s.Prefix + strings.ToUpper(key))
}

func (s *Settings) Put(key, value string) {
	if s.values == nil {
		s.values = map[string]string{}
	}
	s.values[key] = value
}

func flagChanged(cmd *cobra.Command, name string) bool {
	if name == "" {
		return false
	}
	flag := cmd.Flags().Lookup(name)
	return flag != nil && flag.Changed
}

func firstSet(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func Mask(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 4 {
		return strings.Repeat("x", len(value))
	}
	return strings.Repeat("x", len(value)-4) + value[len(value)-4:]
}

func topLevel(cmd *cobra.Command) string {
	for current := cmd; current != nil; current = current.Parent() {
		if current.Parent() != nil && !current.Parent().HasParent() {
			return current.Name()
		}
	}
	return ""
}

func (s *Settings) Guard(cmd *cobra.Command, args []string) error {
	if Tooling[topLevel(cmd)] || !cmd.Runnable() {
		return nil
	}
	access := Access(cmd)
	if access == mcp.AccessRead || access == AccessNone {
		return nil
	}
	plan := s.planText(cmd, args)
	if s.plan {
		fmt.Fprintln(cmd.ErrOrStderr(), plan)
		return &Error{Code: ExitOK, Message: "planned"}
	}
	if !s.Loaded.Protected {
		return nil
	}
	fmt.Fprintln(cmd.ErrOrStderr(), plan)
	if s.yes {
		return nil
	}
	hint := "Rerun with --yes to confirm, or --plan to only print it."
	if os.Getenv("NIMBODY_MCP") != "" || !Terminal(os.Stdin) {
		return Refused(s.Loaded.Config+" is protected and this command writes", hint)
	}
	want := "yes"
	if access == mcp.AccessDestructive {
		want = s.Loaded.Config
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "type %s to continue: ", want)
	answer, err := readLine(os.Stdin)
	if err != nil || strings.TrimSpace(answer) != want {
		return Refused("not confirmed", hint)
	}
	return nil
}

func readLine(in io.Reader) (string, error) {
	var line []byte
	buffer := make([]byte, 1)
	for {
		n, err := in.Read(buffer)
		if n > 0 {
			if buffer[0] == '\n' {
				return string(line), nil
			}
			line = append(line, buffer[0])
		}
		if err != nil {
			return string(line), err
		}
	}
}

func (s *Settings) planText(cmd *cobra.Command, args []string) string {
	parts := []string{"plan:", cmd.CommandPath()}
	parts = append(parts, args...)
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		if flag.Name == "yes" || flag.Name == "plan" {
			return
		}
		value := flag.Value.String()
		if setting, ok := s.setting(flag.Name); ok && setting.Secret {
			value = Mask(value)
		}
		parts = append(parts, "--"+flag.Name+"="+value)
	})
	target := []string{"config " + s.Loaded.Config}
	for _, source := range s.Loaded.Values {
		if source.Value != "" {
			target = append(target, source.Key+" "+source.Value)
		}
	}
	return strings.Join(parts, " ") + "\non " + strings.Join(target, ", ")
}

func (s *Settings) ConfigCommand() *cobra.Command {
	config := &cobra.Command{
		Use:   "config",
		Short: "Named configs in ~/." + s.Name + "/" + ConfigFile + ", a local ." + s.Name + " that takes over, and env files",
	}
	var local bool
	var target string

	get := &cobra.Command{
		Use:     "get",
		Short:   "Print the active config, every value and the layer it came from",
		Example: s.Name + " config get --config prd",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return PrintValue(cmd.OutOrStdout(), s.Loaded, FormatFor(cmd.OutOrStdout(), wantsJSON(cmd)))
		},
	}

	list := &cobra.Command{
		Use:     "list",
		Short:   "Print the named configs, which one is the default and which are protected",
		Example: s.Name + " config list",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := s.HomePath()
			if err != nil {
				return err
			}
			home, err := readHome(path)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(home.Configs))
			for name := range home.Configs {
				names = append(names, name)
			}
			sort.Strings(names)
			rows := make([]map[string]any, 0, len(names))
			for _, name := range names {
				rows = append(rows, map[string]any{"name": name, "default": name == home.Default, "active": name == s.Loaded.Config, "protected": home.Configs[name][KeyProtected] == "true"})
			}
			return PrintValue(cmd.OutOrStdout(), rows, FormatFor(cmd.OutOrStdout(), wantsJSON(cmd)))
		},
	}

	set := &cobra.Command{
		Use:     "set <key=value> [key=value]",
		Short:   "Write values into a named config, or into the local ." + s.Name + " with --local",
		Example: s.Name + " config set --to prd endpoint=https://example.com protected=true",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			values := map[string]string{}
			for _, entry := range args {
				name, value, found := strings.Cut(entry, "=")
				if !found || name == "" {
					return Rejected("entries look like key=value, got "+entry, "")
				}
				key := strings.ToLower(strings.TrimSpace(name))
				if setting, ok := s.setting(key); ok {
					key = setting.Key
					if local && setting.Secret {
						return Unauthenticated(key+" is a secret and never goes into the local ."+s.Name, "Set it without --local so it lands in ~/."+s.Name+".")
					}
				} else if key != KeyProtected {
					return Rejected("unknown key "+name, "Known keys: "+s.keyNames()+", "+KeyProtected+".")
				}
				values[key] = value
			}
			change := s.changeText(values)
			if local {
				path := s.LocalPath()
				if path == "" {
					return Rejected("no local ."+s.Name+" here", "Run '"+s.Name+" config init' first.")
				}
				current, err := readLocal(path)
				if err != nil {
					return err
				}
				for key, value := range values {
					current[key] = value
				}
				return s.write(cmd, path, current, "write "+change+" into "+path, "wrote "+path)
			}
			path, err := s.HomePath()
			if err != nil {
				return err
			}
			home, err := readHome(path)
			if err != nil {
				return err
			}
			name := firstSet(target, s.Loaded.Config, defaultConfig)
			if home.Configs[name] == nil {
				home.Configs[name] = map[string]string{}
			}
			for key, value := range values {
				home.Configs[name][key] = value
			}
			if home.Default == "" {
				home.Default = name
			}
			return s.write(cmd, path, home, "write "+change+" into "+name+" in "+path, "wrote "+name+" in "+path)
		},
	}
	s.set = set
	set.Flags().BoolVar(&local, "local", false, "write into the local ."+s.Name+" instead of a named config")
	set.Flags().StringVar(&target, "to", "", "the named config to write, defaults to the active one")

	use := &cobra.Command{
		Use:     "use <name>",
		Short:   "Make a named config the one in use, in the local ." + s.Name + " when there is one, else the default",
		Example: s.Name + " config use prd",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := s.HomePath()
			if err != nil {
				return err
			}
			home, err := readHome(path)
			if err != nil {
				return err
			}
			if home.Configs[args[0]] == nil {
				return Rejected("no config named "+args[0]+" in "+path, "Run '"+s.Name+" config list'.")
			}
			if localPath, values, err := s.localActive(); err != nil {
				return err
			} else if localPath != "" {
				values[keyUse] = args[0]
				return s.write(cmd, localPath, values, "use "+args[0]+" in "+localPath, "using "+args[0]+" in "+localPath)
			}
			home.Default = args[0]
			return s.write(cmd, path, home, "use "+args[0]+" by default in "+path, "using "+args[0]+" by default")
		},
	}

	initLocal := &cobra.Command{
		Use:     "init [name]",
		Short:   "Create a local ." + s.Name + " in this directory that takes over, using the named config",
		Example: s.Name + " config init tst",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			path := filepath.Join(dir, "."+s.Name, ConfigFile)
			if _, err := os.Stat(path); err == nil {
				return Rejected(path+" exists", "Edit it with '"+s.Name+" config set --local key=value'.")
			}
			values := map[string]string{keyUse: s.Loaded.Config}
			if len(args) == 1 {
				values[keyUse] = args[0]
			}
			return s.write(cmd, path, values, "create "+path+" using "+values[keyUse], "wrote "+path)
		},
	}

	toggle := &cobra.Command{
		Use:       "local <on|off>",
		Short:     "Turn the nearest local ." + s.Name + " on or off without removing it",
		Example:   s.Name + " config local off",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "on" && args[0] != "off" {
				return Rejected("local takes on or off, got "+args[0], "")
			}
			path := s.LocalPath()
			if path == "" {
				return Rejected("no local ."+s.Name+" here", "Run '"+s.Name+" config init' first.")
			}
			values, err := readLocal(path)
			if err != nil {
				return err
			}
			if args[0] == "off" {
				values[keyActive] = "false"
			} else {
				delete(values, keyActive)
			}
			return s.write(cmd, path, values, "turn local "+args[0]+" in "+path, "local "+args[0]+" in "+path)
		},
	}

	path := &cobra.Command{
		Use:     "path",
		Short:   "Print the home config file and the local one in effect",
		Example: s.Name + " config path",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), s.Loaded.Home)
			if s.Loaded.Local != "" {
				fmt.Fprintln(cmd.OutOrStdout(), s.Loaded.Local)
			}
			return nil
		},
	}

	config.AddCommand(get, list, set, use, initLocal, toggle, path)
	return config
}

func (s *Settings) keyNames() string {
	names := make([]string, 0, len(s.Keys))
	for _, key := range s.Keys {
		names = append(names, key.Key)
	}
	return strings.Join(names, ", ")
}

func (s *Settings) write(cmd *cobra.Command, path string, value any, plan, done string) error {
	if s.plan {
		fmt.Fprintln(cmd.ErrOrStderr(), "plan: "+plan)
		return &Error{Code: ExitOK, Message: "planned"}
	}
	if err := writeYAML(path, value); err != nil {
		return err
	}
	cmd.PrintErrln(done)
	return nil
}

func (s *Settings) changeText(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := values[key]
		if setting, ok := s.setting(key); ok && setting.Secret {
			value = Mask(value)
		}
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, ", ")
}
