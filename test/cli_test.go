package test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/nimling/nimbody/cli"
	"github.com/nimling/nimbody/mcp"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

func cliTree() *cobra.Command {
	root := &cobra.Command{Use: "demo"}
	article := &cobra.Command{Use: "article", Short: "Articles"}
	run := func(cmd *cobra.Command, args []string) error { return nil }
	get := &cobra.Command{Use: "get [id]", Short: "Read articles", Example: "demo article get 42", RunE: run}
	get.Flags().Bool("json", false, "print json")
	create := &cobra.Command{Use: "create", Short: "Add an article", RunE: run}
	remove := &cobra.Command{Use: "delete <id>", Short: "Remove an article", RunE: run}
	hidden := &cobra.Command{Use: "secret", Short: "Kept from tools", RunE: run, Annotations: map[string]string{cli.AnnotationMCP: cli.AccessNone}}
	article.AddCommand(get, create, remove, hidden)
	login := &cobra.Command{Use: "login", Short: "Sign in", RunE: run}
	root.AddCommand(article, login, cli.SkillCommand("demo", "---\nname: demo\n---\n"))
	return root
}

func TestCLIToolsAnnotateEveryCommandAndSkipTooling(t *testing.T) {
	tools := cli.Tools(cliTree(), "/bin/demo")
	byName := map[string]mcp.Tool{}
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	assert.Len(t, byName, 3, "get, create and delete, without login, skill or the opted out command")
	assert.Equal(t, true, byName["article_get"].Annotations["readOnlyHint"])
	assert.Contains(t, byName["article_get"].Description, "example: demo article get 42")
	assert.NotContains(t, byName["article_get"].Description, "confirm with the user")
	assert.Equal(t, false, byName["article_create"].Annotations["readOnlyHint"])
	assert.Contains(t, byName["article_create"].Description, "confirm with the user")
	assert.Equal(t, true, byName["article_delete"].Annotations["destructiveHint"])
}

func TestCLIAccessAnnotationOverridesTheVerb(t *testing.T) {
	cmd := &cobra.Command{Use: "get", Annotations: map[string]string{cli.AnnotationMCP: mcp.AccessWrite}}
	assert.Equal(t, mcp.AccessWrite, cli.Access(cmd))
	assert.Equal(t, mcp.AccessRead, cli.Access(&cobra.Command{Use: "list"}))
	assert.Equal(t, mcp.AccessDestructive, cli.Access(&cobra.Command{Use: "revoke"}))
}

func TestCLIExecuteMapsUnknownCommandToTwoWithAHint(t *testing.T) {
	root := cliTree()
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"no-such"})
	assert.Equal(t, cli.ExitRejected, cli.Execute(root))
	assert.Contains(t, stderr.String(), "unknown command")
	assert.Contains(t, stderr.String(), "Run 'demo --help' for usage.")
}

func TestCLIExitCodes(t *testing.T) {
	assert.Equal(t, cli.ExitOK, cli.ExitCode(nil))
	assert.Equal(t, cli.ExitFailure, cli.ExitCode(errors.New("boom")))
	assert.Equal(t, cli.ExitRejected, cli.ExitCode(cli.Rejected("bad", "")))
	assert.Equal(t, cli.ExitRefused, cli.ExitCode(cli.Refused("no", "")))
	assert.Equal(t, cli.ExitUnauthenticated, cli.ExitCode(cli.Unauthenticated("who", "run demo login")))
	assert.Equal(t, cli.ExitInterrupted, cli.ExitCode(context.Canceled))
	assert.Equal(t, cli.ExitRejected, cli.ExitCode(errors.New(`unknown flag: --nope`)))

	var stderr bytes.Buffer
	cli.Report(&stderr, "demo", cli.Unauthenticated("not signed in", "Run 'demo login'."))
	assert.Equal(t, "demo: not signed in\nRun 'demo login'.\n", stderr.String())
}

func TestCLISkillGetWritesToStdout(t *testing.T) {
	root := cliTree()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"skill", "get"})
	assert.NoError(t, root.Execute())
	assert.Contains(t, stdout.String(), "name: demo")
	assert.Empty(t, stderr.String())
}

