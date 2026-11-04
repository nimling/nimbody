package registry

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"
	"github.com/nimling/nimbody/respond"
)

const (
	DiscoveryPath = "/.well-known/terraform.json"
	ProvidersPath = "/terraform/providers/v1/"
	ModulesPath   = "/terraform/modules/v1/"
	StatePath     = "/terraform/state/"
	Namespace     = "samna"
	Protocol      = "6.0"
)

type Registry struct {
	Type       string
	Version    string
	Release    Release
	SigningKey SigningKey
	State      *Backend
}

type Discovery struct {
	Providers string `json:"providers.v1"`
	Modules   string `json:"modules.v1"`
}

type SigningKey struct {
	KeyID      string `json:"key_id"`
	ASCIIArmor string `json:"ascii_armor"`
}

type SigningKeys struct {
	GPGPublicKeys []SigningKey `json:"gpg_public_keys"`
}

type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type ProviderVersion struct {
	Version     string      `json:"version"`
	Protocols   []string    `json:"protocols"`
	Platforms   []Platform  `json:"platforms"`
	SigningKeys SigningKeys `json:"signing_keys"`
}

type ProviderVersions struct {
	Versions []ProviderVersion `json:"versions"`
}

type ProviderDownload struct {
	Protocols           []string    `json:"protocols"`
	OS                  string      `json:"os"`
	Arch                string      `json:"arch"`
	Filename            string      `json:"filename"`
	DownloadURL         string      `json:"download_url"`
	ShasumsURL          string      `json:"shasums_url"`
	ShasumsSignatureURL string      `json:"shasums_signature_url"`
	Shasum              string      `json:"shasum"`
	SigningKeys         SigningKeys `json:"signing_keys"`
}

type ModuleVersion struct {
	Version string `json:"version"`
}

type ModuleVersionList struct {
	Versions []ModuleVersion `json:"versions"`
}

type ModuleVersions struct {
	Modules []ModuleVersionList `json:"modules"`
}

type Failure struct {
	Errors []string `json:"errors"`
}

type service struct {
	registry Registry
	version  string
	mu       sync.Mutex
	cached   *release
}

func Mount(e *echo.Echo, r Registry) {
	s := &service{registry: r, version: strings.TrimPrefix(r.Version, "v")}
	e.GET(DiscoveryPath, s.getDiscovery)

	providers := e.Group(ProvidersPath + Namespace + "/" + r.Type)
	providers.GET("/versions", s.getProviderVersions)
	providers.GET("/:version/download/:os/:arch", s.getProviderDownload)
	providers.GET("/:version/files/:file", s.getProviderFile)

	modules := e.Group(ModulesPath + Namespace + "/:name/:system")
	modules.GET("/versions", s.getModuleVersions)
	modules.GET("/:version/download", s.getModuleDownload)
	modules.GET("/:version/files/:file", s.getModuleFile)

	if r.State != nil {
		r.State.mount(e)
	}
}

func base(ctx echo.Context) string {
	return ctx.Scheme() + "://" + ctx.Request().Host
}

func reply(ctx echo.Context, status int, body any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return respond.Passthrough(ctx, status, echo.MIMEApplicationJSON, encoded)
}

func refuse(ctx echo.Context, status int, message string) error {
	return reply(ctx, status, Failure{Errors: []string{message}})
}

func (s *service) getDiscovery(ctx echo.Context) error {
	return reply(ctx, http.StatusOK, Discovery{Providers: ProvidersPath, Modules: ModulesPath})
}

func (s *service) signingKeys() SigningKeys {
	keys := SigningKeys{GPGPublicKeys: []SigningKey{}}
	if s.registry.SigningKey.ASCIIArmor != "" {
		keys.GPGPublicKeys = append(keys.GPGPublicKeys, s.registry.SigningKey)
	}
	return keys
}

func (s *service) running(ctx echo.Context) bool {
	return s.version != "" && strings.TrimPrefix(ctx.Param("version"), "v") == s.version
}

func (s *service) unreachable(ctx echo.Context, err error) error {
	ctx.Logger().Error("terraform registry: " + err.Error())
	return refuse(ctx, http.StatusBadGateway, "the release of "+s.registry.Type+" "+s.version+" cannot be read")
}

func (s *service) getProviderVersions(ctx echo.Context) error {
	out := ProviderVersions{Versions: []ProviderVersion{}}
	rel, err := s.release(ctx.Request().Context())
	if err != nil {
		return s.unreachable(ctx, err)
	}
	if rel != nil && len(rel.platforms) > 0 {
		out.Versions = append(out.Versions, ProviderVersion{Version: s.version, Protocols: []string{Protocol}, Platforms: rel.platforms, SigningKeys: s.signingKeys()})
	}
	return reply(ctx, http.StatusOK, out)
}

