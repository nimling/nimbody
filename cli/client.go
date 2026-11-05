package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nimling/nimbody/mcp"
	"github.com/nimling/nimbody/respond"
	"github.com/nimling/nimbody/stream"
	"github.com/spf13/cobra"
)

const (
	AuthToken  = "token"
	AuthLogin  = "login"
	AuthClient = "client_credentials"
	AuthNone   = "none"

	KeyEndpoint     = "endpoint"
	KeyAppSlug      = "app_slug"
	KeyClientID     = "client_id"
	KeyClientSecret = "client_secret"

	PathStatus = "/status"
	PathHealth = "/status/health"

	FlagStream  = "stream"
	StreamUsage = "send Accept: text/event-stream and print every frame as it arrives until the stream ends"

	tokenSkew = 30 * time.Second
)

type Client struct {
	Settings     *Settings
	Product      string
	Version      func() string
	LoginPath    string
	TokenPath    string
	RevokePath   string
	IdentityPath string
	Exchange     string
	HealthPath   string
	AppHeader    string
	HTTP         *http.Client
	Output       *Output

	mode     string
	resolved bool
	cred     Credential
	token    string
	expiry   time.Time
	issued   Token
	reused   bool
	rejected string
	probe    sync.Once
	probed   error
	service  serviceStatus
}

type serviceStatus struct {
	Service   string   `json:"service"`
	Version   string   `json:"version"`
	Endpoints []string `json:"endpoints"`
}

type Token struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope,omitempty"`
}

type clientEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) name() string { return c.Settings.Name }

func (c *Client) http() *http.Client {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 120 * time.Second}
	}
	return c.HTTP
}

func (c *Client) Endpoint() string {
	return strings.TrimRight(c.Settings.Get(KeyEndpoint), "/")
}

func (c *Client) EnvToken() string {
	return firstSet(os.Getenv(c.Settings.Prefix+"TOKEN"), os.Getenv(c.Settings.Prefix+"ACCESS_TOKEN"))
}

func (c *Client) Mode() string {
	c.resolve()
	return c.mode
}

func (c *Client) resolve() {
	if c.resolved {
		return
	}
	c.resolved = true
	c.mode = AuthNone
	switch {
	case c.EnvToken() != "":
		c.mode = AuthToken
	case c.Endpoint() == "":
	default:
		if cred, err := c.Settings.GetCredential(c.Endpoint(), ""); err == nil {
			c.mode, c.cred = AuthLogin, cred
			return
		}
		if c.Settings.Get(KeyClientID) != "" && c.Settings.Get(KeyClientSecret) != "" {
			c.mode = AuthClient
		}
	}
}

func (c *Client) unauthenticated() error {
	return Unauthenticated("no credential for "+firstSet(c.Endpoint(), "this config"), "Run '"+c.name()+" login', set "+c.Settings.Prefix+"CLIENT_ID and "+c.Settings.Prefix+"CLIENT_SECRET, or "+c.Settings.Prefix+"TOKEN in CI.")
}

func (c *Client) GetToken(ctx context.Context) (Token, error) {
	c.resolve()
	switch {
	case c.mode == AuthToken:
		return Token{AccessToken: c.EnvToken(), TokenType: "Bearer"}, nil
	case c.mode == AuthLogin:
		return c.refresh(ctx)
	case c.mode == AuthClient && c.Exchange != "":
		return c.exchange(ctx)
	case c.mode == AuthClient:
		return Token{}, Rejected("a machine client sends "+c.Settings.Prefix+"CLIENT_ID and "+c.Settings.Prefix+"CLIENT_SECRET to "+c.Endpoint()+" as Basic auth on every request, so it holds no token", "")
	}
	return Token{}, c.unauthenticated()
}

