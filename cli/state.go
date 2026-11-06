package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/nimling/nimbody/mcp"
	"github.com/nimling/nimbody/state"
	"github.com/spf13/cobra"
)

type Send func(cmd *cobra.Command, method, path string, query url.Values, contentType string, body []byte) ([]byte, error)

func StateCommand(send Send, output *Output) *cobra.Command {
	if output == nil {
		output = &Output{}
	}
	root := &cobra.Command{
		Use:         "state",
		Short:       "The state schema: every group with its create body schema, dependencies, references, filters and field flags",
		Example:     "state",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{AnnotationMCP: mcp.AccessRead},
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := send(cmd, http.MethodGet, "/state", nil, "", nil)
			if err != nil {
				return err
			}
			return output.Print(cmd.OutOrStdout(), cmd.ErrOrStderr(), body)
		},
	}
	root.AddCommand(stateExport(send, output), stateImport(send, output))
	return root
}

func stateExport(send Send, output *Output) *cobra.Command {
	var targets, keys []string
	var store, history, blobs bool
	var file string
	cmd := &cobra.Command{
		Use:         "export",
		Short:       "Export records as state lines: a manifest, one line per record and one per blob",
		Example:     "state export --target room:location=hq --key room:north --blobs -o rooms.ndjson",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{AnnotationMCP: mcp.AccessRead},
		RunE: func(cmd *cobra.Command, args []string) error {
			in := state.ExportRequest{Store: store, History: history, Blobs: blobs}
			for _, raw := range targets {
				target, err := state.ParseTarget(raw)
				if err != nil {
					return Rejected(err.Error(), "")
				}
				in.Targets = append(in.Targets, target)
			}
			for _, raw := range keys {
				group, key, err := state.ParseKey(raw)
				if err != nil {
					return Rejected(err.Error(), "")
				}
				found := false
				for i := range in.Targets {
					if in.Targets[i].Group == group {
						in.Targets[i].Keys = append(in.Targets[i].Keys, key)
						found = true
					}
				}
				if !found {
					in.Targets = append(in.Targets, state.Target{Group: group, Keys: []string{key}})
				}
			}
			payload, err := json.Marshal(in)
			if err != nil {
				return err
			}
			body, err := send(cmd, http.MethodPost, "/state/export", nil, "application/json", payload)
			if err != nil {
				return err
			}
			if store {
				return output.Print(cmd.OutOrStdout(), cmd.ErrOrStderr(), body)
			}
			lines := stateLines(body)
			read, err := state.Read(bytes.NewReader(lines))
			if err != nil {
				return &Error{Code: ExitFailure, Message: "the export is not state lines", Cause: err}
			}
			if file == "" || file == "-" {
				if _, err := cmd.OutOrStdout().Write(lines); err != nil {
					return err
				}
			} else if err := os.WriteFile(file, lines, 0o600); err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), stateSummary(read.Manifest, file))
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringArrayVar(&targets, "target", nil, "group to export, group:field=value,field=value to filter it, repeatable; none exports every group")
	flags.StringArrayVar(&keys, "key", nil, "one record as group:key, repeatable")
	flags.BoolVar(&store, "store", false, "save the export as an asset on the service and print its id")
	flags.BoolVar(&history, "history", false, "include the history of each record")
	flags.BoolVar(&blobs, "blobs", false, "inline the blobs of each record")
	flags.StringVarP(&file, "output", "o", "-", "file to write the state lines to, - for stdout")
	return cmd
}

