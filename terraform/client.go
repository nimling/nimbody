package terraform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/nimling/nimbody/cli"
	"github.com/nimling/nimbody/state"
)

type command struct {
	executable string
	config     string
}

func (c command) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	executable := c.executable
	if executable == "" {
		resolved, err := cli.Executable()
		if err != nil {
			return nil, err
		}
		executable = resolved
	}
	if c.config != "" {
		args = append(args, "--config", c.config)
	}
	run := exec.CommandContext(ctx, executable, args...)
	run.Stdin = bytes.NewReader(stdin)
	var out, errOut bytes.Buffer
	run.Stdout, run.Stderr = &out, &errOut
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, cli.EnvTerraform+"=") {
			run.Env = append(run.Env, entry)
		}
	}
	run.Env = append(run.Env, "NO_COLOR=1")
	if err := run.Run(); err != nil {
		text := strings.TrimSpace(errOut.String())
		if text == "" {
			text = err.Error()
		}
		return out.Bytes(), fmt.Errorf("%s %s: %s", executable, strings.Join(args, " "), text)
	}
	return out.Bytes(), nil
}

func (c command) GetDocument(ctx context.Context) (state.Document, error) {
	var doc state.Document
	out, err := c.run(ctx, nil, "state")
	if err != nil {
		return doc, err
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return doc, fmt.Errorf("the state schema is not json: %w", err)
	}
	return doc, nil
}

func (c command) GetRecord(ctx context.Context, group, key string) (state.Line, bool, error) {
	out, err := c.run(ctx, nil, "state", "export", "--key", group+":"+key, "-o", "-")
	if err != nil {
		return state.Line{}, false, err
	}
	file, err := state.Read(bytes.NewReader(out))
	if err != nil {
		return state.Line{}, false, err
	}
	for _, line := range file.Records {
		if line.Group == group && line.Key == key {
			return line, true, nil
		}
	}
	return state.Line{}, false, nil
}

func (c command) PostImport(ctx context.Context, doc state.Document, line *state.Line, dryRun bool, deletes ...string) (state.Result, error) {
	file := state.File{Manifest: state.Manifest{Format: state.FormatVersion, Service: doc.Service, Version: doc.Version, Scope: []state.Target{}}}
	args := []string{"state", "import", "-"}
	key := ""
	if line != nil {
		file.Records = []state.Line{*line}
		key = line.Group + ":" + line.Key
	}
	for _, removed := range deletes {
		args = append(args, "--delete", removed)
		key = removed
	}
	if dryRun {
		args = append(args, "--dry-run")
	}
	var body bytes.Buffer
	if err := file.Write(&body); err != nil {
		return state.Result{}, err
	}
	out, err := c.run(ctx, body.Bytes(), args...)
	var results []state.Result
	if json.Unmarshal(out, &results) != nil {
		if err == nil {
			err = errors.New("the import answer is not json")
		}
		return state.Result{}, err
	}
	for _, result := range results {
		if result.Group+":"+result.Key == key {
			return result, nil
		}
	}
	if err == nil {
		err = errors.New("the import answered nothing for " + key)
	}
	return state.Result{}, err
}
