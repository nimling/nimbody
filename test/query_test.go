package test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody/query"
	"github.com/stretchr/testify/assert"
)

func queryContext(target string) echo.Context {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec)
}

func TestValidateQueryParamsRejectsUnknown(t *testing.T) {
	ctx := queryContext("/?bogus=1")
	_, err := query.ValidateQueryParams(ctx, false, "with")
	assert.ErrorContains(t, err, "unsupported query parameters: bogus")
}

func TestValidateQueryParamsAllowsPaginationKnobs(t *testing.T) {
	ctx := queryContext("/?limit=5&page=2&order=ASC&column=id&cursor=x")
	params, err := query.ValidateQueryParams(ctx, true, "with")
	assert.NoError(t, err)
	assert.False(t, params.Has("limit"))
}

func TestValidateQueryParamsAliasGroup(t *testing.T) {
	ctx := queryContext("/?organization_id=1")
	params, err := query.ValidateQueryParams(ctx, false, "org_id|organization_id")
	assert.NoError(t, err)
	assert.Equal(t, "1", params.Get("org_id"))
}

func TestValidateQueryParamsConflictingAliases(t *testing.T) {
	ctx := queryContext("/?org_id=1&organization_id=2")
	_, err := query.ValidateQueryParams(ctx, false, "org_id|organization_id")
	assert.ErrorContains(t, err, "conflicting query parameters")
}

func TestValidateQueryParamsNestedWithFilters(t *testing.T) {
	ctx := queryContext("/?with=contact@role:main")
	params, err := query.ValidateQueryParams(ctx, false, query.QueryParam{"with": {"role"}})
	assert.NoError(t, err)

	tokens, err := params.GetWith()
	assert.NoError(t, err)
	assert.Len(t, tokens, 1)
	assert.Equal(t, "contact", tokens[0].Name)
	assert.Equal(t, "main", tokens[0].Filters["role"])
}

func TestValidateQueryParamsRejectsUndeclaredWithFilter(t *testing.T) {
	ctx := queryContext("/?with=contact@bogus:1")
	_, err := query.ValidateQueryParams(ctx, false, query.QueryParam{"with": {"role"}})
	assert.ErrorContains(t, err, "unsupported filter on with: bogus")
}

func TestParseWithSplitsCommaSeparatedNames(t *testing.T) {
	tokens, err := query.ParseWith([]string{"role, image ,bio"})
	assert.NoError(t, err)
	assert.Len(t, tokens, 3)
	assert.Equal(t, "role", tokens[0].Name)
	assert.Equal(t, "image", tokens[1].Name)
	assert.Equal(t, "bio", tokens[2].Name)
}

func TestGetUUIDInvalid(t *testing.T) {
	ctx := queryContext("/?object_id=notauuid")
	params, err := query.ValidateQueryParams(ctx, false, "object_id")
	assert.NoError(t, err)
	_, err = params.GetUUID("object_id")
	assert.Error(t, err)
}

func TestGetTimeParamFormats(t *testing.T) {
	ctx := queryContext("/?from=2026-07-13")
	params, err := query.ValidateQueryParams(ctx, false, "from")
	assert.NoError(t, err)
	ts, err := params.GetTime("from")
	assert.NoError(t, err)
	assert.Equal(t, 2026, ts.Year())
	assert.Equal(t, 13, ts.Day())
}
