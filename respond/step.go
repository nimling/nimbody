package respond

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

const MIMEEventStream = "text/event-stream"

type Step struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	ParentID  string     `json:"parent_id,omitempty"`
	DependsOn []string   `json:"depends_on,omitempty"`
	Status    string     `json:"status"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Data      any        `json:"data,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type ReturnResult struct {
	Status int          `json:"status"`
	Body   ReturnObject `json:"body"`
}

type Steps struct {
	ctx    echo.Context
	stream bool
	mu     sync.Mutex
	rows   map[string]*Step
}

type stepsKey struct{}

func Streaming(ctx echo.Context) bool {
	return strings.Contains(ctx.Request().Header.Get(echo.HeaderAccept), MIMEEventStream)
}

func GetSteps(ctx echo.Context) *Steps {
	if s, ok := ctx.Get("steps").(*Steps); ok {
		return s
	}
	s := &Steps{ctx: ctx, stream: Streaming(ctx), rows: map[string]*Step{}}
	ctx.Set("steps", s)
	req := ctx.Request()
	ctx.SetRequest(req.WithContext(context.WithValue(req.Context(), stepsKey{}, s)))
	return s
}

func StepsFrom(ctx context.Context) *Steps {
	s, _ := ctx.Value(stepsKey{}).(*Steps)
	return s
}

func (s *Steps) Start(id, label, parentID string, dependsOn ...string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row := &Step{ID: id, Label: label, ParentID: parentID, DependsOn: dependsOn, Status: "running", StartedAt: time.Now().UTC()}
	s.rows[id] = row
	if s.stream {
		writeEvent(s.ctx, "step", row)
	}
}

func (s *Steps) Done(id string, data any) {
	s.end(id, "done", data, "")
}

func (s *Steps) Fail(id string, err error) {
	s.end(id, "failed", nil, err.Error())
}

func (s *Steps) end(id, status string, data any, message string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok {
		return
	}
	now := time.Now().UTC()
	row.Status = status
	row.EndedAt = &now
	row.Data = data
	row.Error = message
	if s.stream {
		writeEvent(s.ctx, "step", row)
	}
}

func writeEvent(ctx echo.Context, event string, data any) error {
	res := ctx.Response()
	if !res.Committed {
		res.Header().Set(echo.HeaderContentType, MIMEEventStream)
		res.Header().Set("Cache-Control", "no-cache")
		res.Header().Set("Connection", "keep-alive")
		res.Header().Set("X-Accel-Buffering", "no")
		res.WriteHeader(http.StatusOK)
	}
	body, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(res, "event: %s\ndata: %s\n\n", event, body); err != nil {
		return err
	}
	res.Flush()
	return nil
}

func write(ctx echo.Context, statusCode int, returnData ReturnObject) error {
	if Streaming(ctx) {
		return writeEvent(ctx, "result", ReturnResult{Status: statusCode, Body: returnData})
	}
	ctx.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSONCharsetUTF8)
	ctx.Response().WriteHeader(statusCode)
	enc := json.NewEncoder(ctx.Response())
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(returnData)
}
