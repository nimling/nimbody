package test

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody/respond"
	"github.com/nimling/nimbody/stream"
	"github.com/stretchr/testify/assert"
)

func TestStreamReadFrames(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set(echo.HeaderAccept, respond.MIMEEventStream)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	steps := respond.GetSteps(ctx)
	steps.Start("a", "A", "")
	steps.Done("a", "x")
	steps.Start("b", "B", "a")
	assert.NoError(t, respond.JSON(ctx, http.StatusCreated, []string{"one"}, 1))

	var seen []respond.Step
	out, err := stream.Read(rec.Result(), func(step respond.Step) { seen = append(seen, step) })
	assert.NoError(t, err)
	assert.Len(t, seen, 3)
	assert.Equal(t, "done", seen[1].Status)
	assert.Equal(t, "a", seen[2].ParentID)
	assert.Equal(t, http.StatusCreated, out.Status)
	assert.Contains(t, string(out.Body), `"count":1`)
}

func TestStreamReadPlainJSON(t *testing.T) {
	res := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {echo.MIMEApplicationJSONCharsetUTF8}}, Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}
	out, err := stream.Read(res, nil)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, out.Status)
	assert.Equal(t, `{"data":[]}`, string(out.Body))
}

func TestStreamLineAndParse(t *testing.T) {
	line := `step {"id":"a","label":"A","status":"running","started_at":"2026-10-16T10:00:00Z"}`
	step, ok := stream.Parse(line)
	assert.True(t, ok)
	assert.Equal(t, "A", step.Label)
	assert.Equal(t, "A  running", stream.Line(step))
	_, ok = stream.Parse("plain stderr")
	assert.False(t, ok)
}

func TestStreamMachine(t *testing.T) {
	t.Setenv(stream.EnvMCP, "")
	assert.False(t, stream.Machine())
	t.Setenv(stream.EnvMCP, "1")
	assert.True(t, stream.Machine())
}

func TestLoginPageEscapesAndMarksFailure(t *testing.T) {
	ok, err := stream.GetLoginPage(stream.LoginPage{Name: "scon", Product: "Samna Connect", Title: "Signed in", Message: "Return to your terminal."})
	assert.NoError(t, err)
	assert.Contains(t, string(ok), "<h1>Signed in</h1>")
	assert.Contains(t, string(ok), `<span class="cli">scon</span>`)
	assert.Contains(t, string(ok), `<span class="brand-name">Samna Connect</span>`)
	assert.Contains(t, string(ok), `<span class="brand-mark" aria-hidden="true">S</span>`)
	assert.NotContains(t, string(ok), "card failed")

	failed, err := stream.GetLoginPage(stream.LoginPage{Name: "scon", Title: "Not signed in", Message: "<script>x</script>", Failed: true})
	assert.NoError(t, err)
	assert.Contains(t, string(failed), "card failed")
	assert.NotContains(t, string(failed), "<script>x</script>")
}

func frameServer(middleware ...echo.MiddlewareFunc) *echo.Echo {
	e := echo.New()
	e.POST("/big", func(ctx echo.Context) error {
		return respond.JSON(ctx, http.StatusOK, strings.Repeat("x", respond.FrameLimit+1), 1)
	}, middleware...)
	return e
}

func streamFrame(e *echo.Echo) *http.Response {
	req := httptest.NewRequest(http.MethodPost, "/big", nil)
	req.Header.Set(echo.HeaderAccept, respond.MIMEEventStream)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Result()
}

func TestStreamReadTakesTheFrameLimitTheEndpointDeclares(t *testing.T) {
	res := streamFrame(frameServer(respond.FrameLimitMiddleware(respond.FrameLimit * 2)))
	assert.Equal(t, strconv.Itoa(respond.FrameLimit*2), res.Header.Get(respond.HeaderFrameLimit))
	out, err := stream.Read(res, nil)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, out.Status)
	assert.Greater(t, len(out.Body), respond.FrameLimit)

	out, err = stream.Read(streamFrame(frameServer(respond.FrameLimitMiddleware(0))), nil)
	assert.NoError(t, err, "a limit of 0 is unbounded")
	assert.Greater(t, len(out.Body), respond.FrameLimit)
}

func TestStreamReadKeepsTheDefaultFrameLimit(t *testing.T) {
	res := streamFrame(frameServer())
	assert.Equal(t, strconv.Itoa(respond.FrameLimit), res.Header.Get(respond.HeaderFrameLimit))
	_, err := stream.Read(res, nil)
	assert.ErrorIs(t, err, bufio.ErrTooLong)

	res = streamFrame(frameServer())
	res.Header.Del(respond.HeaderFrameLimit)
	_, err = stream.Read(res, nil)
	assert.ErrorIs(t, err, bufio.ErrTooLong, "a stream that announces nothing keeps the default")
}
