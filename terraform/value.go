package terraform

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/nimling/nimbody/state"
)

func valueOf(data any, typ tftypes.Type) (tftypes.Value, error) {
	if data == nil {
		return tftypes.NewValue(typ, nil), nil
	}
	switch {
	case typ.Is(tftypes.DynamicPseudoType):
		return inferred(data)
	case typ.Is(tftypes.String):
		if text, ok := data.(string); ok {
			return tftypes.NewValue(typ, text), nil
		}
		return tftypes.NewValue(typ, fmt.Sprint(data)), nil
	case typ.Is(tftypes.Number):
		number, ok := new(big.Float).SetString(fmt.Sprint(data))
		if !ok {
			return tftypes.Value{}, fmt.Errorf("%v is not a number", data)
		}
		return tftypes.NewValue(typ, number), nil
	case typ.Is(tftypes.Bool):
		flag, ok := data.(bool)
		if !ok {
			return tftypes.Value{}, fmt.Errorf("%v is not a boolean", data)
		}
		return tftypes.NewValue(typ, flag), nil
	}
	switch kind := typ.(type) {
	case tftypes.List:
		return elements(data, typ, func(int) tftypes.Type { return kind.ElementType })
	case tftypes.Set:
		return elements(data, typ, func(int) tftypes.Type { return kind.ElementType })
	case tftypes.Tuple:
		return elements(data, typ, func(i int) tftypes.Type { return kind.ElementTypes[i] })
	case tftypes.Map:
		object, ok := data.(map[string]any)
		if !ok {
			return tftypes.Value{}, fmt.Errorf("%v is not an object", data)
		}
		values := map[string]tftypes.Value{}
		for name, item := range object {
			value, err := valueOf(item, kind.ElementType)
			if err != nil {
				return tftypes.Value{}, err
			}
			values[name] = value
		}
		return tftypes.NewValue(typ, values), nil
	case tftypes.Object:
		object, ok := data.(map[string]any)
		if !ok {
			return tftypes.Value{}, fmt.Errorf("%v is not an object", data)
		}
		values := map[string]tftypes.Value{}
		for name, attribute := range kind.AttributeTypes {
			value, err := valueOf(object[name], attribute)
			if err != nil {
				return tftypes.Value{}, fmt.Errorf("%s: %w", name, err)
			}
			values[name] = value
		}
		return tftypes.NewValue(typ, values), nil
	}
	return tftypes.Value{}, fmt.Errorf("type %s is not read here", typ)
}

func elements(data any, typ tftypes.Type, element func(int) tftypes.Type) (tftypes.Value, error) {
	list, ok := data.([]any)
	if !ok {
		return tftypes.Value{}, fmt.Errorf("%v is not a list", data)
	}
	values := make([]tftypes.Value, 0, len(list))
	for i, item := range list {
		value, err := valueOf(item, element(i))
		if err != nil {
			return tftypes.Value{}, err
		}
		values = append(values, value)
	}
	return tftypes.NewValue(typ, values), nil
}

func inferred(data any) (tftypes.Value, error) {
	switch value := data.(type) {
	case string:
		return tftypes.NewValue(tftypes.String, value), nil
	case bool:
		return tftypes.NewValue(tftypes.Bool, value), nil
	case json.Number, float64:
		return valueOf(value, tftypes.Number)
	case []any:
		types := make([]tftypes.Type, 0, len(value))
		values := make([]tftypes.Value, 0, len(value))
		for _, item := range value {
			inner, err := inferred(item)
			if err != nil {
				return tftypes.Value{}, err
			}
			types = append(types, inner.Type())
			values = append(values, inner)
		}
		return tftypes.NewValue(tftypes.Tuple{ElementTypes: types}, values), nil
	case map[string]any:
		types := map[string]tftypes.Type{}
		values := map[string]tftypes.Value{}
		for name, item := range value {
			inner, err := inferred(item)
			if err != nil {
				return tftypes.Value{}, err
			}
			types[name] = inner.Type()
			values[name] = inner
		}
		return tftypes.NewValue(tftypes.Object{AttributeTypes: types}, values), nil
	case nil:
		return tftypes.NewValue(tftypes.DynamicPseudoType, nil), nil
	}
	return tftypes.Value{}, fmt.Errorf("%v is not json", data)
}

