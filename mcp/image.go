package mcp

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const imageLimit = 8 << 20

var imageTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

func shown(result string, failed bool, key string) []content {
	out := []content{{Type: "text", Text: result}}
	if failed || key == "" {
		return out
	}
	path := strings.TrimSpace(result)
	var fields map[string]any
	if json.Unmarshal([]byte(path), &fields) == nil {
		held, _ := fields[key].(string)
		path = held
	}
	kind, known := imageTypes[strings.ToLower(filepath.Ext(path))]
	if !known {
		return out
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > imageLimit {
		return out
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	return append(out, content{Type: "image", Data: base64.StdEncoding.EncodeToString(raw), MimeType: kind})
}
