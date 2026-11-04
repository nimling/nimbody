package test

import (
	"bytes"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nimling/nimbody/cli"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const homeConfig = `default: tst
configs:
  tst:
    endpoint: https://tst.example.com
    app_slug: demo
    client_secret: tst-secret
  prd:
    endpoint: https://prd.example.com
    protected: "true"
`

type configRun struct {
	settings *cli.Settings
	root     *cobra.Command
	ran      []string
	stderr   *bytes.Buffer
	stdout   *bytes.Buffer
}

func configTree(t *testing.T) *configRun {
	t.Helper()
	run := &configRun{
		settings: &cli.Settings{Name: "demo", Prefix: "DEMO_", Keys: []cli.Setting{
			{Key: "endpoint", Env: "DEMO_ENDPOINT", Flag: "endpoint"},
			{Key: "app_slug", Env: "DEMO_APP_SLUG", Flag: "app"},
			{Key: "client_secret", Env: "DEMO_CLIENT_SECRET", Secret: true},
		}},
		stderr: &bytes.Buffer{},
		stdout: &bytes.Buffer{},
	}
	root := &cobra.Command{Use: "demo", SilenceErrors: true, SilenceUsage: true}
	root.PersistentFlags().String("endpoint", "", "endpoint")
	root.PersistentFlags().String("app", "", "app")
	run.settings.Flags(root)
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if err := run.settings.Load(cmd); err != nil {
			return err
		}
		return run.settings.Guard(cmd, args)
	}
	record := func(cmd *cobra.Command, args []string) error {
		run.ran = append(run.ran, cmd.CommandPath()+"="+os.Getenv("DEMO_ENDPOINT"))
		return nil
	}
	article := &cobra.Command{Use: "article"}
	article.AddCommand(
		&cobra.Command{Use: "get", RunE: record},
		&cobra.Command{Use: "create", RunE: record},
		&cobra.Command{Use: "delete <id>", Args: cobra.ExactArgs(1), RunE: record},
	)
	root.AddCommand(article, run.settings.ConfigCommand())
	root.SetOut(run.stdout)
	root.SetErr(run.stderr)
	run.root = root
	return run
}

func (r *configRun) execute(args ...string) error {
	r.root.SetArgs(args)
	return r.root.Execute()
}

func configHome(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, env := range []string{"DEMO_ENDPOINT", "DEMO_APP_SLUG", "DEMO_CLIENT_SECRET", "DEMO_CONFIG", "DEMO_EXTRA", "NIMBODY_MCP"} {
		t.Setenv(env, "")
		os.Unsetenv(env)
	}
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".demo"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".demo", cli.ConfigFile), []byte(homeConfig), 0o600))
	project := filepath.Join(home, "work", "repo", "sub")
	require.NoError(t, os.MkdirAll(project, 0o755))
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	require.NoError(t, os.Chdir(project))
	return home, filepath.Join(home, "work", "repo")
}

func writeLocal(t *testing.T, repo, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".demo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".demo", cli.ConfigFile), []byte(body), 0o644))
}

func layer(loaded cli.Loaded, key string) cli.Source {
	for _, source := range loaded.Values {
		if source.Key == key {
			return source
		}
	}
	return cli.Source{}
}

func TestConfigDefaultFromHomeSetsTheEnvironment(t *testing.T) {
	configHome(t)
	run := configTree(t)
	require.NoError(t, run.execute("article", "get"))
	assert.Equal(t, []string{"demo article get=https://tst.example.com"}, run.ran)
	assert.Equal(t, "tst", run.settings.Loaded.Config)
	assert.Equal(t, cli.LayerConfig, layer(run.settings.Loaded, "endpoint").Layer)
	assert.Equal(t, "xxxxxxcret", layer(run.settings.Loaded, "client_secret").Value, "a secret is masked in the report")
	assert.Equal(t, "tst-secret", os.Getenv("DEMO_CLIENT_SECRET"))
}

func TestConfigFlagAndEnvironmentPickTheNamedConfig(t *testing.T) {
	configHome(t)
	run := configTree(t)
	require.NoError(t, run.execute("article", "get", "--config", "prd"))
	assert.Equal(t, "prd", run.settings.Loaded.Config)
	assert.True(t, run.settings.Loaded.Protected)

	t.Setenv("DEMO_CONFIG", "prd")
	run = configTree(t)
	require.NoError(t, run.execute("article", "get"))
	assert.Equal(t, "prd", run.settings.Loaded.Config)
}

func TestConfigUnknownNameIsRejected(t *testing.T) {
	configHome(t)
	err := configTree(t).execute("article", "get", "--config", "stg")
	assert.Equal(t, cli.ExitRejected, cli.ExitCode(err))
	assert.Contains(t, err.Error(), "no config named stg")
}

func TestConfigLocalTakesOverUntilTurnedOff(t *testing.T) {
	_, repo := configHome(t)
	writeLocal(t, repo, "use: prd\napp_slug: local-app\n")

	run := configTree(t)
	require.NoError(t, run.execute("article", "get"))
	assert.Equal(t, "prd", run.settings.Loaded.Config, "the local directory found walking up picks the config")
	assert.Equal(t, cli.LayerLocal, layer(run.settings.Loaded, "app_slug").Layer)
	assert.Equal(t, "local-app", os.Getenv("DEMO_APP_SLUG"))

	os.Unsetenv("DEMO_APP_SLUG")
	os.Unsetenv("DEMO_ENDPOINT")
	run = configTree(t)
	require.NoError(t, run.execute("article", "get", "--global"))
	assert.Equal(t, "tst", run.settings.Loaded.Config, "--global ignores the local directory")

	os.Unsetenv("DEMO_APP_SLUG")
	os.Unsetenv("DEMO_ENDPOINT")
	run = configTree(t)
	require.NoError(t, run.execute("config", "local", "off"))
	run = configTree(t)
	require.NoError(t, run.execute("article", "get"))
	assert.Equal(t, "tst", run.settings.Loaded.Config, "a local directory turned off is skipped")
	assert.Empty(t, run.settings.Loaded.Local)
}

func TestConfigHomeDirectoryIsNeverReadAsLocal(t *testing.T) {
	home, _ := configHome(t)
	require.NoError(t, os.Chdir(home))
	run := configTree(t)
	require.NoError(t, run.execute("article", "get"))
	assert.Empty(t, run.settings.Loaded.Local)
}

func TestConfigAccountHomeIsNeverReadAsLocalWhenHomeMoves(t *testing.T) {
	account, err := user.Current()
	require.NoError(t, err)
	folder := filepath.Join(account.HomeDir, ".demo")
	if _, err := os.Stat(folder); err == nil {
		t.Skip(folder + " exists on this machine")
	}
	require.NoError(t, os.MkdirAll(folder, 0o700))
	t.Cleanup(func() { os.RemoveAll(folder) })
	require.NoError(t, os.WriteFile(filepath.Join(folder, cli.ConfigFile), []byte(homeConfig), 0o600))
	configHome(t)
	require.NoError(t, os.Chdir(account.HomeDir))
	run := configTree(t)
	require.NoError(t, run.execute("article", "get"))
	assert.Empty(t, run.settings.Loaded.Local)
}

func TestConfigSecretInLocalIsRefused(t *testing.T) {
	_, repo := configHome(t)
	writeLocal(t, repo, "client_secret: leaked\n")
	err := configTree(t).execute("article", "get")
	assert.Equal(t, cli.ExitUnauthenticated, cli.ExitCode(err))
	assert.Contains(t, err.Error(), "client_secret")
}

func TestConfigEnvFileLayersAboveConfigAndBelowTheEnvironment(t *testing.T) {
	home, repo := configHome(t)
	writeLocal(t, repo, "use: tst\n")
	file := filepath.Join(home, "values.env")
	require.NoError(t, os.WriteFile(file, []byte("DEMO_ENDPOINT=https://file.example.com\nDEMO_APP_SLUG=file-app\nDEMO_EXTRA=kept\n"), 0o600))

	t.Setenv("DEMO_APP_SLUG", "process-app")
	run := configTree(t)
	require.NoError(t, run.execute("article", "get", "--env", file))
	assert.Equal(t, cli.LayerEnvFile, layer(run.settings.Loaded, "endpoint").Layer)
	assert.Equal(t, "https://file.example.com", os.Getenv("DEMO_ENDPOINT"))
	assert.Equal(t, cli.LayerEnv, layer(run.settings.Loaded, "app_slug").Layer)
	assert.Equal(t, "process-app", os.Getenv("DEMO_APP_SLUG"))
	assert.Equal(t, "kept", os.Getenv("DEMO_EXTRA"), "an env file loads every key, not only the cli's own")
}

func TestConfigFlagIsReportedAsTheFlagLayer(t *testing.T) {
	configHome(t)
	run := configTree(t)
	require.NoError(t, run.execute("article", "get", "--endpoint", "https://flag.example.com"))
	assert.Equal(t, cli.LayerFlag, layer(run.settings.Loaded, "endpoint").Layer)
	assert.Equal(t, "https://flag.example.com", layer(run.settings.Loaded, "endpoint").Value)
}

func TestGuardLetsReadsAndUnprotectedWritesThrough(t *testing.T) {
	configHome(t)
	run := configTree(t)
	require.NoError(t, run.execute("article", "create"))
	require.NoError(t, configTree(t).execute("article", "get", "--config", "prd"))
}

func TestGuardRefusesAProtectedWriteWithoutYes(t *testing.T) {
	configHome(t)
	run := configTree(t)
	err := run.execute("article", "delete", "42", "--config", "prd")
	assert.Equal(t, cli.ExitRefused, cli.ExitCode(err))
	assert.Empty(t, run.ran)
	assert.Contains(t, run.stderr.String(), "plan: demo article delete 42")
	assert.Contains(t, run.stderr.String(), "config prd")

	run = configTree(t)
	require.NoError(t, run.execute("article", "delete", "42", "--config", "prd", "--yes"))
	assert.Len(t, run.ran, 1)
}

func TestGuardPlanPrintsAndStops(t *testing.T) {
	configHome(t)
	run := configTree(t)
	err := run.execute("article", "create", "--plan")
	assert.Equal(t, cli.ExitOK, cli.ExitCode(err))
	assert.Empty(t, run.ran)
	assert.True(t, strings.HasPrefix(run.stderr.String(), "plan: demo article create"))
}

func TestGuardLeavesToolingAlone(t *testing.T) {
	configHome(t)
	require.NoError(t, configTree(t).execute("config", "use", "prd", "--config", "prd"))
}

func TestConfigCommandsWriteTheFiles(t *testing.T) {
	home, repo := configHome(t)
	require.NoError(t, os.Chdir(repo))

	require.NoError(t, configTree(t).execute("config", "set", "--to", "stg", "endpoint=https://stg.example.com", "protected=true"))
	require.NoError(t, configTree(t).execute("config", "use", "stg"))
	body, err := os.ReadFile(filepath.Join(home, ".demo", cli.ConfigFile))
	require.NoError(t, err)
	assert.Contains(t, string(body), "default: stg")
	assert.Contains(t, string(body), "https://stg.example.com")

	require.NoError(t, configTree(t).execute("config", "init", "tst"))
	require.NoError(t, configTree(t).execute("config", "set", "--local", "app_slug=repo-app"))
	err = configTree(t).execute("config", "set", "--local", "client_secret=x")
	assert.Equal(t, cli.ExitUnauthenticated, cli.ExitCode(err))
	require.NoError(t, configTree(t).execute("config", "use", "prd"))
	local, err := os.ReadFile(filepath.Join(repo, ".demo", cli.ConfigFile))
	require.NoError(t, err)
	assert.Contains(t, string(local), "use: prd", "use writes the local directory when one is in effect")
	assert.Contains(t, string(local), "app_slug: repo-app")

	err = configTree(t).execute("config", "set", "colour=red")
	assert.Equal(t, cli.ExitRejected, cli.ExitCode(err))
}
