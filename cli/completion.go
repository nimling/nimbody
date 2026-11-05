package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func GetDataDir(name string) (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, name), nil
}

func CompletionCommand(root *cobra.Command, skill string) *cobra.Command {
	var auto, withSkill bool
	name := root.Name()
	root.CompletionOptions.DisableDefaultCmd = true
	cmd := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Print the shell completion script, or install it with --auto",
		Long: "Print the completion script for one shell to stdout, or pass --auto to install it.\n\n" +
			"--auto detects the shell from $SHELL when none is given, writes the script under the XDG data dir,\n" +
			"$XDG_DATA_HOME/" + name + "/completions or ~/.local/share/" + name + "/completions, and points the shell rc file at it\n" +
			"inside a managed block. Rerunning --auto refreshes the script and leaves the rc as it is. fish loads\n" +
			"from its own completions directory and needs no rc change.",
		Example:   name + " completion zsh\n" + name + " completion --auto",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			shell := ""
			if len(args) == 1 {
				shell = args[0]
			}
			if !auto {
				if withSkill {
					return Rejected("--skill installs alongside --auto", "")
				}
				if shell == "" {
					return Rejected("name a shell, bash, zsh, fish or powershell, or pass --auto", "Run '"+name+" completion --auto'.")
				}
				return completionScript(root, shell, cmd.OutOrStdout())
			}
			if err := installCompletion(cmd, root, shell); err != nil {
				return err
			}
			if withSkill {
				path, err := InstallSkill(name, skill, false)
				if err != nil {
					return err
				}
				cmd.PrintErrln("wrote " + path)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&auto, "auto", false, "detect the shell and install the completion")
	if skill != "" {
		cmd.Flags().BoolVar(&withSkill, "skill", false, "install the claude skill under ~/.claude/skills in the same step")
	}
	return cmd
}

func completionScript(root *cobra.Command, shell string, out io.Writer) error {
	switch shell {
	case "bash":
		return root.GenBashCompletionV2(out, true)
	case "zsh":
		return root.GenZshCompletion(out)
	case "fish":
		return root.GenFishCompletion(out, true)
	case "powershell":
		return root.GenPowerShellCompletionWithDesc(out)
	}
	return Rejected("no completion for "+shell, "Use bash, zsh, fish or powershell.")
}

func installCompletion(cmd, root *cobra.Command, shell string) error {
	name := root.Name()
	if shell == "" {
		base := filepath.Base(os.Getenv("SHELL"))
		for _, known := range []string{"zsh", "bash", "fish"} {
			if strings.Contains(base, known) {
				shell = known
				break
			}
		}
	}
	if shell == "" {
		return Rejected("cannot tell the shell from $SHELL", "Name it: '"+name+" completion <shell> --auto'.")
	}
	var script bytes.Buffer
	if err := completionScript(root, shell, &script); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if shell == "fish" {
		dir := filepath.Join(home, ".config", "fish", "completions")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(dir, name+".fish")
		if err := os.WriteFile(path, script.Bytes(), 0o644); err != nil {
			return err
		}
		cmd.PrintErrln("wrote " + path)
		return nil
	}
	if shell != "bash" && shell != "zsh" {
		return Rejected("--auto installs for bash, zsh and fish", "Run '"+name+" completion "+shell+"' and load it yourself.")
	}
	data, err := GetDataDir(name)
	if err != nil {
		return err
	}
	dir := filepath.Join(data, "completions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	scriptPath := filepath.Join(dir, name+".bash")
	rc := filepath.Join(home, ".bashrc")
	block := fmt.Sprintf("[ -f %q ] && source %q", scriptPath, scriptPath)
	if shell == "zsh" {
		scriptPath = filepath.Join(dir, "_"+name)
		rc = filepath.Join(home, ".zshrc")
		block = fmt.Sprintf("fpath=(%q $fpath)\nautoload -Uz compinit && compinit -u", dir)
	}
	if err := os.WriteFile(scriptPath, script.Bytes(), 0o644); err != nil {
		return err
	}
	cmd.PrintErrln("wrote " + scriptPath)
	if err := managedBlock(rc, name, block); err != nil {
		return err
	}
	cmd.PrintErrln("wired " + rc + ", start a new shell or run: source " + rc)
	return nil
}

func managedBlock(path, name, body string) error {
	start := "# >>> " + name + " completion >>>"
	end := "# <<< " + name + " completion <<<"
	block := start + "\n" + body + "\n" + end
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := string(data)
	opening := strings.Index(content, start)
	closing := strings.Index(content, end)
	switch {
	case opening >= 0 && closing > opening:
		content = content[:opening] + block + content[closing+len(end):]
	default:
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += block + "\n"
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
