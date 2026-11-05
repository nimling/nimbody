package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/nimling/nimbody/stream"
	"github.com/spf13/cobra"
)

const (
	loginTimeout  = 10 * time.Minute
	logoutTimeout = 30 * time.Second
	loopbackHost  = "127.0.0.1:0"
	loopbackPath  = "/callback"
)

type cliTokens struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresIn int    `json:"refresh_expires_in"`
}

type oauthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

type identity struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
}

type Status struct {
	Endpoint       string          `json:"endpoint"`
	AppSlug        string          `json:"app_slug,omitempty"`
	Auth           string          `json:"auth"`
	ClientID       string          `json:"client_id,omitempty"`
	CredentialFile string          `json:"credential_file,omitempty"`
	Store          string          `json:"store,omitempty"`
	Subject        string          `json:"subject,omitempty"`
	TokenType      string          `json:"token_type,omitempty"`
	ExpiresIn      int             `json:"access_expires_in,omitempty"`
	Scope          string          `json:"scope,omitempty"`
	Identity       json.RawMessage `json:"identity,omitempty"`
	Health         json.RawMessage `json:"health,omitempty"`
}

var OpenBrowser = func(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}

func GetVerifier() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func GetChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (c *Client) GetLoginURL(challenge, redirect, state string) string {
	query := url.Values{"cli_challenge": {challenge}}
	if redirect != "" {
		query.Set("cli_redirect", redirect)
		query.Set("cli_state", state)
	}
	return c.Endpoint() + firstSet(c.LoginPath, PathLogin) + "?" + query.Encode()
}

func (c *Client) page(title, message string, failed bool) []byte {
	page, _ := stream.GetLoginPage(stream.LoginPage{Name: c.name(), Product: firstSet(c.Product, c.name()), Title: title, Message: message, Failed: failed})
	return page
}

