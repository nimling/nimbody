package stream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nimling/nimbody/respond"
)

const EnvMCP = "NIMBODY_MCP"

const linePrefix = "step "

type Result struct {
	Status int
	Body   []byte
}

type resultFrame struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func Read(res *http.Response, onStep func(respond.Step)) (Result, error) {
	if !strings.HasPrefix(res.Header.Get("Content-Type"), respond.MIMEEventStream) {
		body, err := io.ReadAll(res.Body)
		return Result{Status: res.StatusCode, Body: body}, err
	}

	out := Result{Status: res.StatusCode}
	var event string
	var data bytes.Buffer
	flush := func() error {
		defer func() {
			event = ""
			data.Reset()
		}()
		if data.Len() == 0 {
			return nil
		}
		switch event {
		case "step":
			var step respond.Step
			if err := json.Unmarshal(data.Bytes(), &step); err != nil {
				return err
			}
			if onStep != nil {
				onStep(step)
			}
		case "result":
			var frame resultFrame
			if err := json.Unmarshal(data.Bytes(), &frame); err != nil {
				return err
			}
			out.Status = frame.Status
			out.Body = frame.Body
		}
		return nil
	}

	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<24)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return out, err
			}
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	return out, flush()
}

func Machine() bool {
	return os.Getenv(EnvMCP) != ""
}

func Report(w io.Writer) func(respond.Step) {
	machine := Machine()
	return func(step respond.Step) {
		if machine {
			line, _ := json.Marshal(step)
			fmt.Fprintf(w, "%s%s\n", linePrefix, line)
			return
		}
		fmt.Fprintln(w, Line(step))
	}
}

func Line(step respond.Step) string {
	switch step.Status {
	case "done":
		return fmt.Sprintf("%s  done %s", step.Label, step.EndedAt.Sub(step.StartedAt).Round(time.Millisecond))
	case "failed":
		return fmt.Sprintf("%s  failed: %s", step.Label, step.Error)
	default:
		return step.Label + "  running"
	}
}

func Parse(line string) (respond.Step, bool) {
	if !strings.HasPrefix(line, linePrefix) {
		return respond.Step{}, false
	}
	var step respond.Step
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, linePrefix)), &step); err != nil {
		return respond.Step{}, false
	}
	return step, true
}
