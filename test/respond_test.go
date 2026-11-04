package test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody/respond"
	"github.com/stretchr/testify/assert"
)

func respondContext(target string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func TestJSONEnvelope(t *testing.T) {
	ctx, rec := respondContext("/")
	err := respond.JSON(ctx, http.StatusOK, []string{"a", "b"}, 2)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, float64(2), body["count"])
	assert.NotEmpty(t, body["request_id"])
	assert.Len(t, body["data"], 2)
}

func TestErrorEnvelope(t *testing.T) {
	ctx, rec := respondContext("/")
	err := respond.Error(ctx, http.StatusBadRequest, "Malformed request")
	assert.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var body map[string]any
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errObj := body["error"].(map[string]any)
	assert.Equal(t, "Malformed request", errObj["message"])
}

func TestRequestIDMiddlewareSetsID(t *testing.T) {
	ctx, _ := respondContext("/")
	handler := respond.RequestIDMiddleware()(func(c echo.Context) error {
		assert.NotNil(t, c.Get("request_id"))
		return nil
	})
	assert.NoError(t, handler(ctx))
}

func TestInitPaginationDefaults(t *testing.T) {
	ctx, _ := respondContext("/")
	assert.NoError(t, respond.InitPagination(ctx, nil, nil))
	p := respond.GetPagination(ctx)
	assert.Equal(t, 1, p.Page)
	assert.Equal(t, 20, p.Limit)
	assert.Equal(t, "DESC", p.Order)
	assert.Equal(t, "id", p.Column)
}

func TestInitPaginationRejectsInvalidOrder(t *testing.T) {
	ctx, _ := respondContext("/?order=sideways")
	assert.ErrorContains(t, respond.InitPagination(ctx, nil, nil), "order invalid")
}

func TestInitPaginationNormalizesLowercaseOrder(t *testing.T) {
	ctx, _ := respondContext("/?order=asc")
	assert.NoError(t, respond.InitPagination(ctx, nil, nil))
	assert.Equal(t, "ASC", respond.GetPagination(ctx).Order)
}

func TestValidateSortColumnAllowlist(t *testing.T) {
	ctx, _ := respondContext("/?column=name")
	assert.NoError(t, respond.InitPagination(ctx, nil, nil))
	assert.NoError(t, respond.ValidateSortColumn(ctx, []string{"name"}))
	assert.ErrorContains(t, respond.ValidateSortColumn(ctx, []string{"email"}), "column not allowed")
}

func TestPaginationOffset(t *testing.T) {
	ctx, _ := respondContext("/?page=3&limit=10")
	assert.NoError(t, respond.InitPagination(ctx, nil, nil))
	assert.Equal(t, 20, respond.GetPagination(ctx).Offset())
}

func TestFilterToScope(t *testing.T) {
	scope := json.RawMessage(`{"name": {}}`)
	out, err := respond.FilterToScope(map[string]any{"name": "a", "secret": "b"}, scope)
	assert.NoError(t, err)
	filtered := out.(map[string]any)
	assert.Equal(t, "a", filtered["name"])
	assert.NotContains(t, filtered, "secret")
}

func TestRejectOutOfScope(t *testing.T) {
	scope := json.RawMessage(`{"name": {}}`)
	assert.NoError(t, respond.RejectOutOfScope(map[string]any{"name": "a"}, scope))
	assert.ErrorContains(t, respond.RejectOutOfScope(map[string]any{"secret": "b"}, scope), "not in scope")
}

func TestStepsStreamFramesAndResult(t *testing.T) {
	ctx, rec := respondContext("/")
	ctx.Request().Header.Set(echo.HeaderAccept, respond.MIMEEventStream)
	steps := respond.GetSteps(ctx)
	steps.Start("validate", "Validate", "")
	steps.Done("validate", nil)
	steps.Start("write", "Write", "", "validate")
	steps.Fail("write", errors.New("boom"))
	assert.NoError(t, respond.JSON(ctx, http.StatusCreated, map[string]string{"id": "x"}, 1))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, respond.MIMEEventStream, rec.Header().Get(echo.HeaderContentType))
	frames := strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n")
	assert.Len(t, frames, 5)
	assert.True(t, strings.HasPrefix(frames[0], "event: step\ndata: "))
	assert.Contains(t, frames[0], `"status":"running"`)
	assert.Contains(t, frames[1], `"status":"done"`)
	assert.Contains(t, frames[2], `"depends_on":["validate"]`)
	assert.Contains(t, frames[3], `"status":"failed"`)
	assert.Contains(t, frames[3], `"error":"boom"`)
	assert.True(t, strings.HasPrefix(frames[4], "event: result\ndata: "))

	var result map[string]any
	assert.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(frames[4], "event: result\ndata: ")), &result))
	assert.Equal(t, float64(http.StatusCreated), result["status"])
	body := result["body"].(map[string]any)
	assert.Equal(t, float64(1), body["count"])
	assert.NotEmpty(t, body["request_id"])
}

