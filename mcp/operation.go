package mcp

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody/respond"
	"github.com/nimling/nimbody/shape"
)

const (
	InPath   = "path"
	InQuery  = "query"
	InHeader = "header"
	InBody   = "body"
)

type Spec struct {
	Name        string
	Description string
	Method      string
	Path        string
	Paths       []string
	Status      int
	Invalid     string
	Access      string
	Page        string
	Example     string
	Middleware  []echo.MiddlewareFunc
}

type Input struct {
	Name     string
	Param    string
	In       string
	Kind     string
	Usage    string
	Required bool
	index    []int
}

type Operation struct {
	Spec
	Inputs       []Input
	InputSchema  map[string]any
	OutputSchema map[string]any
	Route        echo.HandlerFunc
	call         func(ctx echo.Context, arguments json.RawMessage) (any, int, error)
}

type List[T any] struct {
	Rows  []T
	Total int
}

func (l List[T]) rows() (any, int) {
	if l.Rows == nil {
		return []T{}, l.Total
	}
	return l.Rows, l.Total
}

type lister interface {
	rows() (any, int)
}

type bounder interface {
	Bound(ctx echo.Context, target any) error
}

var (
	marshaler     = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
	timeType      = reflect.TypeFor[time.Time]()
	rawType       = reflect.TypeFor[json.RawMessage]()
	reserved      = []string{"limit", "page", "order", "column", "cursor"}
)

func Define[In any, Out any](spec Spec, service func(ctx echo.Context, in In) (Out, error)) Operation {
	run := func(ctx echo.Context, in In) (any, int, error) {
		out, err := service(ctx, in)
		if err != nil {
			return nil, 0, err
		}
		if listed, ok := any(out).(lister); ok {
			rows, total := listed.rows()
			return rows, total, nil
		}
		value := reflect.ValueOf(out)
		if value.Kind() == reflect.Slice || value.Kind() == reflect.Array {
			return out, value.Len(), nil
		}
		return out, 1, nil
	}
	status := spec.Status
	switch {
	case status != 0:
	case spec.Method == http.MethodPost:
		status = http.StatusCreated
	default:
		status = http.StatusOK
	}
	inputs := inputsOf(reflect.TypeFor[In](), nil)
	for i, input := range inputs {
		if input.In != InPath {
			continue
		}
		inputs[i].Required = true
		for _, path := range spec.paths() {
			if !strings.Contains(path+"/", "/:"+input.Param+"/") {
				inputs[i].Required = false
			}
		}
	}
	operation := Operation{
		Spec:         spec,
		Inputs:       inputs,
		InputSchema:  inputSchema(inputs, reflect.TypeFor[In]()),
		OutputSchema: GetOutputSchema(reflect.TypeFor[Out]()),
		Route: func(ctx echo.Context) error {
			var in In
			if err := bind(ctx, &in, inputs); err != nil {
				return refuse(ctx, spec.Name, invalid(spec, err))
			}
			value, count, err := run(ctx, in)
			switch {
			case err != nil:
				return refuse(ctx, spec.Name, err)
			case status == http.StatusNoContent && !respond.Streaming(ctx):
				return ctx.NoContent(status)
			case status == http.StatusNoContent:
				return respond.NoContent(ctx, status)
			}
			return respond.JSON(ctx, status, value, count)
		},
		call: func(ctx echo.Context, arguments json.RawMessage) (any, int, error) {
			var in In
			if len(arguments) > 0 {
				if err := json.Unmarshal(arguments, &in); err != nil {
					return nil, 0, invalid(spec, err)
				}
			}
			if input, found := undecoded(inputs); found {
				reflect.ValueOf(&in).Elem().FieldByIndex(input.index).SetZero()
			} else if err := recorded(ctx, &in, inputs, arguments); err != nil {
				return nil, 0, invalid(spec, err)
			}
			return run(ctx, in)
		},
	}
	return operation
}

func Route(group *echo.Group, operations ...Operation) {
	for _, operation := range operations {
		for _, path := range operation.paths() {
			group.Add(operation.Method, path, operation.Route, operation.Middleware...)
		}
	}
}

func (s Spec) paths() []string {
	return append([]string{s.Path}, s.Paths...)
}

func invalid(spec Spec, err error) error {
	if spec.Invalid != "" {
		return echo.NewHTTPError(http.StatusBadRequest, spec.Invalid)
	}
	return echo.NewHTTPError(http.StatusBadRequest, message(err))
}

