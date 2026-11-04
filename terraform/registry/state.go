package registry

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody/respond"
)

const (
	MethodLock   = "LOCK"
	MethodUnlock = "UNLOCK"
)

type Lock struct {
	ID        string `json:"ID"`
	Operation string `json:"Operation"`
	Info      string `json:"Info"`
	Who       string `json:"Who"`
	Version   string `json:"Version"`
	Created   string `json:"Created"`
	Path      string `json:"Path"`
}

type Backend struct {
	Access      echo.MiddlewareFunc
	GetState    func(ctx echo.Context, workspace string) ([]byte, error)
	PutState    func(ctx echo.Context, workspace string, body []byte) error
	DeleteState func(ctx echo.Context, workspace string) error
	GetLock     func(ctx echo.Context, workspace string) (*Lock, error)
	PostLock    func(ctx echo.Context, workspace string, lock Lock) (Lock, error)
	DeleteLock  func(ctx echo.Context, workspace string) error
}

var workspaceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (b *Backend) mount(e *echo.Echo) {
	var middleware []echo.MiddlewareFunc
	if b.Access != nil {
		middleware = append(middleware, b.Access)
	}
	group := e.Group(StatePath+":workspace", append(middleware, b.workspace)...)
	group.GET("", b.getState)
	group.POST("", b.postState)
	group.DELETE("", b.deleteState)
	group.Add(MethodLock, "", b.lock)
	group.Add(MethodUnlock, "", b.unlock)
}

func (b *Backend) workspace(next echo.HandlerFunc) echo.HandlerFunc {
	return func(ctx echo.Context) error {
		if !workspaceName.MatchString(ctx.Param("workspace")) {
			return respond.Error(ctx, http.StatusBadRequest, "a workspace is letters, digits, dot, dash and underscore, at most 128")
		}
		return next(ctx)
	}
}

func (b *Backend) failure(ctx echo.Context, err error) error {
	var rejected *echo.HTTPError
	if errors.As(err, &rejected) {
		if text, ok := rejected.Message.(string); ok {
			return respond.Error(ctx, rejected.Code, text)
		}
		return respond.Error(ctx, rejected.Code, http.StatusText(rejected.Code))
	}
	ctx.Logger().Error("terraform state " + ctx.Param("workspace") + ": " + err.Error())
	return respond.ErrorInternal(ctx)
}

func (b *Backend) held(ctx echo.Context, lock Lock) error {
	body, err := json.Marshal(lock)
	if err != nil {
		return err
	}
	return respond.Passthrough(ctx, http.StatusLocked, echo.MIMEApplicationJSON, body)
}

func (b *Backend) getState(ctx echo.Context) error {
	body, err := b.GetState(ctx, ctx.Param("workspace"))
	if err != nil {
		return b.failure(ctx, err)
	}
	if body == nil {
		return ctx.NoContent(http.StatusNotFound)
	}
	return respond.Passthrough(ctx, http.StatusOK, echo.MIMEApplicationJSON, body)
}

func (b *Backend) postState(ctx echo.Context) error {
	workspace := ctx.Param("workspace")
	body, err := io.ReadAll(ctx.Request().Body)
	if err != nil {
		return respond.Error(ctx, http.StatusBadRequest, "the state body cannot be read")
	}
	if digest := ctx.Request().Header.Get("Content-MD5"); digest != "" {
		sum := md5.Sum(body)
		if digest != base64.StdEncoding.EncodeToString(sum[:]) {
			return respond.Error(ctx, http.StatusBadRequest, "the state body does not match its Content-MD5")
		}
	}
	if !json.Valid(body) {
		return respond.Error(ctx, http.StatusBadRequest, "the state body is not json")
	}
	current, err := b.GetLock(ctx, workspace)
	if err != nil {
		return b.failure(ctx, err)
	}
	if current != nil && current.ID != ctx.QueryParam("ID") {
		return b.held(ctx, *current)
	}
	if err := b.PutState(ctx, workspace, body); err != nil {
		return b.failure(ctx, err)
	}
	return ctx.NoContent(http.StatusOK)
}

func (b *Backend) deleteState(ctx echo.Context) error {
	workspace := ctx.Param("workspace")
	current, err := b.GetLock(ctx, workspace)
	if err != nil {
		return b.failure(ctx, err)
	}
	if current != nil && current.ID != ctx.QueryParam("ID") {
		return b.held(ctx, *current)
	}
	if err := b.DeleteState(ctx, workspace); err != nil {
		return b.failure(ctx, err)
	}
	return ctx.NoContent(http.StatusOK)
}

func readLock(ctx echo.Context) (*Lock, error) {
	body, err := io.ReadAll(ctx.Request().Body)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, nil
	}
	var lock Lock
	if err := json.Unmarshal(body, &lock); err != nil {
		return nil, err
	}
	return &lock, nil
}

func (b *Backend) lock(ctx echo.Context) error {
	lock, err := readLock(ctx)
	if err != nil || lock == nil || lock.ID == "" {
		return respond.Error(ctx, http.StatusBadRequest, "a lock takes the lock info as json with its ID")
	}
	current, err := b.PostLock(ctx, ctx.Param("workspace"), *lock)
	if err != nil {
		return b.failure(ctx, err)
	}
	if current.ID != lock.ID {
		return b.held(ctx, current)
	}
	return ctx.NoContent(http.StatusOK)
}

func (b *Backend) unlock(ctx echo.Context) error {
	workspace := ctx.Param("workspace")
	lock, err := readLock(ctx)
	if err != nil {
		return respond.Error(ctx, http.StatusBadRequest, "an unlock takes the lock info as json, or no body to force it")
	}
	current, err := b.GetLock(ctx, workspace)
	if err != nil {
		return b.failure(ctx, err)
	}
	if current == nil {
		return ctx.NoContent(http.StatusOK)
	}
	if lock != nil && lock.ID != current.ID {
		return b.held(ctx, *current)
	}
	if err := b.DeleteLock(ctx, workspace); err != nil {
		return b.failure(ctx, err)
	}
	return ctx.NoContent(http.StatusOK)
}
