package query

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

var paginationParams = map[string]bool{
	"column": true,
	"cursor": true,
	"limit":  true,
	"order":  true,
	"page":   true,
}

type QueryParams struct {
	Values url.Values
}

func (q *QueryParams) Has(name string) bool {
	return q.Values.Has(name)
}

func (q *QueryParams) Get(name string) string {
	return q.Values.Get(name)
}

func (q *QueryParams) GetTime(name string) (*time.Time, error) {
	return GetTimeParam(name, q.Values)
}

func (q *QueryParams) GetUUID(name string) (*uuid.UUID, error) {
	raw := q.Values.Get(name)
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func (q *QueryParams) GetSlice(name string) []string {
	var out []string
	for _, v := range q.Values[name] {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func (q *QueryParams) GetUUIDs(name string) ([]uuid.UUID, error) {
	parts := q.GetSlice(name)
	if len(parts) == 0 {
		return nil, nil
	}
	out := make([]uuid.UUID, 0, len(parts))
	for _, part := range parts {
		id, err := uuid.Parse(part)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

type WithToken struct {
	Name    string
	Filters map[string]string
}

func ParseWith(values []string) ([]WithToken, error) {
	var out []WithToken
	for _, v := range values {
		for _, raw := range strings.Split(v, ",") {
			seg := strings.TrimSpace(raw)
			if seg == "" {
				continue
			}
			parts := strings.Split(seg, "@")
			tok := WithToken{Name: strings.TrimSpace(parts[0])}
			if tok.Name == "" {
				continue
			}
			for _, p := range parts[1:] {
				key, value, ok := strings.Cut(p, ":")
				if !ok {
					return nil, fmt.Errorf("invalid filter on with=%s: %s", tok.Name, p)
				}
				if tok.Filters == nil {
					tok.Filters = make(map[string]string)
				}
				tok.Filters[strings.TrimSpace(key)] = value
			}
			out = append(out, tok)
		}
	}
	return out, nil
}

func (q *QueryParams) GetWith() ([]WithToken, error) {
	return ParseWith(q.Values["with"])
}

type QueryParam map[string][]string

func ValidateQueryParams(ctx echo.Context, paginated bool, allowed ...any) (*QueryParams, error) {
	canonical := make(url.Values)
	groupOf := make(map[string]string)
	nested := make(map[string]map[string]string)

	register := func(entry string) string {
		names := strings.Split(entry, "|")
		head := names[0]
		for _, name := range names {
			groupOf[name] = head
		}
		return head
	}

	for _, spec := range allowed {
		switch v := spec.(type) {
		case nil:
			continue
		case string:
			register(v)
		case QueryParam:
			for param, nestedKeys := range v {
				head := register(param)
				keys := make(map[string]string)
				for _, nk := range nestedKeys {
					names := strings.Split(nk, "|")
					nestedHead := names[0]
					for _, name := range names {
						keys[name] = nestedHead
					}
				}
				nested[head] = keys
			}
		default:
			return nil, fmt.Errorf("invalid query parameter spec: %T", spec)
		}
	}

	incoming := ctx.QueryParams()
	seenGroup := make(map[string]string)
	var unknown []string

	for key, values := range incoming {
		if paginated && paginationParams[key] {
			continue
		}
		group, ok := groupOf[key]
		if !ok {
			unknown = append(unknown, key)
			continue
		}
		if prev, exists := seenGroup[group]; exists && prev != key {
			return nil, fmt.Errorf("conflicting query parameters %q and %q", prev, key)
		}
		seenGroup[group] = key
		canonical[group] = values

		if keys, has := nested[group]; has {
			tokens, err := ParseWith(values)
			if err != nil {
				return nil, err
			}
			for _, tok := range tokens {
				for fk := range tok.Filters {
					if _, ok := keys[fk]; !ok {
						return nil, fmt.Errorf("unsupported filter on %s: %s", group, fk)
					}
				}
			}
		}
	}

	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unsupported query parameters: %s", strings.Join(unknown, ", "))
	}

	return &QueryParams{Values: canonical}, nil
}

var (
	timeLayouts = []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		time.RFC1123,
		time.RFC1123Z,
		time.RFC822,
		time.RFC822Z,
		"2006-01-02",
		"01/02/2006",
		"02/01/2006",
		"2006/01/02",
		"January 2, 2006",
		"2 January 2006",
		"Jan 2, 2006",
		"2 Jan 2006",
	}

	ErrNoTimeParam       = errors.New("time parameter not found")
	ErrInvalidTimeFormat = errors.New("invalid time format")
)

func GetTimeParam(name string, values url.Values) (*time.Time, error) {
	if !values.Has(name) {
		return nil, ErrNoTimeParam
	}

	timeStr := values.Get(name)
	if timeStr == "" {
		return nil, ErrNoTimeParam
	}

	if unixSec, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		if unixSec > 1000000000000 {
			t := time.UnixMilli(unixSec)
			return &t, nil
		}
		t := time.Unix(unixSec, 0)
		return &t, nil
	}

	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, timeStr); err == nil {
			return &t, nil
		}
	}

	return nil, ErrInvalidTimeFormat
}

func GetTimeParamInLocation(name string, values url.Values, loc *time.Location) (time.Time, error) {
	if !values.Has(name) {
		return time.Time{}, ErrNoTimeParam
	}

	timeStr := values.Get(name)
	if timeStr == "" {
		return time.Time{}, ErrNoTimeParam
	}

	if unixSec, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		if unixSec > 1000000000000 {
			return time.UnixMilli(unixSec), nil
		}
		return time.Unix(unixSec, 0), nil
	}

	for _, layout := range timeLayouts {
		if t, err := time.ParseInLocation(layout, timeStr, loc); err == nil {
			return t, nil
		}
	}

	return time.Time{}, ErrInvalidTimeFormat
}
