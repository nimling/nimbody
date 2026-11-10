package endpoint

import (
	"context"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody/mcp"
	"github.com/nimling/nimbody/respond"
	"github.com/nimling/nimbody/state"
)

const (
	APIPath    = "/api"
	StatusPath = "/status"
	HealthPath = "/status/health"
)

type Set struct {
	Service       string
	Version       string
	Health        func(ctx context.Context) error
	MCP           *mcp.Server
	MCPAccess     []echo.MiddlewareFunc
	APIMiddleware []echo.MiddlewareFunc
	State         *state.Context
	Groups        []state.Definition
}

type Status struct {
	Service   string   `json:"service"`
	Version   string   `json:"version"`
	Endpoints []string `json:"endpoints"`
}

type Health struct {
	Healthy bool `json:"healthy"`
}

func Mount(e *echo.Echo, set Set) error {
	status := Status{Service: set.Service, Version: set.Version, Endpoints: []string{APIPath, StatusPath}}

	var operations []mcp.Operation
	if set.State != nil {
		host := *set.State
		if host.Service == "" {
			host.Service = set.Service
		}
		if host.Version == "" {
			host.Version = set.Version
		}
		operations = state.Register(host, set.APIMiddleware, set.Groups...)
		mcp.Route(e.Group(APIPath), operations...)
		status.Endpoints = append(status.Endpoints, APIPath+state.Path)
	}

	if set.MCP != nil {
		set.MCP.Operations = append(set.MCP.Operations, operations...)
		var names []string
		for _, operation := range set.MCP.Operations {
			names = append(names, operation.Name)
		}
		for _, page := range set.MCP.Pages {
			for _, tool := range page.Tools {
				names = append(names, tool.Name)
			}
		}
		named := map[string]bool{}
		for _, name := range names {
			if named[name] {
				return errors.New("endpoint: two tools are called " + name)
			}
			named[name] = true
		}
		set.MCP.Mount(e, set.MCPAccess...)
		status.Endpoints = append(status.Endpoints, mcp.MountPath)
	}

	e.GET(StatusPath, func(ctx echo.Context) error {
		return respond.JSON(ctx, http.StatusOK, status, 1)
	})
	e.GET(HealthPath, func(ctx echo.Context) error {
		if set.Health != nil {
			if err := set.Health(ctx.Request().Context()); err != nil {
				ctx.Logger().Error("health: " + err.Error())
				return respond.Error(ctx, http.StatusServiceUnavailable, "unhealthy", Health{Healthy: false})
			}
		}
		return respond.JSON(ctx, http.StatusOK, Health{Healthy: true}, 1)
	})
	return nil
}