func jsonOf(value tftypes.Value) (any, bool, error) {
	if value.IsNull() || !value.IsKnown() {
		return nil, false, nil
	}
	typ := value.Type()
	switch {
	case typ.Is(tftypes.String):
		var text string
		return text, true, value.As(&text)
	case typ.Is(tftypes.Bool):
		var flag bool
		return flag, true, value.As(&flag)
	case typ.Is(tftypes.Number):
		number := new(big.Float)
		if err := value.As(&number); err != nil {
			return nil, false, err
		}
		if number.IsInt() {
			return json.Number(number.Text('f', 0)), true, nil
		}
		return json.Number(number.Text('g', -1)), true, nil
	}
	switch typ.(type) {
	case tftypes.List, tftypes.Set, tftypes.Tuple:
		var items []tftypes.Value
		if err := value.As(&items); err != nil {
			return nil, false, err
		}
		out := make([]any, 0, len(items))
		for _, item := range items {
			converted, _, err := jsonOf(item)
			if err != nil {
				return nil, false, err
			}
			out = append(out, converted)
		}
		return out, true, nil
	case tftypes.Map, tftypes.Object:
		var fields map[string]tftypes.Value
		if err := value.As(&fields); err != nil {
			return nil, false, err
		}
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		sort.Strings(names)
		out := map[string]any{}
		for _, name := range names {
			converted, present, err := jsonOf(fields[name])
			if err != nil {
				return nil, false, err
			}
			if present {
				out[name] = converted
			}
		}
		return out, true, nil
	}
	return nil, false, fmt.Errorf("type %s is not written here", typ)
}

func at(value tftypes.Value, path *tftypes.AttributePath, typ tftypes.Type) tftypes.Value {
	found, _, err := tftypes.WalkAttributePath(value, path)
	if err != nil {
		return tftypes.NewValue(typ, nil)
	}
	if inner, ok := found.(tftypes.Value); ok {
		return inner
	}
	return tftypes.NewValue(typ, nil)
}

func matches(paths []*tftypes.AttributePath, path *tftypes.AttributePath) bool {
	for _, candidate := range paths {
		if candidate.Equal(path) {
			return true
		}
	}
	return false
}

func put(data map[string]any, tokens []string, value any) {
	for _, token := range tokens[:len(tokens)-1] {
		next, ok := data[token].(map[string]any)
		if !ok {
			next = map[string]any{}
			data[token] = next
		}
		data = next
	}
	data[tokens[len(tokens)-1]] = value
}

func (r resource) value(line state.Line) (tftypes.Value, error) {
	data := map[string]any{KeyAttribute: line.Key}
	for field, value := range line.Data {
		data[field] = value
	}
	for field, value := range line.Refs {
		data[field] = value
	}
	value, err := valueOf(data, r.typ)
	if err != nil {
		return tftypes.Value{}, err
	}
	return r.withoutWriteOnly(value)
}

func (r resource) withoutWriteOnly(value tftypes.Value) (tftypes.Value, error) {
	return tftypes.Transform(value, func(path *tftypes.AttributePath, inner tftypes.Value) (tftypes.Value, error) {
		if matches(r.writeOnly, path) {
			return tftypes.NewValue(inner.Type(), nil), nil
		}
		return inner, nil
	})
}

func (r resource) line(planned, config tftypes.Value) (state.Line, error) {
	raw, _, err := jsonOf(planned)
	if err != nil {
		return state.Line{}, err
	}
	data, _ := raw.(map[string]any)
	for _, path := range r.writeOnly {
		secret, present, err := jsonOf(at(config, path, tftypes.DynamicPseudoType))
		if err != nil {
			return state.Line{}, err
		}
		if !present {
			continue
		}
		tokens := make([]string, 0, len(path.Steps()))
		for _, step := range path.Steps() {
			tokens = append(tokens, string(step.(tftypes.AttributeName)))
		}
		put(data, tokens, secret)
	}
	key, _ := data[KeyAttribute].(string)
	delete(data, KeyAttribute)
	line := state.Line{Group: r.group.Name, Key: key, Data: data}
	for field := range r.group.Refs {
		if value, ok := data[field]; ok {
			if line.Refs == nil {
				line.Refs = map[string]any{}
			}
			line.Refs[field] = value
			delete(data, field)
		}
	}
	return line, nil
}