func (s *service) getProviderDownload(ctx echo.Context) error {
	if !s.running(ctx) {
		return refuse(ctx, http.StatusNotFound, "this instance serves "+s.registry.Type+" "+s.version+" only")
	}
	rel, err := s.release(ctx.Request().Context())
	if err != nil {
		return s.unreachable(ctx, err)
	}
	files := GetFiles(s.registry.Type, s.version, ctx.Param("os"), ctx.Param("arch"))
	if rel == nil || rel.sums[files.Archive] == "" {
		return refuse(ctx, http.StatusNotFound, s.registry.Type+" "+s.version+" has no build for "+ctx.Param("os")+"_"+ctx.Param("arch"))
	}
	at := base(ctx) + ProvidersPath + Namespace + "/" + s.registry.Type + "/" + s.version + "/files/"
	return reply(ctx, http.StatusOK, ProviderDownload{
		Protocols:           []string{Protocol},
		OS:                  ctx.Param("os"),
		Arch:                ctx.Param("arch"),
		Filename:            files.Archive,
		DownloadURL:         at + files.Archive,
		ShasumsURL:          at + files.Sums,
		ShasumsSignatureURL: at + files.Signature,
		Shasum:              rel.sums[files.Archive],
		SigningKeys:         s.signingKeys(),
	})
}

func (s *service) getProviderFile(ctx echo.Context) error {
	if !s.running(ctx) {
		return refuse(ctx, http.StatusNotFound, "this instance serves "+s.registry.Type+" "+s.version+" only")
	}
	name := ctx.Param("file")
	files := GetFiles(s.registry.Type, s.version, "", "")
	if name != files.Sums && name != files.Signature && !strings.HasPrefix(name, "terraform-provider-"+s.registry.Type+"_"+s.version+"_") {
		return refuse(ctx, http.StatusNotFound, name+" is not a file of "+s.registry.Type+" "+s.version)
	}
	return s.stream(ctx, name)
}

func (s *service) module(ctx echo.Context) (string, bool, error) {
	rel, err := s.release(ctx.Request().Context())
	if err != nil {
		return "", false, err
	}
	name := GetModuleArchive(ctx.Param("name"), ctx.Param("system"), s.version)
	if rel == nil {
		return name, false, nil
	}
	_, ok := rel.assets[name]
	return name, ok, nil
}

func (s *service) getModuleVersions(ctx echo.Context) error {
	out := ModuleVersions{Modules: []ModuleVersionList{{Versions: []ModuleVersion{}}}}
	_, ok, err := s.module(ctx)
	if err != nil {
		return s.unreachable(ctx, err)
	}
	if ok {
		out.Modules[0].Versions = append(out.Modules[0].Versions, ModuleVersion{Version: s.version})
	}
	return reply(ctx, http.StatusOK, out)
}

func (s *service) getModuleDownload(ctx echo.Context) error {
	if !s.running(ctx) {
		return refuse(ctx, http.StatusNotFound, "this instance serves its modules at "+s.version+" only")
	}
	name, ok, err := s.module(ctx)
	if err != nil {
		return s.unreachable(ctx, err)
	}
	if !ok {
		return refuse(ctx, http.StatusNotFound, "module "+ctx.Param("name")+"/"+ctx.Param("system")+" is not in the release "+s.version)
	}
	at := base(ctx) + ModulesPath + Namespace + "/" + ctx.Param("name") + "/" + ctx.Param("system") + "/" + s.version + "/files/" + name
	ctx.Response().Header().Set("X-Terraform-Get", at)
	return ctx.NoContent(http.StatusNoContent)
}

func (s *service) getModuleFile(ctx echo.Context) error {
	if !s.running(ctx) || ctx.Param("file") != GetModuleArchive(ctx.Param("name"), ctx.Param("system"), s.version) {
		return refuse(ctx, http.StatusNotFound, ctx.Param("file")+" is not a module of "+s.version)
	}
	return s.stream(ctx, ctx.Param("file"))
}

func (s *service) stream(ctx echo.Context, name string) error {
	rel, err := s.release(ctx.Request().Context())
	if err != nil {
		return s.unreachable(ctx, err)
	}
	if rel == nil {
		return refuse(ctx, http.StatusNotFound, "there is no release "+s.version)
	}
	item, ok := rel.assets[name]
	if !ok {
		return refuse(ctx, http.StatusNotFound, name+" is not in the release "+s.version)
	}
	body, err := s.registry.Release.open(ctx.Request().Context(), item)
	if err != nil {
		return s.unreachable(ctx, err)
	}
	defer body.Close()
	contentType := echo.MIMEOctetStream
	switch {
	case strings.HasSuffix(name, ".zip"):
		contentType = "application/zip"
	case strings.HasSuffix(name, ".tar.gz"):
		contentType = "application/gzip"
	case strings.HasSuffix(name, "_SHA256SUMS"):
		contentType = echo.MIMETextPlainCharsetUTF8
	}
	if item.Size > 0 {
		ctx.Response().Header().Set(echo.HeaderContentLength, strconv.FormatInt(item.Size, 10))
	}
	return respond.Stream(ctx, http.StatusOK, contentType, body)
}