func refuse(ctx echo.Context, name string, err error) error {
	var rejection *respond.Rejection
	var rejected *echo.HTTPError
	switch {
	case errors.As(err, &rejection) && rejection.Data != nil:
		return respond.Error(ctx, rejection.Status, rejection.Message, rejection.Data)
	case errors.As(err, &rejection):
		return respond.Error(ctx, rejection.Status, rejection.Message)
	case errors.As(err, &rejected):
		return respond.Error(ctx, rejected.Code, message(rejected))
	}
	ctx.Logger().Error(name + ": " + err.Error())
	return respond.ErrorInternal(ctx)
}

func bind(ctx echo.Context, in any, inputs []Input) error {
	binder := &echo.DefaultBinder{}
	_, raw := undecoded(inputs)
	body, bound := in.(bounder)
	switch {
	case raw:
	case bound && ctx.Request().ContentLength != 0:
		if err := body.Bound(ctx, in); err != nil {
			return err
		}
	default:
		if err := binder.BindBody(ctx, in); err != nil {
			return err
		}
	}
	switch ctx.Request().Method {
	case http.MethodGet, http.MethodDelete, http.MethodHead:
		if err := binder.BindQueryParams(ctx, in); err != nil {
			return err
		}
	default:
		if declares(inputs, InQuery) {
			if err := binder.BindQueryParams(ctx, in); err != nil {
				return err
			}
		}
	}
	if declares(inputs, InHeader) {
		if err := binder.BindHeaders(ctx, in); err != nil {
			return err
		}
	}
	if err := binder.BindPathParams(ctx, in); err != nil {
		return err
	}
	value := reflect.ValueOf(in).Elem()
	if value.Kind() != reflect.Struct {
		return nil
	}
	for _, input := range inputs {
		prefix, open := strings.CutSuffix(input.Param, "*")
		if input.In != InQuery || !open {
			continue
		}
		field := value.FieldByIndex(input.index)
		if field.Kind() != reflect.Map || field.Type().Key().Kind() != reflect.String {
			continue
		}
		entries := opened(ctx.QueryParams(), prefix, inputs)
		if len(entries) == 0 {
			continue
		}
		key, elem := field.Type().Key(), field.Type().Elem()
		filled := reflect.MakeMap(field.Type())
		for name, values := range entries {
			var entry reflect.Value
			switch {
			case elem.Kind() == reflect.String:
				entry = reflect.ValueOf(values[0]).Convert(elem)
			case elem.Kind() == reflect.Slice && elem.Elem().Kind() == reflect.String:
				entry = reflect.ValueOf(values).Convert(elem)
			case elem.Kind() == reflect.Interface && len(values) == 1:
				entry = reflect.ValueOf(values[0])
			case elem.Kind() == reflect.Interface:
				entry = reflect.ValueOf(values)
			default:
				continue
			}
			filled.SetMapIndex(reflect.ValueOf(name).Convert(key), entry)
		}
		field.Set(filled)
	}
	return nil
}

func declares(inputs []Input, in string) bool {
	for _, input := range inputs {
		if input.In == in {
			return true
		}
	}
	return false
}

func undecoded(inputs []Input) (Input, bool) {
	for _, input := range inputs {
		if input.In == InBody && input.Param == "*" {
			return input, true
		}
	}
	return Input{}, false
}

func recorded(ctx echo.Context, in any, inputs []Input, arguments json.RawMessage) error {
	body, bound := in.(bounder)
	if !bound {
		return nil
	}
	fields := map[string]json.RawMessage{}
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &fields); err != nil {
			return err
		}
	}
	consumed := map[string]bool{"limit": true, "cursor": true}
	for name := range shape.Schema() {
		consumed[name] = true
	}
	for _, input := range inputs {
		consumed[input.Name] = input.In != InBody
	}
	for name := range fields {
		if consumed[name] {
			delete(fields, name)
		}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx.Request().Context(), http.MethodPost, ctx.Request().URL.Path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	return body.Bound(ctx.Echo().NewContext(req, ctx.Response()), in)
}

func opened(query url.Values, prefix string, inputs []Input) url.Values {
	entries := url.Values{}
	for key, values := range query {
		name, found := strings.CutPrefix(key, prefix)
		if !found || name == "" || (prefix == "" && taken(key, inputs)) {
			continue
		}
		entries[name] = values
	}
	return entries
}

func taken(key string, inputs []Input) bool {
	for _, name := range reserved {
		if key == name {
			return true
		}
	}
	for _, input := range inputs {
		if input.In != InQuery {
			continue
		}
		if prefix, open := strings.CutSuffix(input.Param, "*"); open {
			if prefix != "" && strings.HasPrefix(key, prefix) {
				return true
			}
			continue
		}
		if key == input.Param {
			return true
		}
	}
	return false
}

func message(err error) string {
	var rejected *echo.HTTPError
	if errors.As(err, &rejected) {
		if text, ok := rejected.Message.(string); ok {
			return text
		}
		return fmt.Sprint(rejected.Message)
	}
	return err.Error()
}

