package cli

import (
	"io"
	"os"
)

func Terminal(out io.Writer) bool {
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func Colour(out io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("CLICOLOR") == "0" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if force := os.Getenv("CLICOLOR_FORCE"); force != "" && force != "0" {
		return true
	}
	return Terminal(out)
}

type Style struct {
	on bool
}

func StyleFor(out io.Writer) Style {
	return Style{on: Colour(out)}
}

func (s Style) wrap(code, text string) string {
	if !s.on || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s Style) Bold(text string) string   { return s.wrap("1", text) }
func (s Style) Dim(text string) string    { return s.wrap("2", text) }
func (s Style) Red(text string) string    { return s.wrap("31", text) }
func (s Style) Green(text string) string  { return s.wrap("32", text) }
func (s Style) Warn(text string) string   { return s.wrap("33", text) }
func (s Style) Accent(text string) string { return s.wrap("36", text) }
