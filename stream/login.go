package stream

import (
	"bytes"
	_ "embed"
	"html/template"
	"strings"
	"unicode/utf8"
)

//go:embed login.html
var loginSource string

var loginPage = template.Must(template.New("login").Parse(loginSource))

type LoginPage struct {
	Name    string
	Product string
	Title   string
	Message string
	Failed  bool
}

func (p LoginPage) GetMark() string {
	r, size := utf8.DecodeRuneInString(p.Product)
	if size == 0 {
		return ""
	}
	return strings.ToUpper(string(r))
}

func GetLoginPage(page LoginPage) ([]byte, error) {
	var out bytes.Buffer
	err := loginPage.Execute(&out, page)
	return out.Bytes(), err
}