func stateImport(send Send, output *Output) *cobra.Command {
	var targets, deletes []string
	var mode, asset string
	var dryRun bool
	var chunk int
	cmd := &cobra.Command{
		Use:         "import [file]",
		Short:       "Import state lines from a file, stdin or a stored asset: upsert, create or sync, with deletes by key and a dry run",
		Example:     "state import rooms.ndjson --mode sync --target room:location=hq --dry-run",
		Args:        cobra.MaximumNArgs(1),
		Annotations: map[string]string{AnnotationMCP: mcp.AccessDestructive},
		RunE: func(cmd *cobra.Command, args []string) error {
			in := state.ImportRequest{AssetID: asset, Mode: mode, DryRun: dryRun}
			query := url.Values{}
			if mode != "" {
				query.Set("mode", mode)
			}
			if dryRun {
				query.Set("dry_run", "true")
			}
			for _, raw := range targets {
				target, err := state.ParseTarget(raw)
				if err != nil {
					return Rejected(err.Error(), "")
				}
				in.Targets = append(in.Targets, target)
				query.Add("target", raw)
			}
			for _, raw := range deletes {
				group, key, err := state.ParseKey(raw)
				if err != nil {
					return Rejected(err.Error(), "")
				}
				if in.Delete == nil {
					in.Delete = map[string][]string{}
				}
				in.Delete[group] = append(in.Delete[group], key)
				query.Add("delete", raw)
			}
			var body []byte
			var err error
			switch {
			case len(args) == 1 && asset != "":
				return Rejected("import takes a file or --asset, not both", "")
			case len(args) == 1:
				lines, err := readState(cmd.InOrStdin(), args[0])
				if err != nil {
					return err
				}
				if chunk > 0 && len(lines) > chunk<<20 {
					body, err = stateImportStaged(cmd, send, in, lines, chunk<<20)
				} else {
					body, err = send(cmd, http.MethodPost, "/state/import", query, state.MIMENDJSON, lines)
				}
				if err != nil {
					return err
				}
			case asset != "":
				payload, err := json.Marshal(in)
				if err != nil {
					return err
				}
				body, err = send(cmd, http.MethodPost, "/state/import", nil, "application/json", payload)
				if err != nil {
					return err
				}
			default:
				return Rejected("import takes a file, - for stdin, or --asset", "Run '"+cmd.Root().Name()+" state export -o <file>' for a file.")
			}
			var wrapped struct {
				Data state.ImportResponse `json:"data"`
			}
			if err = json.Unmarshal(body, &wrapped); err != nil {
				return &Error{Code: ExitFailure, Message: "the import answer is not json", Cause: err}
			}
			if err := output.PrintValue(cmd.OutOrStdout(), wrapped.Data.Results); err != nil {
				return err
			}
			for _, line := range stateGroupCounts(wrapped.Data) {
				fmt.Fprintln(cmd.ErrOrStderr(), line)
			}
			fmt.Fprintln(cmd.ErrOrStderr(), stateCounts(wrapped.Data))
			if refused := wrapped.Data.Counts[state.OutcomeRefused]; refused > 0 {
				return Refused(fmt.Sprintf("%d of %d records refused", refused, len(wrapped.Data.Results)), "Each refused record names its error in the error column.")
			}
			return nil
		},
	}
	flags := cmd.Flags()
	flags.BoolVar(&dryRun, "dry-run", false, "list every outcome and write nothing")
	flags.StringVar(&mode, "mode", "", "upsert (default), create, or sync, which also deletes what the targets reach and the file lacks")
	flags.StringArrayVar(&deletes, "delete", nil, "delete a record as group:key, repeatable")
	flags.StringArrayVar(&targets, "target", nil, "import only this group, group:field=value to narrow what a sync deletes, repeatable")
	flags.StringVar(&asset, "asset", "", "import a stored export by its asset id instead of a file")
	flags.IntVar(&chunk, "chunk", 4, "upload a file over this many MiB in parts of that size to an import session, committed as one import; 0 sends it in one request")
	return cmd
}

func readState(in io.Reader, file string) ([]byte, error) {
	if file == "-" {
		return io.ReadAll(in)
	}
	body, err := os.ReadFile(file)
	if err != nil {
		return nil, Rejected("cannot read "+file, "")
	}
	return body, nil
}

