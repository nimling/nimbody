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

	table := cli.Table{Name: "demo", Send: client.Send, Output: output, Resources: []cli.Resource{{
		Name: "article", Short: "Articles",
		Verbs: []cli.Verb{
			{Name: "get", Short: "Read articles, or one by id", Method: http.MethodGet, Paths: []string{"/api/articles", "/api/articles/{id}"}},
			{Name: "create", Short: "Add an article", Method: http.MethodPost, Paths: []string{"/api/articles"}, Body: true},
			{Name: "update", Short: "Change fields of an article", Method: http.MethodPatch, Paths: []string{"", "/api/articles/{id}"}, Body: true},
			{Name: "delete", Short: "Remove an article", Method: http.MethodDelete, Paths: []string{"", "/api/articles/{id}"}},
		},
	}}}
	table.Mount(root)
	if get, _, err := root.Find([]string{"article", "get"}); err == nil {
		cli.PutOutput[[]article](get)
	}
	root.AddCommand(settings.ConfigCommand(), cli.MCPCommand(root, "demo", client.Version))
	client.Mount(root)
	os.Exit(cli.Execute(root))
}
