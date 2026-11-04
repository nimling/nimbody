package state

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
)

func ParsePointer(pointer string) ([]string, error) {
	if !strings.HasPrefix(pointer, "/") {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "a field pointer starts with /, not "+pointer)
	}
	tokens := strings.Split(pointer[1:], "/")
	for i, token := range tokens {
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
	}
	return tokens, nil
}

func JoinPointer(pointer, token string) string {
	return pointer + "/" + strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
}

func GetProperties(schema map[string]any) map[string]any {
	properties := map[string]any{}
	branches := []map[string]any{schema}
	if _, discriminated := schema["discriminator"].(map[string]any); discriminated {
		for _, union := range []string{"oneOf", "anyOf"} {
			list, _ := schema[union].([]any)
			for _, item := range list {
				if branch, ok := item.(map[string]any); ok {
					branches = append(branches, branch)
				}
			}
		}
	}
	for _, branch := range branches {
		own, _ := branch["properties"].(map[string]any)
		for name, property := range own {
			if _, taken := properties[name]; !taken {
				properties[name] = property
			}
		}
	}
	return properties
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for item := range set {
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

func lookup(data map[string]any, pointer string) (any, bool) {
	tokens, err := ParsePointer(pointer)
	if err != nil {
		return nil, false
	}
	var current any = data
	for _, token := range tokens {
		switch node := current.(type) {
		case map[string]any:
			value, ok := node[token]
			if !ok {
				return nil, false
			}
			current = value
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func strip(data map[string]any, pointer string) map[string]any {
	tokens, err := ParsePointer(pointer)
	if err != nil {
		return data
	}
	stripped, _ := without(data, tokens).(map[string]any)
	return stripped
}

func without(node any, tokens []string) any {
	switch value := node.(type) {
	case map[string]any:
		child, ok := value[tokens[0]]
		if !ok {
			return node
		}
		out := make(map[string]any, len(value))
		for field, item := range value {
			out[field] = item
		}
		if len(tokens) == 1 {
			delete(out, tokens[0])
		} else {
			out[tokens[0]] = without(child, tokens[1:])
		}
		return out
	case []any:
		index, err := strconv.Atoi(tokens[0])
		if err != nil || index < 0 || index >= len(value) {
			return node
		}
		out := append([]any{}, value...)
		if len(tokens) == 1 {
			out[index] = nil
		} else {
			out[index] = without(value[index], tokens[1:])
		}
		return out
	default:
		return node
	}
}

func dotted(pointer string) string {
	tokens, err := ParsePointer(pointer)
	if err != nil {
		return pointer
	}
	return strings.Join(tokens, ".")
}

func discriminated(data map[string]any, pointer string) string {
	if pointer == "" {
		return ""
	}
	value, ok := lookup(data, pointer)
	if !ok || value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}