func TestStepsWithoutAcceptKeepJSON(t *testing.T) {
	ctx, rec := respondContext("/")
	steps := respond.GetSteps(ctx)
	steps.Start("validate", "Validate", "")
	steps.Done("validate", nil)
	assert.NoError(t, respond.JSON(ctx, http.StatusOK, []string{"a"}, 1))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, echo.MIMEApplicationJSONCharsetUTF8, rec.Header().Get(echo.HeaderContentType))

	var body map[string]any
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, float64(1), body["count"])
	assert.Nil(t, body["status"])
}

func TestErrorStreamsResultFrame(t *testing.T) {
	ctx, rec := respondContext("/")
	ctx.Request().Header.Set(echo.HeaderAccept, respond.MIMEEventStream)
	assert.NoError(t, respond.Error(ctx, http.StatusBadRequest, "Malformed request"))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "event: result\ndata: ")
	assert.Contains(t, rec.Body.String(), `"status":400`)
	assert.Contains(t, rec.Body.String(), `"message":"Malformed request"`)
}

func TestStepsFromRequestContext(t *testing.T) {
	ctx, _ := respondContext("/")
	assert.Nil(t, respond.StepsFrom(ctx.Request().Context()))
	steps := respond.GetSteps(ctx)
	assert.Same(t, steps, respond.StepsFrom(ctx.Request().Context()))
	assert.Same(t, steps, respond.GetSteps(ctx))
	var absent *respond.Steps
	absent.Start("x", "X", "")
	absent.Done("x", nil)
}

func TestErrorEnvelopeUnchanged(t *testing.T) {
	ctx, rec := respondContext("/")
	ctx.Set("request_id", uuid.MustParse("00000000-0000-0000-0000-000000000001"))
	assert.NoError(t, respond.Error(ctx, http.StatusBadRequest, "Malformed request"))
	assert.Equal(t, "{\n  \"data\": [],\n  \"error\": {\n    \"message\": \"Malformed request\"\n  },\n  \"request_id\": \"00000000-0000-0000-0000-000000000001\",\n  \"count\": 0\n}\n", rec.Body.String())
}

func TestJSONEnvelopeUnchanged(t *testing.T) {
	ctx, rec := respondContext("/")
	ctx.Set("request_id", uuid.MustParse("00000000-0000-0000-0000-000000000001"))
	assert.NoError(t, respond.JSON(ctx, http.StatusConflict, []string{"a"}, 0))
	assert.Equal(t, "{\n  \"data\": [\n    \"a\"\n  ],\n  \"request_id\": \"00000000-0000-0000-0000-000000000001\",\n  \"count\": 0\n}\n", rec.Body.String())
}

func TestErrorEnvelopeWithData(t *testing.T) {
	ctx, rec := respondContext("/")
	assert.NoError(t, respond.Error(ctx, http.StatusConflict, "Outside of timeslots", []map[string]string{{"id": "a"}, {"id": "b"}}))
	assert.Equal(t, http.StatusConflict, rec.Code)

	var body map[string]any
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "Outside of timeslots", body["error"].(map[string]any)["message"])
	assert.Len(t, body["data"], 2)
	assert.Equal(t, float64(2), body["count"])
	assert.NotEmpty(t, body["request_id"])
}

func TestErrorEnvelopeWithObject(t *testing.T) {
	ctx, rec := respondContext("/")
	assert.NoError(t, respond.Error(ctx, http.StatusConflict, "Overlaps a booking", map[string]string{"id": "a"}))

	var body map[string]any
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "a", body["data"].(map[string]any)["id"])
	assert.Equal(t, float64(1), body["count"])
}

func TestErrorWithDataStreamsResultFrame(t *testing.T) {
	ctx, rec := respondContext("/")
	ctx.Request().Header.Set(echo.HeaderAccept, respond.MIMEEventStream)
	assert.NoError(t, respond.Error(ctx, http.StatusConflict, "Overlaps a booking", []string{"a"}))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, respond.MIMEEventStream, rec.Header().Get(echo.HeaderContentType))
	frame := strings.TrimSpace(rec.Body.String())
	assert.True(t, strings.HasPrefix(frame, "event: result\ndata: "))

	var result map[string]any
	assert.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(frame, "event: result\ndata: ")), &result))
	assert.Equal(t, float64(http.StatusConflict), result["status"])
	body := result["body"].(map[string]any)
	assert.Equal(t, []any{"a"}, body["data"])
	assert.Equal(t, float64(1), body["count"])
	assert.Equal(t, "Overlaps a booking", body["error"].(map[string]any)["message"])
}
