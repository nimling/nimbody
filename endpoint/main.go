package endpoint

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody/mcp"
	"github.com/nimling/nimbody/respond"
	"github.com/nimling/nimbody/state"
	"github.com/nimling/nimbody/terraform/registry"
)

const (
	StatusPath = "/status"
	HealthPath = "/status/health"
	StatePath  = "/state"
)

type Set struct {
	Service   string
	Version   string
	Health    func(ctx context.Context) error
	MCP       *mcp.Server
	MCPAccess []echo.MiddlewareFunc
	State     *state.Context
	Groups    []state.Definition
	Terraform *registry.Registry
}

type Status struct {
	Service   string   `json:"service"`
	Version   string   `json:"version"`
	Endpoints []string `json:"endpoints"`
}

type Health struct {
	Healthy bool `json:"healthy"`
}

func Mount(e *echo.Echo, set Set) {
	status := Status{Service: set.Service, Version: set.Version, Endpoints: []string{"/api", StatusPath}}

	var operations []mcp.Operation
	if set.State != nil {
		host := *set.State
		if host.Service == "" {
			host.Service = set.Service
		}
		if host.Version == "" {
			host.Version = set.Version
		}
		operations = state.Register(e.Group(""), host, set.Groups...)
		status.Endpoints = append(status.Endpoints, StatePath)
	}

	if set.MCP != nil {
		set.MCP.Operations = append(set.MCP.Operations, operations...)
		set.MCP.Mount(e, set.MCPAccess...)
		status.Endpoints = append(status.Endpoints, mcp.MountPath)
	}

	if set.Terraform != nil {
		r := *set.Terraform
		if r.Version == "" {
			r.Version = set.Version
		}
		registry.Mount(e, r)
		status.Endpoints = append(status.Endpoints, registry.DiscoveryPath, registry.ProvidersPath, registry.ModulesPath)
		if r.State != nil {
			status.Endpoints = append(status.Endpoints, registry.StatePath)
		}
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
}