func (c *Client) loopbackCode(ctx context.Context, listener net.Listener, state string) (string, error) {
	codes := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(loopbackPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		code := r.URL.Query().Get("code")
		if code == "" || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(state)) != 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(c.page("Not signed in", "This sign in did not come from your terminal. Start it again with "+c.name()+" login.", true))
			return
		}
		_, _ = w.Write(c.page("Signed in", c.name()+" has your sign in. Return to your terminal; you can close this tab.", false))
		select {
		case codes <- code:
		default:
		}
	})
	loopback := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = loopback.Serve(listener) }()
	defer loopback.Close()
	select {
	case code := <-codes:
		return code, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (c *Client) loginCode(cmd *cobra.Command, challenge string, noBrowser bool) (string, error) {
	errOut := cmd.ErrOrStderr()
	style := StyleFor(errOut)
	if !noBrowser {
		if listener, err := net.Listen("tcp", loopbackHost); err == nil {
			state, err := GetVerifier()
			if err != nil {
				listener.Close()
				return "", err
			}
			target := c.GetLoginURL(challenge, "http://"+listener.Addr().String()+loopbackPath, state)
			fmt.Fprintln(errOut, "sign in  "+target)
			if OpenBrowser(target) != nil {
				fmt.Fprintln(errOut, style.Dim("no browser opened here, open the address yourself"))
			}
			fmt.Fprintln(errOut, style.Dim("waiting for the sign in in the browser"))
			ctx, cancel := context.WithTimeout(cmd.Context(), loginTimeout)
			defer cancel()
			code, err := c.loopbackCode(ctx, listener, state)
			if errors.Is(err, context.DeadlineExceeded) {
				return "", Unauthenticated(fmt.Sprintf("the sign in was not completed within %s", loginTimeout), "Run '"+c.name()+" login' again.")
			}
			return code, err
		}
	}
	fmt.Fprintln(errOut, "sign in  "+c.GetLoginURL(challenge, "", ""))
	fmt.Fprint(errOut, "paste the code the page shows: ")
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	code := strings.TrimSpace(line)
	if code == "" {
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return "", Unauthenticated("no code was pasted", "Run '"+c.name()+" login' again.")
	}
	return code, nil
}

func (c *Client) postForm(ctx context.Context, endpoint, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.UserAgent())
	res, err := c.http().Do(req)
	if err != nil {
		return &Error{Code: ExitFailure, Message: endpoint + " is unreachable", Cause: err}
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode >= http.StatusMultipleChoices {
		var failed oauthError
		if json.Unmarshal(raw, &failed) == nil && failed.Code != "" {
			if failed.Description != "" {
				return fmt.Errorf("POST %s: %s, %s", path, failed.Code, failed.Description)
			}
			return fmt.Errorf("POST %s: %s", path, failed.Code)
		}
		return fmt.Errorf("POST %s: %s", path, res.Status)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func accessClientID(accessToken string) string {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		ClientID string `json:"client_id"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.ClientID
}

func (c *Client) LoginCommand() *cobra.Command {
	var noBrowser bool
	cmd := &cobra.Command{
		Use:     "login",
		Short:   "Sign in through the browser and act as yourself on this endpoint across invocations",
		Long:    "Sign in through the service's own login in the browser. The cli keeps a PKCE verifier, takes the code back on a loopback address, or pasted with --no-browser, exchanges it at the service for a session of its own, and stores it in the keychain, or in a 0600 file under ~/." + c.name() + " where no keychain exists. A stored login takes precedence over " + c.Settings.Prefix + "CLIENT_ID and " + c.Settings.Prefix + "CLIENT_SECRET until " + c.name() + " logout.",
		Example: c.name() + " login --no-browser",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if c.Endpoint() == "" {
				return Unauthenticated("no endpoint in config "+c.Settings.Loaded.Config, "Run '"+c.name()+" config set endpoint=https://...'.")
			}
			verifier, err := GetVerifier()
			if err != nil {
				return err
			}
			code, err := c.loginCode(cmd, GetChallenge(verifier), noBrowser)
			if err != nil {
				return err
			}
			var tokens cliTokens
			if err := c.postForm(cmd.Context(), c.Endpoint(), PathCliToken, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}}, &tokens); err != nil {
				return Unauthenticated(err.Error(), "Run '"+c.name()+" login' again.")
			}
			c.resolved, c.mode, c.token = true, AuthLogin, ""
			c.cred = Credential{Endpoint: c.Endpoint(), AppSlug: accessClientID(tokens.AccessToken), RefreshToken: tokens.RefreshToken, AccessToken: tokens.AccessToken, AccessExpiresAt: time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second), ObtainedAt: time.Now()}
			if _, err := c.Settings.PutCredential(c.cred); err != nil {
				return err
			}
			errOut := cmd.ErrOrStderr()
			if body, err := c.Send(cmd, http.MethodGet, firstSet(c.IdentityPath, PathIdentity), nil, "", nil); err == nil {
				var who identity
				var wrapped struct {
					Data json.RawMessage `json:"data"`
				}
				if json.Unmarshal(body, &wrapped) == nil && json.Unmarshal(wrapped.Data, &who) == nil && who.UserID != "" {
					c.cred.Subject = who.UserID
					fmt.Fprintln(errOut, "signed in as "+StyleFor(errOut).Accent(firstSet(who.Email, who.UserID)))
				}
			}
			path, err := c.Settings.PutCredential(c.cred)
			if err != nil {
				return err
			}
			fmt.Fprintln(errOut, "wrote "+path)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the address and paste the code instead of opening a browser")
	return cmd
}

func (c *Client) LogoutCommand() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:     "logout",
		Short:   "End the stored login for this endpoint, or for every endpoint with --all",
		Example: c.name() + " logout --all",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var creds []Credential
			if all {
				stored, err := c.Settings.GetCredentials()
				if err != nil {
					return err
				}
				creds = stored
			} else {
				if c.Endpoint() == "" {
					return Unauthenticated("no endpoint in config "+c.Settings.Loaded.Config, "Run '"+c.name()+" logout --all', or pick a config with --config.")
				}
				cred, err := c.Settings.GetCredential(c.Endpoint(), "")
				if errors.Is(err, ErrNoCredential) {
					cmd.PrintErrln("no stored login for " + c.Endpoint())
					return nil
				}
				if err != nil {
					return err
				}
				creds = append(creds, cred)
			}
			if len(creds) == 0 {
				cmd.PrintErrln("no stored logins")
				return nil
			}
			errOut := cmd.ErrOrStderr()
			for _, cred := range creds {
				ctx, cancel := context.WithTimeout(cmd.Context(), logoutTimeout)
				err := c.postForm(ctx, cred.Endpoint, PathCliRevoke, url.Values{"token": {cred.RefreshToken}, "token_type_hint": {"refresh_token"}}, nil)
				cancel()
				if err != nil {
					fmt.Fprintln(errOut, StyleFor(errOut).Warn("the session at "+cred.Endpoint+" was not ended: "+err.Error()))
				} else {
					fmt.Fprintln(errOut, "ended the session at "+cred.Endpoint)
				}
				if err := c.Settings.DeleteCredential(cred); err != nil {
					return err
				}
				fmt.Fprintln(errOut, "removed "+cred.Path)
			}
			c.resolved = false
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "remove the stored login for every endpoint")
	return cmd
}

func (c *Client) StatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Short:   "Report the endpoint, the identity in use, its token and the server health",
		Example: c.name() + " status --json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			report := Status{Endpoint: c.Endpoint(), AppSlug: c.Settings.Get(KeyAppSlug), Auth: c.Mode()}
			switch report.Auth {
			case AuthNone:
				return c.unauthenticated()
			case AuthClient:
				report.ClientID = c.Settings.Get(KeyClientID)
			case AuthLogin:
				report.CredentialFile, report.Store, report.Subject = c.cred.Path, c.cred.Store, c.cred.Subject
			}
			if report.Auth != AuthClient || c.Exchange != "" {
				token, err := c.GetToken(cmd.Context())
				if err != nil {
					return err
				}
				report.TokenType, report.ExpiresIn, report.Scope = token.TokenType, token.ExpiresIn, token.Scope
			}
			envelopeData := func(body []byte) json.RawMessage {
				var wrapped struct {
					Data json.RawMessage `json:"data"`
				}
				if json.Unmarshal(body, &wrapped) == nil && len(wrapped.Data) > 0 {
					return wrapped.Data
				}
				return body
			}
			health, err := c.Send(cmd, http.MethodGet, firstSet(c.HealthPath, PathHealth), nil, "", nil)
			if err != nil {
				return err
			}
			report.Health = envelopeData(health)
			if report.Auth != AuthClient {
				if who, err := c.Send(cmd, http.MethodGet, firstSet(c.IdentityPath, PathIdentity), nil, "", nil); err == nil {
					report.Identity = envelopeData(who)
				}
			}
			return c.output().PrintValue(cmd.OutOrStdout(), report)
		},
	}
}

func (c *Client) TokenCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "token",
		Short:   "Print an access token for the identity in use",
		Example: c.name() + " token --jq .access_token",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			token, err := c.GetToken(cmd.Context())
			if err != nil {
				return err
			}
			return c.output().PrintValue(cmd.OutOrStdout(), token)
		},
	}
}

func (c *Client) output() *Output {
	if c.Output == nil {
		c.Output = &Output{}
	}
	return c.Output
}
