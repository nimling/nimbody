package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/spf13/cobra"
)

func SkillCommand(name, document string) *cobra.Command {
	var project bool
	skill := &cobra.Command{
		Use:   "skill",
		Short: "The claude skill that teaches an agent to drive this cli",
	}
	get := &cobra.Command{
		Use:   "get",
		Short: "Print the skill document to stdout",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprint(cmd.OutOrStdout(), document)
			return err
		},
	}
	install := &cobra.Command{
		Use:   "install",
		Short: "Install the skill under ~/.claude/skills, or into the current project with --project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := InstallSkill(name, document, project)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "wrote "+path)
			return nil
		},
	}
	install.Flags().BoolVar(&project, "project", false, "install into .claude of the current directory instead of the home directory")
	skill.AddCommand(get, install)
	return skill
}

func InstallSkill(name, document string, project bool) (string, error) {
	root := ".claude"
	if !project {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".claude")
	}
	dir := filepath.Join(root, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "SKILL.md")
	return path, os.WriteFile(path, []byte(document), 0o644)
}

func SkillTreeCommand(primary string, tree fs.FS) *cobra.Command {
	var project bool
	skill := &cobra.Command{
		Use:   "skill",
		Short: "The claude skills that teach an agent to drive this cli",
	}
	get := &cobra.Command{
		Use:   "get [file]",
		Short: "Print SKILL.md, or a file of the skill tree, to stdout",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := path.Join(primary, "SKILL.md")
			if len(args) == 1 {
				name = path.Join(primary, args[0])
				if _, err := fs.Stat(tree, name); err != nil {
					name = path.Clean(args[0])
				}
			}
			body, err := fs.ReadFile(tree, name)
			if err != nil {
				return Rejected("no skill file is "+args[0], "Run '"+cmd.Root().Name()+" skill get' for the main document.")
			}
			_, err = cmd.OutOrStdout().Write(body)
			return err
		},
	}
	install := &cobra.Command{
		Use:   "install",
		Short: "Install every skill under ~/.claude/skills, or into the current project with --project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			written, err := InstallSkillTree(tree, project)
			for _, dir := range written {
				fmt.Fprintln(cmd.ErrOrStderr(), "wrote "+dir)
			}
			return err
		},
	}
	install.Flags().BoolVar(&project, "project", false, "install into .claude of the current directory instead of the home directory")
	skill.AddCommand(get, install)
	return skill
}

func InstallSkillTree(tree fs.FS, project bool) ([]string, error) {
	root := ".claude"
	if !project {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(home, ".claude")
	}
	entries, err := fs.ReadDir(tree, ".")
	if err != nil {
		return nil, err
	}
	var written []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, "skills", entry.Name())
		err := fs.WalkDir(tree, entry.Name(), func(name string, item fs.DirEntry, err error) error {
			if err != nil || item.IsDir() {
				return err
			}
			body, err := fs.ReadFile(tree, name)
			if err != nil {
				return err
			}
			target := filepath.Join(root, "skills", filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.WriteFile(target, body, 0o644)
		})
		if err != nil {
			return written, err
		}
		written = append(written, dir)
	}
	return written, nil
}
