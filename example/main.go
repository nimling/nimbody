package main

import (
	"net/http"
	"os"

	"github.com/joho/godotenv"
	"github.com/nimling/nimbody/cli"
	"github.com/spf13/cobra"
)

type article struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

var settings = &cli.Settings{Name: "demo", Prefix: "DEMO_", Keys: []cli.Setting{
	{Key: cli.KeyEndpoint, Env: "DEMO_ENDPOINT", Flag: "endpoint"},
	{Key: cli.KeyAppSlug, Env: "DEMO_APP_SLUG", Flag: "app"},
	{Key: cli.KeyClientID, Env: "DEMO_CLIENT_ID", Flag: "client-id"},
	{Key: cli.KeyClientSecret, Env: "DEMO_CLIENT_SECRET", Secret: true},
}}

var output = &cli.Output{}

var client = &cli.Client{Settings: settings, Product: "Demo", Version: func() string { return "v0.0.0" }, Output: output}

func main() {
	_ = godotenv.Load(".env")

	root := &cobra.Command{
		Use:         "demo",
		Short:       "Operate the demo articles api",
		Annotations: map[string]string{cli.AnnotationMCPInput: cli.InputTyped},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if err := settings.Load(cmd); err != nil {
				return err
			}
			return settings.Guard(cmd, args)
		},
	}
	flags := root.PersistentFlags()
	flags.String("endpoint", "", "service base url, defaults to DEMO_ENDPOINT")
	flags.String("app", "", "app slug, defaults to DEMO_APP_SLUG")
	flags.String("client-id", "", "machine client id, defaults to DEMO_CLIENT_ID")
	settings.Flags(root)
	output.Flags(root)

	articles := &cobra.Command{Use: "article", Short: "Articles"}
	get := cli.PutOutput[[]article](&cobra.Command{
		Use:     "get [id]",
		Short:   "Read articles, or one by id",
		Example: "demo article get 42",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/api/articles"
			if len(args) == 1 {
				path += "/" + args[0]
			}
			body, err := client.Send(cmd, http.MethodGet, path, output.GetQuery(), "", nil)
			if err != nil {
				return err
			}
			return output.Print(cmd.OutOrStdout(), cmd.ErrOrStderr(), body)
		},
	})
	articles.AddCommand(get)
	root.AddCommand(articles, settings.ConfigCommand(), cli.MCPCommand(root, "demo", client.Version))
	client.Mount(root)
	os.Exit(cli.Execute(root))
}
