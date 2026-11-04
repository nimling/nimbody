package respond

import (
	"encoding/json"
	"fmt"
)

func FilterToScope(value any, scope json.RawMessage) (any, error) {
	keys, ok, err := scopeKeys(scope)
	if err != nil {
		return nil, err
	}
	if !ok {
		return value, nil
	}
	return filterValue(value, keys), nil
}

func RejectOutOfScope(body map[string]any, scope json.RawMessage) error {
	keys, ok, err := scopeKeys(scope)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	for k := range body {
		if _, allowed := keys[k]; !allowed {
			return fmt.Errorf("field %q not in scope", k)
		}
	}
	return nil
}

func scopeKeys(scope json.RawMessage) (map[string]struct{}, bool, error) {
	if len(scope) == 0 || string(scope) == "null" {
		return nil, false, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(scope, &raw); err != nil {
		return nil, false, fmt.Errorf("decode scope: %w", err)
	}
	keys := make(map[string]struct{}, len(raw))
	for k := range raw {
		keys[k] = struct{}{}
	}
	return keys, true, nil
}

func filterValue(value any, keys map[string]struct{}) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, vv := range v {
			if _, allowed := keys[k]; allowed {
				out[k] = vv
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = filterValue(item, keys)
		}
		return out
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			return value
		}
		var asMap map[string]any
		if json.Unmarshal(raw, &asMap) == nil {
			return filterValue(asMap, keys)
		}
		var asSlice []any
		if json.Unmarshal(raw, &asSlice) == nil {
			return filterValue(asSlice, keys)
		}
		return value
	}
}