func stateLines(body []byte) []byte {
	var wrapped struct {
		Data      []json.RawMessage `json:"data"`
		RequestID *string           `json:"request_id"`
	}
	if json.Unmarshal(body, &wrapped) != nil || wrapped.RequestID == nil {
		return body
	}
	var out bytes.Buffer
	for _, line := range wrapped.Data {
		var compact bytes.Buffer
		if json.Compact(&compact, line) != nil {
			out.Write(line)
		} else {
			out.Write(compact.Bytes())
		}
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func stateSummary(manifest state.Manifest, file string) string {
	records, blobs := 0, 0
	for _, group := range manifest.Groups {
		records += group.Count
		blobs += group.Blobs
	}
	target := file
	if target == "" || target == "-" {
		target = "stdout"
	}
	line := fmt.Sprintf("exported %d records in %d groups", records, len(manifest.Groups))
	switch {
	case blobs == 1:
		line += " with 1 blob"
	case blobs > 1:
		line += fmt.Sprintf(" with %d blobs", blobs)
	}
	return line + " to " + target
}

func stateCounts(out state.ImportResponse) string {
	outcomes := make([]string, 0, len(out.Counts))
	for outcome := range out.Counts {
		outcomes = append(outcomes, outcome)
	}
	sort.Strings(outcomes)
	parts := make([]string, 0, len(outcomes))
	for _, outcome := range outcomes {
		parts = append(parts, fmt.Sprintf("%d %s", out.Counts[outcome], outcome))
	}
	line := out.Mode
	if out.DryRun {
		line += " dry run, nothing written"
	}
	if len(parts) == 0 {
		return line + ": nothing to do"
	}
	return line + ": " + strings.Join(parts, ", ")
}

func stateImportStaged(cmd *cobra.Command, send Send, in state.ImportRequest, lines []byte, size int) ([]byte, error) {
	started, err := send(cmd, http.MethodPost, "/state/import/session", nil, "application/json", []byte("{}"))
	if err != nil {
		return nil, err
	}
	var session struct {
		Data state.Session `json:"data"`
	}
	if err := json.Unmarshal(started, &session); err != nil || session.Data.ID == "" {
		return nil, &Error{Code: ExitFailure, Message: "the import session answer is not json", Cause: err}
	}
	path := "/state/import/session/" + session.Data.ID
	abandon := func(err error) ([]byte, error) {
		_, _ = send(cmd, http.MethodDelete, path, nil, "", nil)
		return nil, err
	}
	parts := stateParts(lines, size)
	for i, part := range parts {
		fmt.Fprintf(cmd.ErrOrStderr(), "upload part %d of %d, %d bytes\n", i+1, len(parts), len(part))
		if _, err := send(cmd, http.MethodPut, path+"/part/"+strconv.Itoa(i+1), nil, state.MIMENDJSON, part); err != nil {
			return abandon(err)
		}
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return abandon(err)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "commit %d parts\n", len(parts))
	body, err := send(cmd, http.MethodPost, path+"/commit", nil, "application/json", payload)
	if err != nil {
		return abandon(err)
	}
	return body, nil
}

func stateParts(lines []byte, size int) [][]byte {
	parts := make([][]byte, 0)
	var current []byte
	for len(lines) > 0 {
		end := bytes.IndexByte(lines, '\n')
		line := lines
		if end >= 0 {
			line = lines[:end+1]
		}
		lines = lines[len(line):]
		if len(current) > 0 && len(current)+len(line) > size {
			parts = append(parts, current)
			current = nil
		}
		current = append(current, line...)
	}
	if len(current) > 0 {
		parts = append(parts, current)
	}
	return parts
}

func stateGroupCounts(out state.ImportResponse) []string {
	byGroup := map[string]map[string]int{}
	groups := make([]string, 0)
	for _, result := range out.Results {
		if byGroup[result.Group] == nil {
			byGroup[result.Group] = map[string]int{}
			groups = append(groups, result.Group)
		}
		byGroup[result.Group][result.Outcome]++
	}
	lines := make([]string, 0, len(groups))
	for _, group := range groups {
		outcomes := make([]string, 0, len(byGroup[group]))
		for outcome := range byGroup[group] {
			outcomes = append(outcomes, outcome)
		}
		sort.Strings(outcomes)
		parts := make([]string, 0, len(outcomes))
		for _, outcome := range outcomes {
			parts = append(parts, fmt.Sprintf("%d %s", byGroup[group][outcome], outcome))
		}
		lines = append(lines, group+": "+strings.Join(parts, ", "))
	}
	return lines
}