func (c *Client) refresh(ctx context.Context) (Token, error) {
	unlock, err := c.Settings.LockCredential(c.Endpoint(), "")
	if err != nil {
		return Token{}, err
	}
	defer unlock()
	if stored, err := c.Settings.GetCredential(c.Endpoint(), ""); err == nil {
		c.cred = stored
	}
	if c.cred.AccessToken != "" && time.Now().Add(tokenSkew).Before(c.cred.AccessExpiresAt) {
		return Token{AccessToken: c.cred.AccessToken, TokenType: "Bearer", ExpiresIn: int(time.Until(c.cred.AccessExpiresAt).Seconds())}, nil
	}
	if c.cred.RefreshToken == "" {
		return Token{}, c.unauthenticated()
	}
	var tokens cliTokens
	if err := c.postForm(ctx, c.Endpoint(), c.TokenPath, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.cred.RefreshToken}}, &tokens); err != nil {
		return Token{}, Unauthenticated(err.Error(), "Run '"+c.name()+" login' again.")
	}
	if tokens.RefreshToken != "" {
		c.cred.RefreshToken = tokens.RefreshToken
	}
	c.cred.AccessToken = tokens.AccessToken
	c.cred.AccessExpiresAt = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	c.cred.ObtainedAt = time.Now()
	if _, err := c.Settings.PutCredential(c.cred); err != nil {
		return Token{}, err
	}
	return Token{AccessToken: tokens.AccessToken, TokenType: "Bearer", ExpiresIn: tokens.ExpiresIn}, nil
}

