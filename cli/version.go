package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/nimling/nimbody/stream"
	"github.com/spf13/cobra"
)

var Version = "dev"

func GetVersion() string {
	if Version != "" && Version != "dev" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

type Versions struct {
	Version string `json:"version"`
	Server  string `json:"server,omitempty"`
}

func VersionCommand(server func(ctx context.Context) (string, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "Print the build version, and the version the server runs",
		Example: "version --json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			versions := Versions{Version: GetVersion()}
			if server != nil {
				running, err := server(cmd.Context())
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), StyleFor(cmd.ErrOrStderr()).Warn("the server version is unknown: "+err.Error()))
				}
				versions.Server = running
			}
			return PrintValue(cmd.OutOrStdout(), versions, FormatFor(cmd.OutOrStdout(), wantsJSON(cmd)))
		},
	}
}

type Notifier struct {
	Settings *Settings
	Latest   func(ctx context.Context) (string, error)
	Hint     string
	Every    time.Duration
	checked  chan string
}

type updateCache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

func (n *Notifier) Off() bool {
	return os.Getenv(n.Settings.Prefix+"NO_UPDATE_NOTIFIER") != "" || stream.Machine() || GetVersion() == "dev"
}

func (n *Notifier) cachePath() (string, error) {
	dir, err := n.Settings.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "update.json"), nil
}

func (n *Notifier) Check(ctx context.Context) (string, error) {
	if n.Off() || n.Latest == nil {
		return "", nil
	}
	path, err := n.cachePath()
	if err != nil {
		return "", err
	}
	every := n.Every
	if every == 0 {
		every = 24 * time.Hour
	}
	var cache updateCache
	if body, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(body, &cache)
	}
	if time.Since(cache.CheckedAt) >= every {
		latest, err := n.Latest(ctx)
		if err != nil {
			return "", err
		}
		cache = updateCache{CheckedAt: time.Now().UTC(), Latest: latest}
		body, _ := json.Marshal(cache)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return "", err
		}
	}
	if !Newer(cache.Latest, GetVersion()) {
		return "", nil
	}
	note := "a newer " + n.Settings.Name + " is out, " + cache.Latest + ", this is " + GetVersion() + "."
	if n.Hint != "" {
		note += " " + n.Hint
	}
	return note + " Set " + n.Settings.Prefix + "NO_UPDATE_NOTIFIER=1 to stop this note.", nil
}

func (n *Notifier) Mount(root *cobra.Command) {
	before, after := root.PersistentPreRunE, root.PersistentPostRunE
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if before != nil {
			if err := before(cmd, args); err != nil {
				return err
			}
		}
		if !n.Off() && Terminal(cmd.ErrOrStderr()) {
			n.checked = make(chan string, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				note, _ := n.Check(ctx)
				n.checked <- note
			}()
		}
		return nil
	}
	root.PersistentPostRunE = func(cmd *cobra.Command, args []string) error {
		if after != nil {
			if err := after(cmd, args); err != nil {
				return err
			}
		}
		if n.checked == nil {
			return nil
		}
		select {
		case note := <-n.checked:
			if note != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), StyleFor(cmd.ErrOrStderr()).Dim(note))
			}
		case <-time.After(2 * time.Second):
		}
		return nil
	}
}

func Newer(candidate, current string) bool {
	have, haveOK := semver(current)
	want, wantOK := semver(candidate)
	if !haveOK || !wantOK {
		return false
	}
	for index := range 3 {
		if want[index] != have[index] {
			return want[index] > have[index]
		}
	}
	return want[3] > have[3]
}

func semver(version string) ([4]int, bool) {
	var parts [4]int
	core, pre, hasPre := strings.Cut(strings.TrimPrefix(strings.TrimSpace(version), "v"), "-")
	fields := strings.Split(strings.SplitN(core, "+", 2)[0], ".")
	if len(fields) != 3 {
		return parts, false
	}
	for index, field := range fields {
		number, err := strconv.Atoi(field)
		if err != nil {
			return parts, false
		}
		parts[index] = number
	}
	if !hasPre || pre == "" {
		parts[3] = 1
	}
	return parts, true
}
