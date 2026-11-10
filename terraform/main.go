package terraform

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6/tf6server"
	"github.com/nimling/nimbody/cli"
	"github.com/nimling/nimbody/state"
	"github.com/spf13/cobra"
)

const DefaultHost = "registry.terraform.io"

const DefaultNamespace = "samna"

type Provider struct {
	Name     string
	Source   string
	Version  func() string
	Client   *cli.Client
	Document state.Document
}

var semver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`)

func Register(root *cobra.Command, p Provider) {
	if _, _, name := p.address(); name != p.Name {
		panic("terraform: provider " + p.Name + " has source " + p.Source + ", whose type is not " + p.Name)
	}
	root.AddCommand(Command(p))
	cli.Provider = p.Serve
}

func (p Provider) address() (string, string, string) {
	source := p.Source
	if source == "" {
		source = DefaultNamespace + "/" + p.Name
	}
	parts := strings.Split(source, "/")
	if len(parts) == 2 {
		parts = append([]string{DefaultHost}, parts...)
	}
	if len(parts) != 3 {
		return "", "", ""
	}
	return parts[0], parts[1], parts[2]
}

func (p Provider) source() string {
	host, namespace, name := p.address()
	return host + "/" + namespace + "/" + name
}

func (p Provider) version() (string, error) {
	version := strings.TrimPrefix(p.Version(), "v")
	if !semver.MatchString(version) {
		return "", cli.Rejected("version "+p.Version()+" is not a release version, and terraform takes only those", "Install a released "+p.Name+".")
	}
	return version, nil
}

func (p Provider) Serve() error {
	return tf6server.Serve(p.source(), p.Server)
}

func (p Provider) Server() tfprotov6.ProviderServer {
	return &server{provider: p}
}

func PluginDir() (string, error) {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("APPDATA"), "terraform.d", "plugins"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".terraform.d", "plugins"), nil
}

func (p Provider) Install(dir string) (string, error) {
	version, err := p.version()
	if err != nil {
		return "", err
	}
	if dir == "" {
		if dir, err = PluginDir(); err != nil {
			return "", err
		}
	}
	executable, err := cli.Executable()
	if err != nil {
		return "", err
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		return "", err
	}
	file := GetFiles(p.Name, version, runtime.GOOS, runtime.GOARCH).Executable
	host, namespace, name := p.address()
	path := filepath.Join(dir, host, namespace, name, version, runtime.GOOS+"_"+runtime.GOARCH, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, body, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

func Command(p Provider) *cobra.Command {
	root := &cobra.Command{
		Use:   "terraform",
		Short: "The terraform provider this binary serves, one resource per state group",
	}
	var dir string
	install := &cobra.Command{
		Use:   "install",
		Short: "Place this binary as the " + p.Name + " provider in the local terraform plugin directory, or a filesystem mirror with --dir",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := p.Install(dir)
			if err != nil {
				return err
			}
			version, _ := p.version()
			fmt.Fprintln(cmd.OutOrStdout(), path)
			fmt.Fprintf(cmd.ErrOrStderr(), "required_providers { %s = { source = %q, version = %q } }\n", p.Name, p.source(), version)
			return nil
		},
	}
	install.Flags().StringVar(&dir, "dir", "", "the plugin directory or filesystem mirror to place the provider in, ~/.terraform.d/plugins by default")
	root.AddCommand(install)

	var out, modules string
	pack := &cobra.Command{
		Use:     "package <os>_<arch>=<binary>...",
		Short:   "Write the release files of the " + p.Name + " provider: one zip per platform, the manifest, their SHA256SUMS to sign, and a tar.gz per module, in the layout of the public registry",
		Example: "  " + p.Name + " terraform package --dir dist --modules deploy/terraform linux_amd64=build/" + p.Name + "_linux_amd64 darwin_arm64=build/" + p.Name + "_darwin_arm64",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			version, err := p.version()
			if err != nil {
				return err
			}
			release := Package{Type: p.Name, Version: version, Modules: modules}
			for _, arg := range args {
				platform, path, ok := strings.Cut(arg, "=")
				goos, goarch, split := strings.Cut(platform, "_")
				if !ok || !split || goos == "" || goarch == "" || path == "" {
					return cli.Rejected(arg+" is not <os>_<arch>=<binary>", "Name each build as linux_amd64=build/"+p.Name+"_linux_amd64.")
				}
				release.Binaries = append(release.Binaries, Binary{OS: goos, Arch: goarch, Path: path})
			}
			written, err := PutPackage(out, release)
			if err != nil {
				return err
			}
			for _, path := range written {
				fmt.Fprintln(cmd.OutOrStdout(), path)
			}
			files := GetFiles(p.Name, version, "", "")
			fmt.Fprintf(cmd.ErrOrStderr(), "sign %s into %s with the org key and attach every file to the GitHub release\n", files.Sums, files.Signature)
			return nil
		},
	}
	pack.Flags().StringVar(&out, "dir", "dist", "the directory to write the release files to")
	pack.Flags().StringVar(&modules, "modules", "", "the modules directory, deploy/terraform, holding <name>/<system>/ per module")
	root.AddCommand(pack)

	var file string
	schema := &cobra.Command{
		Use:     "schema",
		Short:   "Write the " + p.Name + " provider schema as terraform providers schema -json does, from the state definitions in this binary, with no endpoint and no login",
		Example: "  " + p.Name + " terraform schema -o schema.json && tfplugindocs generate --provider-name " + p.Name + " --providers-schema schema.json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := p.GetSchemaJSON(cmd.Context())
			if err != nil {
				return err
			}
			body = append(body, '\n')
			if file == "" || file == "-" {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			return os.WriteFile(file, body, 0o644)
		},
	}
	schema.Flags().StringVarP(&file, "output", "o", "-", "file to write the schema to, - for stdout")
	root.AddCommand(schema)
	return root
}
