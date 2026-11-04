package nimbody

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/labstack/echo/v4"
)

type JSONBody map[string]interface{}

type BodyValidator struct {
	Body JSONBody
}

func (v *BodyValidator) HasKey(key string) bool {
	_, ok := v.Body[key]
	return ok
}

func (v *BodyValidator) HasValue(key string) bool {
	return v.Body[key] != nil
}

func (v *BodyValidator) IsEmpty() bool {
	return len(v.Body) == 0
}

func (v *BodyValidator) Deny(fields ...string) []string {
	var denied []string
	for key := range v.Body {
		for _, pattern := range fields {
			if matchDenyPattern(pattern, key) {
				denied = append(denied, key)
				break
			}
		}
	}
	sort.Strings(denied)
	return denied
}

func matchDenyPattern(pattern, key string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(key, pattern[:len(pattern)-1])
	}
	return pattern == key
}

func (v *BodyValidator) ForObject(key string) *BodyValidator {
	value, ok := v.Body[key]
	if !ok {
		return nil
	}
	switch nested := value.(type) {
	case JSONBody:
		return &BodyValidator{Body: nested}
	case map[string]interface{}:
		return &BodyValidator{Body: JSONBody(nested)}
	}
	return nil
}

func Validator(ctx echo.Context) (*BodyValidator, error) {
	var buf bytes.Buffer
	tee := io.TeeReader(ctx.Request().Body, &buf)

	var body JSONBody
	if err := json.NewDecoder(tee).Decode(&body); err != nil {
		return nil, err
	}
	ctx.Request().Body = io.NopCloser(&buf)

	return &BodyValidator{Body: body}, nil
}

func Bind(ctx echo.Context, i interface{}) (*BodyValidator, error) {
	bv, err := Validator(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Bind(i); err != nil {
		return nil, err
	}
	return bv, nil
}

type Body struct {
	bv     *BodyValidator
	target any
}

func (b *Body) Bound(ctx echo.Context, target any) error {
	bv, err := Bind(ctx, target)
	if err != nil {
		return err
	}
	b.bv = bv
	b.target = target
	return nil
}

func (b *Body) Validator() *BodyValidator { return b.bv }

func (b *Body) HasKey(key string) bool {
	return b.bv != nil && b.bv.HasKey(key)
}

func (b *Body) HasValue(key string) bool {
	return b.bv != nil && b.bv.HasValue(key)
}

func (b *Body) IsEmpty() bool {
	return b.bv == nil || b.bv.IsEmpty()
}

func (b *Body) GetUnknown() []string {
	if b.bv == nil || b.target == nil {
		return nil
	}
	allowed := jsonFieldNames(b.target)
	var unknown []string
	for key := range b.bv.Body {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	return unknown
}

func (b *Body) Deny(fields ...string) []string {
	if b.bv == nil {
		return nil
	}
	return b.bv.Deny(fields...)
}

func jsonFieldNames(target any) map[string]struct{} {
	out := map[string]struct{}{}
	rv := reflect.ValueOf(target)
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return out
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		if comma := strings.IndexByte(tag, ','); comma >= 0 {
			tag = tag[:comma]
		}
		if tag == "" || tag == "-" {
			continue
		}
		out[tag] = struct{}{}
	}
	return out
}
