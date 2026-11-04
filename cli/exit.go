package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

const (
	ExitOK              = 0
	ExitFailure         = 1
	ExitRejected        = 2
	ExitRefused         = 3
	ExitUnauthenticated = 4
	ExitInterrupted     = 130
)

type Error struct {
	Code    int
	Message string
	Hint    string
	Cause   error
}

func (e *Error) Error() string {
	switch {
	case e.Cause == nil:
		return e.Message
	case e.Message == "":
		return e.Cause.Error()
	default:
		return e.Message + ": " + e.Cause.Error()
	}
}

func (e *Error) Unwrap() error { return e.Cause }

func Rejected(message, hint string) error {
	return &Error{Code: ExitRejected, Message: message, Hint: hint}
}

func Refused(message, hint string) error {
	return &Error{Code: ExitRefused, Message: message, Hint: hint}
}

func Unauthenticated(message, hint string) error {
	return &Error{Code: ExitUnauthenticated, Message: message, Hint: hint}
}

func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Code
	}
	if errors.Is(err, context.Canceled) {
		return ExitInterrupted
	}
	if usage(err) {
		return ExitRejected
	}
	return ExitFailure
}

var usagePrefixes = []string{"unknown command", "unknown flag", "unknown shorthand flag", "flag needs an argument", "invalid argument", "accepts ", "requires at least", "requires at most", "required flag", "if any flags in the group"}

func usage(err error) bool {
	text := err.Error()
	for _, prefix := range usagePrefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func Report(out io.Writer, name string, err error) {
	if err == nil {
		return
	}
	var done *Error
	if errors.As(err, &done) && done.Code == ExitOK {
		return
	}
	fmt.Fprintf(out, "%s: %s\n", name, err.Error())
	var coded *Error
	if errors.As(err, &coded) && coded.Hint != "" {
		fmt.Fprintf(out, "%s\n", coded.Hint)
		return
	}
	if usage(err) {
		fmt.Fprintf(out, "Run '%s --help' for usage.\n", name)
	}
}

const EnvTerraform = "TF_PLUGIN_MAGIC_COOKIE"

var Provider func() error

func Execute(root *cobra.Command) int {
	root.SilenceErrors = true
	root.SilenceUsage = true
	if Provider != nil && os.Getenv(EnvTerraform) != "" {
		err := Provider()
		Report(root.ErrOrStderr(), root.Name(), err)
		return ExitCode(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := root.ExecuteContext(ctx)
	if err != nil && ctx.Err() != nil {
		err = &Error{Code: ExitInterrupted, Message: "interrupted"}
	}
	Report(root.ErrOrStderr(), root.Name(), err)
	return ExitCode(err)
}
