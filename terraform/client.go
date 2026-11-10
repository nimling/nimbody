package terraform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/nimling/nimbody/cli"
	"github.com/nimling/nimbody/state"
	"github.com/spf13/cobra"
)

type client struct {
	api *cli.Client
}

func (c client) send(ctx context.Context, method, path string, query url.Values, contentType string, body []byte) ([]byte, error) {
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetErr(io.Discard)
	return c.api.Send(cmd, method, path, query, contentType, body)
}

func (c client) GetRecord(ctx context.Context, group, key string) (state.Line, bool, error) {
	payload, err := json.Marshal(state.ExportRequest{Targets: []state.Target{{Group: group, Keys: []string{key}}}})
	if err != nil {
		return state.Line{}, false, err
	}
	out, err := c.send(ctx, http.MethodPost, cli.PathState+"/export", nil, "application/json", payload)
	if err != nil {
		return state.Line{}, false, err
	}
	file, err := state.Read(bytes.NewReader(cli.GetStateLines(out)))
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

func (c client) PostImport(ctx context.Context, doc state.Document, line *state.Line, dryRun bool, deletes ...string) (state.Result, error) {
	file := state.File{Manifest: state.Manifest{Format: state.FormatVersion, Service: doc.Service, Version: doc.Version, Scope: []state.Target{}}}
	query := url.Values{}
	key := ""
	if line != nil {
		file.Records = []state.Line{*line}
		key = line.Group + ":" + line.Key
	}
	for _, removed := range deletes {
		query.Add("delete", removed)
		key = removed
	}
	if dryRun {
		query.Set("dry_run", "true")
	}
	var body bytes.Buffer
	if err := file.Write(&body); err != nil {
		return state.Result{}, err
	}
	out, err := c.send(ctx, http.MethodPost, cli.PathState+"/import", query, state.MIMENDJSON, body.Bytes())
	if err != nil {
		return state.Result{}, err
	}
	var wrapped struct {
		Data state.ImportResponse `json:"data"`
	}
	if err := json.Unmarshal(out, &wrapped); err != nil {
		return state.Result{}, errors.New("the import answer is not json: " + err.Error())
	}
	for _, result := range wrapped.Data.Results {
		if result.Group+":"+result.Key == key {
			return result, nil
		}
	}
	return state.Result{}, errors.New("the import answered nothing for " + key)
}
