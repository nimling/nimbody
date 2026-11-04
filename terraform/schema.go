package terraform

import (
	"fmt"
	"sort"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/nimling/nimbody/state"
)

const KeyAttribute = "key"

type resource struct {
	group     state.GroupSchema
	schema    *tfprotov6.Schema
	typ       tftypes.Type
	writeOnly []*tftypes.AttributePath
	immutable []*tftypes.AttributePath
	computed  []*tftypes.AttributePath
}

type flags struct {
	writeOnly map[string]bool
	computed  map[string]bool
}

func resourceOf(group state.GroupSchema) (resource, error) {
	properties := state.GetProperties(group.Schema)
	if _, taken := properties[KeyAttribute]; taken {
		return resource{}, fmt.Errorf("group %s has a field called %s, which is the record key in terraform", group.Name, KeyAttribute)
	}
	r := resource{group: group}
	f := flags{writeOnly: map[string]bool{}, computed: map[string]bool{}}
	secrets := append([]string{}, group.Fields.Sensitive...)
	for _, pointers := range group.Fields.SensitiveBy {
		secrets = append(secrets, pointers...)
	}
	for _, pointer := range secrets {
		f.writeOnly[pointer] = true
	}
	for _, pointer := range group.Fields.ServerDefault {
		if !f.writeOnly[pointer] {
			f.computed[pointer] = true
		}
	}

	attributes := attributesOf(group.Schema, "", &f)
	for field, target := range group.Refs {
		if _, ok := properties[field]; !ok {
			attributes = append(attributes, &tfprotov6.SchemaAttribute{Name: field, Type: tftypes.String, Optional: true, Description: "the key of a " + target})
		}
	}
	for pointer := range f.writeOnly {
		tokens, err := state.ParsePointer(pointer)
		if err != nil {
			return resource{}, err
		}
		attributes = ensure(attributes, tokens)
	}
	attributes = append(attributes, &tfprotov6.SchemaAttribute{Name: KeyAttribute, Type: tftypes.String, Required: true, Description: "the record key, its identity in the service and in references"})
	sortAttributes(attributes)
	r.schema = &tfprotov6.Schema{Block: &tfprotov6.SchemaBlock{Attributes: attributes, Description: "The " + group.Name + " records of the service, through its state import and export"}}
	r.typ = r.schema.ValueType()

	for pointer := range f.writeOnly {
		r.writeOnly = append(r.writeOnly, pathOf(pointer))
	}
	for pointer := range f.computed {
		r.computed = append(r.computed, pathOf(pointer))
	}
	r.immutable = append(r.immutable, tftypes.NewAttributePath().WithAttributeName(KeyAttribute))
	for _, pointer := range group.Fields.Immutable {
		r.immutable = append(r.immutable, pathOf(pointer))
	}
	return r, nil
}

func pathOf(pointer string) *tftypes.AttributePath {
	tokens, _ := state.ParsePointer(pointer)
	path := tftypes.NewAttributePath()
	for _, token := range tokens {
		path = path.WithAttributeName(token)
	}
	return path
}

func attributesOf(schema map[string]any, pointer string, f *flags) []*tfprotov6.SchemaAttribute {
	required := map[string]bool{}
	listed, _ := schema["required"].([]any)
	for _, name := range listed {
		if text, ok := name.(string); ok {
			required[text] = true
		}
	}
	var attributes []*tfprotov6.SchemaAttribute
	for name, raw := range state.GetProperties(schema) {
		property, _ := raw.(map[string]any)
		at := state.JoinPointer(pointer, name)
		attribute := &tfprotov6.SchemaAttribute{Name: name}
		attribute.Description, _ = property["description"].(string)
		switch {
		case f != nil && f.writeOnly[at]:
			attribute.Type = typeOf(property)
			attribute.WriteOnly, attribute.Sensitive = true, true
		case kindOf(property) == "object" && len(state.GetProperties(property)) > 0:
			attribute.NestedType = &tfprotov6.SchemaObject{Nesting: tfprotov6.SchemaObjectNestingModeSingle, Attributes: attributesOf(property, at, f)}
		case kindOf(property) == "array" && objects(property):
			items, _ := property["items"].(map[string]any)
			attribute.NestedType = &tfprotov6.SchemaObject{Nesting: tfprotov6.SchemaObjectNestingModeList, Attributes: attributesOf(items, "", nil)}
		default:
			attribute.Type = typeOf(property)
		}
		attribute.Computed = f != nil && f.computed[at]
		attribute.Required = required[name] && !attribute.Computed
		attribute.Optional = !attribute.Required
		attributes = append(attributes, attribute)
	}
	sortAttributes(attributes)
	return attributes
}

func ensure(attributes []*tfprotov6.SchemaAttribute, tokens []string) []*tfprotov6.SchemaAttribute {
	for _, attribute := range attributes {
		if attribute.Name != tokens[0] {
			continue
		}
		if len(tokens) > 1 && attribute.NestedType != nil && attribute.NestedType.Nesting == tfprotov6.SchemaObjectNestingModeSingle {
			attribute.NestedType.Attributes = ensure(attribute.NestedType.Attributes, tokens[1:])
			sortAttributes(attribute.NestedType.Attributes)
		}
		return attributes
	}
	if len(tokens) == 1 {
		return append(attributes, &tfprotov6.SchemaAttribute{Name: tokens[0], Type: tftypes.String, Optional: true, WriteOnly: true, Sensitive: true})
	}
	return append(attributes, &tfprotov6.SchemaAttribute{Name: tokens[0], Optional: true, NestedType: &tfprotov6.SchemaObject{Nesting: tfprotov6.SchemaObjectNestingModeSingle, Attributes: ensure(nil, tokens[1:])}})
}

func sortAttributes(attributes []*tfprotov6.SchemaAttribute) {
	sort.Slice(attributes, func(i, j int) bool { return attributes[i].Name < attributes[j].Name })
}

func kindOf(property map[string]any) string {
	switch kind := property["type"].(type) {
	case string:
		return kind
	case []any:
		for _, item := range kind {
			if text, ok := item.(string); ok && text != "null" {
				return text
			}
		}
	}
	if _, ok := property["properties"]; ok {
		return "object"
	}
	if _, ok := property["discriminator"]; ok {
		return "object"
	}
	return ""
}

func objects(property map[string]any) bool {
	items, _ := property["items"].(map[string]any)
	return kindOf(items) == "object" && len(state.GetProperties(items)) > 0
}

func typeOf(property map[string]any) tftypes.Type {
	switch kindOf(property) {
	case "string":
		return tftypes.String
	case "integer", "number":
		return tftypes.Number
	case "boolean":
		return tftypes.Bool
	case "array":
		items, ok := property["items"].(map[string]any)
		if !ok {
			return tftypes.DynamicPseudoType
		}
		element := typeOf(items)
		if element.Is(tftypes.DynamicPseudoType) {
			return tftypes.DynamicPseudoType
		}
		return tftypes.List{ElementType: element}
	case "object":
		properties := state.GetProperties(property)
		if len(properties) > 0 {
			types := map[string]tftypes.Type{}
			for name, raw := range properties {
				nested, _ := raw.(map[string]any)
				types[name] = typeOf(nested)
			}
			return tftypes.Object{AttributeTypes: types}
		}
		if values, ok := property["additionalProperties"].(map[string]any); ok {
			if element := typeOf(values); !element.Is(tftypes.DynamicPseudoType) {
				return tftypes.Map{ElementType: element}
			}
		}
	}
	return tftypes.DynamicPseudoType
}