func (o Operation) Tool() Tool {
	tool := Annotate(Tool{
		Name:         o.Name,
		Description:  Describe(o.Description, o.Example),
		InputSchema:  o.InputSchema,
		OutputSchema: o.OutputSchema,
		Page:         o.Page,
	}, o.Access)
	return tool
}

func inputsOf(t reflect.Type, index []int) []Input {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var inputs []Input
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, omit, skip := jsonName(field)
		if skip {
			continue
		}
		at := append(append([]int{}, index...), i)
		if field.Anonymous && field.Tag.Get("json") == "" && field.Type.Kind() == reflect.Struct {
			inputs = append(inputs, inputsOf(field.Type, at)...)
			continue
		}
		input := Input{Name: name, Param: name, In: InBody, Kind: kindOf(field.Type), Usage: field.Tag.Get("jsonschema"), index: at}
		switch {
		case field.Tag.Get("param") != "":
			input.In, input.Param = InPath, field.Tag.Get("param")
		case field.Tag.Get("query") != "":
			input.In, input.Param = InQuery, field.Tag.Get("query")
		case field.Tag.Get("header") != "":
			input.In, input.Param = InHeader, field.Tag.Get("header")
		case field.Tag.Get("body") == "*" && field.Type == rawType:
			input.Param = "*"
			input.Required = !omit
		default:
			input.Required = !omit && field.Type.Kind() != reflect.Pointer
		}
		inputs = append(inputs, input)
	}
	return inputs
}

func jsonName(field reflect.StructField) (string, bool, bool) {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	name, options, _ := strings.Cut(tag, ",")
	if name == "" {
		name = field.Name
	}
	omit := strings.Contains(options, "omitempty") || strings.Contains(options, "omitzero")
	return name, omit, false
}

func kindOf(t reflect.Type) string {
	kind, _ := schemaOf(t, map[reflect.Type]bool{})["type"].(string)
	return kind
}

func inputSchema(inputs []Input, t reflect.Type) map[string]any {
	properties := map[string]any{}
	var required []string
	fields := fieldTypes(t)
	for _, input := range inputs {
		property := schemaOf(fields[input.Name], map[reflect.Type]bool{})
		if input.Usage != "" {
			property["description"] = input.Usage
		}
		properties[input.Name] = property
		if input.Required {
			required = append(required, input.Name)
		}
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func fieldTypes(t reflect.Type) map[string]reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	types := map[string]reflect.Type{}
	if t.Kind() != reflect.Struct {
		return types
	}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		name, _, skip := jsonName(field)
		if skip || !field.IsExported() {
			continue
		}
		if field.Anonymous && field.Tag.Get("json") == "" && field.Type.Kind() == reflect.Struct {
			for key, value := range fieldTypes(field.Type) {
				types[key] = value
			}
			continue
		}
		types[name] = field.Type
	}
	return types
}

func GetOutputSchema(t reflect.Type) map[string]any {
	data := schemaOf(t, map[reflect.Type]bool{})
	if t.Implements(reflect.TypeFor[lister]()) {
		rows, _ := reflect.Zero(t).Interface().(lister).rows()
		data = schemaOf(reflect.TypeOf(rows), map[reflect.Type]bool{})
	}
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"data": data},
		"required":   []string{"data"},
	}
}

func nullable(kind string) []string {
	return []string{kind, "null"}
}

func schemaOf(t reflect.Type, seen map[reflect.Type]bool) map[string]any {
	if t == nil || t == rawType {
		return map[string]any{}
	}
	if t.Kind() == reflect.Pointer {
		schema := schemaOf(t.Elem(), seen)
		if kind, ok := schema["type"].(string); ok {
			schema["type"] = nullable(kind)
		}
		return schema
	}
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t.Implements(marshaler) || reflect.PointerTo(t).Implements(marshaler):
		return map[string]any{}
	case t.Implements(textMarshaler) || reflect.PointerTo(t).Implements(textMarshaler):
		return map[string]any{"type": "string"}
	}
	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string"}
		}
		return map[string]any{"type": nullable("array"), "items": schemaOf(t.Elem(), seen)}
	case reflect.Array:
		return map[string]any{"type": "array", "items": schemaOf(t.Elem(), seen)}
	case reflect.Map:
		return map[string]any{"type": nullable("object"), "additionalProperties": schemaOf(t.Elem(), seen)}
	case reflect.Struct:
		if seen[t] {
			return map[string]any{"type": "object"}
		}
		seen[t] = true
		defer delete(seen, t)
		properties := map[string]any{}
		for name, field := range fieldTypes(t) {
			properties[name] = schemaOf(field, seen)
		}
		return map[string]any{"type": "object", "properties": properties}
	}
	return map[string]any{}
}
