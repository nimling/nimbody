package main

import (
	"os"

	"github.com/joho/godotenv"
	"github.com/nimling/nimbody/cli"
	"github.com/spf13/cobra"
)

type article struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func main() {
	_ = godotenv.Load(".env")

	root := &cobra.Command{
		Use:         "demo",
		Short:       "Operate the demo articles api",
		Annotations: map[string]string{cli.AnnotationMCPInput: cli.InputTyped},
	}
	articles := &cobra.Command{Use: "article", Short: "Articles"}
	get := cli.PutOutput[[]article](&cobra.Command{
		Use:     "get [id]",
		Short:   "Read articles, or one by id",
		Example: "demo article get 42",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rows := []article{{ID: "42", Title: "Orion"}}
			return cli.PrintValue(cmd.OutOrStdout(), rows, cli.FormatFor(cmd.OutOrStdout(), false))
		},
	})
	articles.AddCommand(get)
	root.AddCommand(articles, cli.MCPCommand(root, "demo", func() string { return "v0.0.0" }))
	os.Exit(cli.Execute(root))
}