func TestCLIPrintFormats(t *testing.T) {
	rows := []byte(`[{"id":"a1","title":"Orion","weight":0},{"id":"b2","title":"Ny nettside","weight":1}]`)

	var plain bytes.Buffer
	assert.NoError(t, cli.Print(&plain, rows, cli.FormatPlain))
	assert.Equal(t, "id\ttitle\tweight\na1\tOrion\t0\nb2\tNy nettside\t1\n", plain.String())

	var table bytes.Buffer
	assert.NoError(t, cli.Print(&table, rows, cli.FormatTable))
	assert.Contains(t, table.String(), "ID  TITLE        WEIGHT")
	assert.Contains(t, table.String(), "2 rows")
	assert.NotContains(t, table.String(), "\x1b[", "a buffer is no terminal, so no colour")

	var asJSON bytes.Buffer
	assert.NoError(t, cli.Print(&asJSON, rows, cli.FormatJSON))
	assert.True(t, strings.HasPrefix(asJSON.String(), "[\n  {"))

	assert.Equal(t, cli.FormatJSON, cli.FormatFor(&bytes.Buffer{}, false), "a pipe gets json so scripts, jq and mcp read every field")
	assert.Equal(t, cli.FormatJSON, cli.FormatFor(&bytes.Buffer{}, true))
}

func TestCLIColourObeysNoColor(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")
	assert.True(t, cli.Colour(&bytes.Buffer{}))
	t.Setenv("NO_COLOR", "1")
	assert.False(t, cli.Colour(&bytes.Buffer{}))
}

func TestMCPAnnotateMarksWritesForConfirmation(t *testing.T) {
	read := mcp.Annotate(mcp.Tool{Description: "read"}, mcp.AccessRead)
	assert.Equal(t, "read", read.Description)
	assert.Equal(t, true, read.Annotations["readOnlyHint"])
	destructive := mcp.Annotate(mcp.Tool{Description: "drop"}, mcp.AccessDestructive)
	assert.Equal(t, true, destructive.Annotations["destructiveHint"])
	assert.Contains(t, destructive.Description, "confirm with the user")
}

func demoPlugin() cli.Plugin {
	return cli.Plugin{Name: "demo", Owner: "nimling", Description: "Demo", Skill: "---\nname: demo\n---\n", Version: func() string { return "v1.2.3" }}
}

func TestCLIPluginWritesManifestMarketplaceAndSkill(t *testing.T) {
	dir := t.TempDir()
	version, err := demoPlugin().Write(dir, "/bin/demo")
	assert.NoError(t, err)
	assert.True(t, strings.HasPrefix(version, "v1.2.3-"), "the version is the cli version plus a content hash")

	manifest, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	assert.NoError(t, err)
	assert.Contains(t, string(manifest), `"command": "/bin/demo"`)
	assert.Contains(t, string(manifest), `"mcp"`)
	skill, err := os.ReadFile(filepath.Join(dir, "skills", "demo", "SKILL.md"))
	assert.NoError(t, err)
	assert.Contains(t, string(skill), "name: demo")

	other := demoPlugin()
	other.Skill += "changed\n"
	changed, _ := other.Write(t.TempDir(), "/bin/demo")
	assert.NotEqual(t, version, changed, "a changed skill changes the version, so status sees drift")
}

func TestCLIPluginDesktopKeepsEveryOtherEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", home)
	path, err := cli.DesktopConfigPath()
	assert.NoError(t, err)
	assert.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	assert.NoError(t, os.WriteFile(path, []byte(`{"theme":"dark","mcpServers":{"other":{"command":"other"}}}`), 0o644))

	_, err = demoPlugin().InstallDesktop()
	assert.NoError(t, err)
	body, _ := os.ReadFile(path)
	assert.Contains(t, string(body), `"theme": "dark"`)
	assert.Contains(t, string(body), `"other"`)
	assert.Contains(t, string(body), `"demo"`)
}

func TestCLIPluginStatusReportsMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want, have, err := demoPlugin().Drift()
	assert.NoError(t, err)
	assert.NotEmpty(t, want)
	assert.Empty(t, have)
}