func (c *Client) exchange(ctx context.Context) (Token, error) {
	clientID := c.Settings.Get(KeyClientID)
	unlock, err := c.Settings.LockCredential(c.Endpoint(), clientID)
	if err != nil {
		return Token{}, err
	}
	defer unlock()
	if stored, err := c.Settings.GetCredential(c.Endpoint(), clientID); err == nil {
		c.cred = stored
	}
	if c.cred.AccessToken != "" && c.cred.AccessToken != c.rejected && time.Now().Add(tokenSkew).Before(c.cred.AccessExpiresAt) {
		c.reused = true
		return Token{AccessToken: c.cred.AccessToken, TokenType: "Bearer", ExpiresIn: int(time.Until(c.cred.AccessExpiresAt).Seconds())}, nil
	}
	c.reused = false
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint()+c.Exchange, strings.NewReader(url.Values{"grant_type": {"client_credentials"}}.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.headers(req)
	req.SetBasicAuth(clientID, c.Settings.Get(KeyClientSecret))
	body, err := c.do(req)
	if err != nil {
		return Token{}, err
	}
	var token Token
	if err := json.Unmarshal(body, &token); err != nil || token.AccessToken == "" {
		var wrapped struct {
			Data Token `json:"data"`
		}
		if json.Unmarshal(body, &wrapped) != nil || wrapped.Data.AccessToken == "" {
			return Token{}, errors.New("the token response of " + c.Exchange + " carried no access token")
		}
		token = wrapped.Data
	}
	c.cred = Credential{Endpoint: c.Endpoint(), AppSlug: c.Settings.Get(KeyAppSlug), ClientID: clientID, AccessToken: token.AccessToken, AccessExpiresAt: time.Now().Add(time.Duration(token.ExpiresIn) * time.Second), ObtainedAt: time.Now()}
	if _, err := c.Settings.PutCredential(c.cred); err != nil {
		return Token{}, err
	}
	return token, nil
}

func (c *Client) authorize(ctx context.Context, req *http.Request) error {
	c.resolve()
	if c.mode == AuthClient && c.Exchange == "" {
		req.SetBasicAuth(c.Settings.Get(KeyClientID), c.Settings.Get(KeyClientSecret))
		return nil
	}
	if c.token == "" || time.Now().After(c.expiry) {
		token, err := c.GetToken(ctx)
		if err != nil {
			return err
		}
		c.issued, c.token = token, token.AccessToken
		c.expiry = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).Add(-tokenSkew)
		if token.ExpiresIn == 0 {
			c.expiry = time.Now().Add(time.Hour)
		}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return nil
}

func (c *Client) UserAgent() string {
	version := "dev"
	if c.Version != nil {
		version = c.Version()
	}
	if stream.Machine() {
		return c.name() + "/" + version + " (mcp)"
	}
	return c.name() + "/" + version
}

func (c *Client) headers(req *http.Request) {
	req.Header.Set("User-Agent", c.UserAgent())
	if slug := c.Settings.Get(KeyAppSlug); slug != "" && c.AppHeader != "" {
		req.Header.Set(c.AppHeader, slug)
	}
}

func (c *Client) Send(cmd *cobra.Command, method, path string, query url.Values, contentType string, body []byte) ([]byte, error) {
	if c.Endpoint() == "" {
		return nil, Unauthenticated("no endpoint in config "+c.Settings.Loaded.Config, "Run '"+c.name()+" config set endpoint=https://...'.")
	}
	target := c.Endpoint() + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	ctx := context.Background()
	live := false
	if cmd != nil && cmd.Context() != nil {
		ctx = cmd.Context()
	}
	if cmd != nil {
		live, _ = cmd.Flags().GetBool(FlagStream)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		req.Body, req.GetBody, req.ContentLength = http.NoBody, nil, 0
	} else if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	accept := respond.MIMEEventStream
	if (method == http.MethodGet || method == http.MethodHead) && !live {
		accept = ContentTypeJSON
	}
	req.Header.Set("Accept", accept)
	c.headers(req)
	if err := c.authorize(ctx, req); err != nil {
		return nil, err
	}
	report := stream.Report(os.Stderr)
	if cmd != nil {
		report = stream.Report(cmd.ErrOrStderr())
	}
	client := c.http()
	if live {
		unbounded := *client
		unbounded.Timeout = 0
		client = &unbounded
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, &Error{Code: ExitFailure, Message: c.Endpoint() + " is unreachable", Cause: err}
	}
	defer res.Body.Close()
	if live && (res.StatusCode != http.StatusUnauthorized || !c.reused) {
		return nil, c.frames(cmd, method, path, res)
	}
	out, err := stream.Read(res, report)
	if err != nil {
		return nil, err
	}
	if out.Status == http.StatusUnauthorized && c.reused {
		c.reused, c.rejected, c.token = false, c.token, ""
		return c.Send(cmd, method, path, query, contentType, body)
	}
	if out.Status >= http.StatusMultipleChoices {
		return nil, c.failure(method, path, out.Status, out.Body)
	}
	return out.Body, nil
}

type frame struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

func (c *Client) frames(cmd *cobra.Command, method, path string, res *http.Response) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if res.StatusCode >= http.StatusMultipleChoices {
		raw, _ := io.ReadAll(res.Body)
		return c.failure(method, path, res.StatusCode, raw)
	}
	if !strings.HasPrefix(res.Header.Get("Content-Type"), respond.MIMEEventStream) {
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			return err
		}
		return c.output().Print(out, errOut, raw)
	}
	current := frame{Event: "message"}
	flush := func() error {
		defer func() { current = frame{Event: "message"} }()
		if len(current.Data) == 0 {
			return nil
		}
		if current.Event == "result" {
			var ended struct {
				Status int             `json:"status"`
				Body   json.RawMessage `json:"body"`
			}
			if json.Unmarshal(current.Data, &ended) == nil && ended.Status >= http.StatusMultipleChoices {
				return c.failure(method, path, ended.Status, ended.Body)
			}
		}
		if c.output().Raw {
			line, err := json.Marshal(current)
			if err != nil {
				return err
			}
			_, err = out.Write(append(line, '\n'))
			return err
		}
		shown := StyleFor(out)
		if _, err := fmt.Fprintln(out, shown.Bold(shown.Accent("event: "+current.Event))); err != nil {
			return err
		}
		return c.output().Print(out, errOut, current.Data)
	}
	lines := stream.Scanner(res)
	for lines.Scan() {
		line := lines.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "event:"):
			current.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if len(current.Data) > 0 {
				current.Data = append(current.Data, '\n')
			}
			current.Data = append(current.Data, strings.TrimSpace(strings.TrimPrefix(line, "data:"))...)
		}
	}
	if err := lines.Err(); err != nil && res.Request.Context().Err() == nil {
		return err
	}
	return flush()
}

func (c *Client) do(req *http.Request) ([]byte, error) {
	res, err := c.http().Do(req)
	if err != nil {
		return nil, &Error{Code: ExitFailure, Message: c.Endpoint() + " is unreachable", Cause: err}
	}
	defer res.Body.Close()
	out, err := stream.Read(res, nil)
	if err != nil {
		return nil, err
	}
	if out.Status >= http.StatusMultipleChoices {
		return nil, c.failure(req.Method, req.URL.Path, out.Status, out.Body)
	}
	return out.Body, nil
}

