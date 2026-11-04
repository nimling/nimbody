package mcp

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody/respond"
)

const (
	InPath  = "path"
	InQuery = "query"
	InBody  = "body"
)

type Spec struct {
	Name        string
	Description string
	Method      string
	Path        string
	Access      string
	Page        string
	Middleware  []echo.MiddlewareFunc
}

type Input struct {
	Name     string
	Param    string
	In       string
	Kind     string
	Usage    string
	Required bool
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

var (
	marshaler     = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
	timeType      = reflect.TypeFor[time.Time]()
	rawType       = reflect.TypeFor[json.RawMessage]()
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
	status := http.StatusOK
	if spec.Method == http.MethodPost {
		status = http.StatusCreated
	}
	inputs := inputsOf(reflect.TypeFor[In]())
	operation := Operation{
		Spec:         spec,
		Inputs:       inputs,
		InputSchema:  inputSchema(inputs, reflect.TypeFor[In]()),
		OutputSchema: outputSchema(reflect.TypeFor[Out]()),
		Route: func(ctx echo.Context) error {
			var in In
			if err := (&echo.DefaultBinder{}).Bind(&in, ctx); err != nil {
				return respond.Error(ctx, http.StatusBadRequest, message(err))
			}
			value, count, err := run(ctx, in)
			var rejected *echo.HTTPError
			switch {
			case errors.As(err, &rejected):
				return respond.Error(ctx, rejected.Code, message(rejected))
			case err != nil:
				ctx.Logger().Error(spec.Name + ": " + err.Error())
				return respond.ErrorInternal(ctx)
			}
			return respond.JSON(ctx, status, value, count)
		},
		call: func(ctx echo.Context, arguments json.RawMessage) (any, int, error) {
			var in In
			if len(arguments) > 0 {
				if err := json.Unmarshal(arguments, &in); err != nil {
					return nil, 0, echo.NewHTTPError(http.StatusBadRequest, err.Error())
				}
			}
			return run(ctx, in)
		},
	}
	return operation
}

func Route(group *echo.Group, operations ...Operation) {
	for _, operation := range operations {
		group.Add(operation.Method, operation.Path, operation.Route, operation.Middleware...)
	}
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
		Description:  o.Description,
		InputSchema:  o.InputSchema,
		OutputSchema: o.OutputSchema,
		Page:         o.Page,
	}, o.Access)
	return tool
}

func inputsOf(t reflect.Type) []Input {
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
		if field.Anonymous && field.Tag.Get("json") == "" && field.Type.Kind() == reflect.Struct {
			inputs = append(inputs, inputsOf(field.Type)...)
			continue
		}
		input := Input{Name: name, Param: name, In: InBody, Kind: kindOf(field.Type), Usage: field.Tag.Get("jsonschema")}
		switch {
		case field.Tag.Get("param") != "":
			input.In, input.Param, input.Required = InPath, field.Tag.Get("param"), true
		case field.Tag.Get("query") != "":
			input.In, input.Param = InQuery, field.Tag.Get("query")
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

func outputSchema(t reflect.Type) map[string]any {
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