func TestCLIToolsKeepARunnableCommandThatHasChildren(t *testing.T) {
	root := &cobra.Command{Use: "demo"}
	run := func(cmd *cobra.Command, args []string) error { return nil }
	look := &cobra.Command{Use: "look", Short: "Look at the page", RunE: run}
	look.AddCommand(&cobra.Command{Use: "marks", Short: "Look with marks", RunE: run})
	root.AddCommand(look)
	names := []string{}
	for _, tool := range cli.Tools(root, "/bin/demo") {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(t, []string{"look", "look_marks"}, names)
}

func skillTree() fstest.MapFS {
	return fstest.MapFS{
		"demo/SKILL.md":               {Data: []byte("---\nname: demo\n---\n")},
		"demo/references/commands.md": {Data: []byte("# commands\n")},
		"demo-pr/SKILL.md":            {Data: []byte("---\nname: demo-pr\n---\n")},
	}
}

func TestCLISkillTreeGetsAnyFileAndInstallsEverySkill(t *testing.T) {
	for args, want := range map[string]string{"": "name: demo\n", "references/commands.md": "# commands", "demo-pr/SKILL.md": "name: demo-pr"} {
		root := &cobra.Command{Use: "demo"}
		root.AddCommand(cli.SkillTreeCommand("demo", skillTree()))
		var stdout bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(append([]string{"skill", "get"}, strings.Fields(args)...))
		assert.NoError(t, root.Execute(), args)
		assert.Contains(t, stdout.String(), want, args)
	}

	t.Setenv("HOME", t.TempDir())
	written, err := cli.InstallSkillTree(skillTree(), false)
	assert.NoError(t, err)
	assert.Len(t, written, 2)
	home, _ := os.UserHomeDir()
	_, err = os.Stat(filepath.Join(home, ".claude", "skills", "demo", "references", "commands.md"))
	assert.NoError(t, err)
}

func TestCLIPluginTreeVersionMovesWithAnySkill(t *testing.T) {
	plugin := demoPlugin()
	plugin.Tree = skillTree()
	dir := t.TempDir()
	first, err := plugin.Write(dir, "/bin/demo")
	assert.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "skills", "demo-pr", "SKILL.md"))
	assert.NoError(t, err)

	changed := skillTree()
	changed["demo-pr/SKILL.md"] = &fstest.MapFile{Data: []byte("---\nname: demo-pr\n---\nmore\n")}
	plugin.Tree = changed
	second, _ := plugin.Write(t.TempDir(), "/bin/demo")
	assert.NotEqual(t, first, second)
}

func TestCLIFormatIsJSONWhenAnMCPToolRunsTheCli(t *testing.T) {
	t.Setenv("NIMBODY_MCP", "1")
	assert.Equal(t, cli.FormatJSON, cli.FormatFor(&bytes.Buffer{}, false), "a tool result keeps every field, nested ones included")
}

func TestCLIToolsTakeThePageACommandNames(t *testing.T) {
	root := &cobra.Command{Use: "demo"}
	run := func(cmd *cobra.Command, args []string) error { return nil }
	root.AddCommand(
		&cobra.Command{Use: "open", Short: "Open a view", RunE: run, Annotations: map[string]string{cli.AnnotationMCP: mcp.AccessRead, cli.AnnotationMCPPage: "open"}},
		&cobra.Command{Use: "get", Short: "Read a view", RunE: run},
	)
	byName := map[string]mcp.Tool{}
	for _, tool := range cli.Tools(root, "/bin/demo") {
		byName[tool.Name] = tool
	}
	assert.Equal(t, "open", byName["open"].Page)
	assert.Equal(t, "", byName["get"].Page)
}

func TestCLIToolsTakeTheImageKeyACommandNames(t *testing.T) {
	root := &cobra.Command{Use: "demo"}
	run := func(cmd *cobra.Command, args []string) error { return nil }
	root.AddCommand(
		&cobra.Command{Use: "shot", Short: "A screenshot", RunE: run, Annotations: map[string]string{cli.AnnotationMCP: mcp.AccessRead, cli.AnnotationMCPImage: "path"}},
		&cobra.Command{Use: "get", Short: "Read a view", RunE: run},
	)
	byName := map[string]mcp.Tool{}
	for _, tool := range cli.Tools(root, "/bin/demo") {
		byName[tool.Name] = tool
	}
	assert.Equal(t, "path", byName["shot"].Image)
	assert.Equal(t, "", byName["get"].Image)
}