func (c *Client) failure(method, path string, status int, body []byte) error {
	detail := strings.TrimSpace(string(body))
	var wrapped clientEnvelope
	if json.Unmarshal(body, &wrapped) == nil && wrapped.Error != nil && wrapped.Error.Message != "" {
		detail = wrapped.Error.Message
	}
	if detail == "" {
		detail = "no response body"
	}
	message := fmt.Sprintf("%s %s: %d %s %s", method, path, status, http.StatusText(status), detail)
	switch {
	case status == http.StatusUnauthorized:
		return Unauthenticated(message, "Run '"+c.name()+" login', or check "+c.Settings.Prefix+"CLIENT_ID and "+c.Settings.Prefix+"CLIENT_SECRET.")
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return Rejected(message, "")
	case status == http.StatusForbidden:
		return Refused(message, "Run '"+c.name()+" status' to see the identity in use.")
	case status < http.StatusInternalServerError:
		return Refused(message, "")
	}
	return &Error{Code: ExitFailure, Message: message}
}

func (c *Client) status(ctx context.Context) (serviceStatus, error) {
	c.probe.Do(func() {
		c.probed = errors.New("no endpoint in config " + c.Settings.Loaded.Config)
		if c.Endpoint() == "" {
			return
		}
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(probe, http.MethodGet, c.Endpoint()+PathStatus, nil)
		if err != nil {
			c.probed = err
			return
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.UserAgent())
		res, err := c.http().Do(req)
		if err != nil {
			c.probed = err
			return
		}
		defer res.Body.Close()
		var wrapped struct {
			Data serviceStatus `json:"data"`
		}
		if res.StatusCode != http.StatusOK {
			c.probed = errors.New("GET " + PathStatus + ": " + res.Status)
			return
		}
		if err := json.NewDecoder(res.Body).Decode(&wrapped); err != nil {
			c.probed = err
			return
		}
		c.service, c.probed = wrapped.Data, nil
	})
	return c.service, c.probed
}

func (c *Client) ServesMCP(ctx context.Context) bool {
	status, err := c.status(ctx)
	return err == nil && slices.Contains(status.Endpoints, mcp.MountPath)
}

func (c *Client) GetServerVersion(ctx context.Context) (string, error) {
	status, err := c.status(ctx)
	if err != nil {
		return "", err
	}
	return status.Version, nil
}

func (c *Client) Remote(ctx context.Context) (string, string, error) {
	c.resolve()
	if c.mode == AuthNone || (c.mode == AuthClient && c.Exchange == "") || !c.ServesMCP(ctx) {
		return "", "", nil
	}
	token, err := c.GetToken(ctx)
	if err != nil {
		return "", "", err
	}
	return c.Endpoint() + mcp.MountPath, token.AccessToken, nil
}

func (c *Client) Mount(root *cobra.Command) error {
	if err := c.Validate(); err != nil {
		return err
	}
	root.AddCommand(c.LoginCommand(), c.LogoutCommand(), c.StatusCommand(), c.TokenCommand(), APICommand(c.Send, c.output()))
	if Bridge == nil {
		Bridge = c.Remote
	}
	return nil
}

func (c *Client) Validate() error {
	if c.Settings == nil {
		return errors.New("cli.Client needs Settings")
	}
	var missing []string
	for _, field := range []struct{ name, value string }{
		{"LoginPath", c.LoginPath},
		{"TokenPath", c.TokenPath},
		{"RevokePath", c.RevokePath},
		{"IdentityPath", c.IdentityPath},
		{"AppHeader", c.AppHeader},
	} {
		if field.value == "" {
			missing = append(missing, field.name)
		}
	}
	if len(missing) > 0 {
		return errors.New("cli.Client needs " + strings.Join(missing, ", ") + ": the cli names its own login, token, revoke and identity paths and its app header")
	}
	for _, path := range []string{c.LoginPath, c.TokenPath, c.RevokePath, c.IdentityPath, firstSet(c.Exchange, "/")} {
		if !strings.HasPrefix(path, "/") {
			return errors.New("cli.Client paths start with a slash, got " + path)
		}
	}
	return nil
}
