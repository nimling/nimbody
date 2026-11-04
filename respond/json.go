package respond

import (
	"encoding/json"
	"github.com/labstack/echo/v4"
)

type FriendlyJSONSerializer struct{}

func (s *FriendlyJSONSerializer) Serialize(c echo.Context, i interface{}, indent string) error {
	enc := json.NewEncoder(c.Response())
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	return enc.Encode(i)
}

func (s *FriendlyJSONSerializer) Deserialize(c echo.Context, i interface{}) error {
	return json.NewDecoder(c.Request().Body).Decode(i)
}
