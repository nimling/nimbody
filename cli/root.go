package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func Root(root *cobra.Command, settings *Settings, output *Output) *cobra.Command {
	for _, key := range settings.Keys {
		if key.Secret && key.Flag != "" {
			panic(settings.Name + ": " + key.Key + " is a secret and a secret is never a flag, it comes from " + key.Env + " or the home config; drop Flag from its Setting")
		}
	}
	root.SilenceErrors = true
	root.SilenceUsage = true
	if root.Annotations == nil {
		root.Annotations = map[string]string{}
	}
	if _, set := root.Annotations[AnnotationMCPInput]; !set {
		root.Annotations[AnnotationMCPInput] = InputTyped
	}
	settings.Flags(root)
	output.flags(root, true)
	own := root.PersistentPreRunE
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if err := settings.Load(cmd); err != nil {
			return err
		}
		if err := settings.Guard(cmd, args); err != nil {
			return err
		}
		if own != nil {
			return own(cmd, args)
		}
		return nil
	}
	return root
}

func wantsJSON(cmd *cobra.Command) bool {
	flag := cmd.Flags().Lookup("json")
	return flag != nil && flag.Value.String() == "true"
}

func manual(document string, style Style) string {
	body := document
	if strings.HasPrefix(body, "---") {
		if end := strings.Index(body[3:], "\n---"); end >= 0 {
			body = body[3+end+4:]
		}
	}
	body = strings.TrimSpace(body) + "\n"
	if !style.on {
		return body
	}
	var out strings.Builder
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			out.WriteString(style.Bold(strings.TrimPrefix(line, "## ")) + "\n")
		case strings.HasPrefix(line, "# "):
			out.WriteString(style.Bold(style.Accent(strings.TrimPrefix(line, "# "))) + "\n")
		default:
			out.WriteString(line + "\n")
		}
	}
	return strings.TrimSuffix(out.String(), "\n")
}

func HelpCommand(banner, document string) *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "The full manual, or the help of one command",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				if Terminal(out) && banner != "" {
					fmt.Fprint(out, banner)
				}
				_, err := fmt.Fprint(out, manual(document, StyleFor(out)))
				return err
			}
			target, _, err := cmd.Root().Find(args)
			if err != nil || target == nil || target == cmd.Root() {
				return Rejected("unknown command "+strings.Join(args, " "), "Run '"+cmd.Root().Name()+" help' for the manual.")
			}
			target.InitDefaultHelpFlag()
			return target.Help()
		},
	}
}

func HelpFunc(banner string, base func(*cobra.Command, []string)) func(*cobra.Command, []string) {
	return func(cmd *cobra.Command, args []string) {
		if cmd == cmd.Root() && banner != "" && Terminal(cmd.OutOrStdout()) {
			fmt.Fprint(cmd.OutOrStdout(), banner)
		}
		base(cmd, args)
	}
}
