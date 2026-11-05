package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

const (
	credentialSuffix       = ".cred"
	credentialClientSuffix = ".client"
	credentialLock         = ".lock"
	credentialLockWait     = 30 * time.Second
	credentialLockPoll     = 100 * time.Millisecond
	StoreKeychain          = "keychain"
	StoreFile              = "file"
)

var ErrNoCredential = errors.New("no stored login")

type Credential struct {
	Endpoint        string    `json:"endpoint"`
	AppSlug         string    `json:"app_slug,omitempty"`
	ClientID        string    `json:"client_id,omitempty"`
	RefreshToken    string    `json:"-"`
	AccessToken     string    `json:"-"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
	Subject         string    `json:"subject,omitempty"`
	ObtainedAt      time.Time `json:"obtained_at"`
	Path            string    `json:"path"`
	Store           string    `json:"store"`
}

type Keyring interface {
	Get(service, account string) (string, error)
	Put(service, account, secret string) error
	Delete(service, account string) error
}

var Keychain Keyring = systemKeychain()

type keychainSecret struct {
	RefreshToken string `json:"refresh_token,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
}

func (s *Settings) credentialPath(endpoint, clientID string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return "", Rejected("cannot read a host out of the endpoint "+endpoint, "Set it with '"+s.Name+" config set endpoint=https://...'.")
	}
	dir, err := s.Home()
	if err != nil {
		return "", err
	}
	replacer := strings.NewReplacer(":", "_", "/", "_", string(filepath.Separator), "_")
	name := replacer.Replace(parsed.Host)
	if clientID != "" {
		return filepath.Join(dir, name+"_"+replacer.Replace(clientID)+credentialClientSuffix), nil
	}
	return filepath.Join(dir, name+credentialSuffix), nil
}

func (s *Settings) GetCredential(endpoint, clientID string) (Credential, error) {
	path, err := s.credentialPath(endpoint, clientID)
	if err != nil {
		return Credential{}, err
	}
	if _, err := os.Stat(path); err != nil {
		return Credential{}, ErrNoCredential
	}
	cred, err := s.readCredential(path)
	if err != nil {
		return Credential{}, err
	}
	token := cred.RefreshToken
	if clientID != "" {
		token = cred.AccessToken
	}
	if cred.ClientID != clientID || token == "" {
		return Credential{}, ErrNoCredential
	}
	return cred, nil
}

func (s *Settings) GetCredentials() ([]Credential, error) {
	dir, err := s.Home()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var creds []Credential
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), credentialSuffix) {
			continue
		}
		cred, err := s.readCredential(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		creds = append(creds, cred)
	}
	return creds, nil
}

func (s *Settings) PutCredential(cred Credential) (string, error) {
	path, err := s.credentialPath(cred.Endpoint, cred.ClientID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	values := map[string]string{
		s.Prefix + "ENDPOINT":    cred.Endpoint,
		s.Prefix + "APP_SLUG":    cred.AppSlug,
		s.Prefix + "SUBJECT":     cred.Subject,
		s.Prefix + "OBTAINED_AT": cred.ObtainedAt.UTC().Format(time.RFC3339),
	}
	if cred.ClientID != "" {
		values[s.Prefix+"CLIENT_ID"] = cred.ClientID
	}
	if !cred.AccessExpiresAt.IsZero() {
		values[s.Prefix+"ACCESS_EXPIRES_AT"] = cred.AccessExpiresAt.UTC().Format(time.RFC3339)
	}
	secret, _ := json.Marshal(keychainSecret{RefreshToken: cred.RefreshToken, AccessToken: cred.AccessToken})
	if Keychain != nil && Keychain.Put(s.Name, filepath.Base(path), base64.RawURLEncoding.EncodeToString(secret)) == nil {
		values[s.Prefix+"STORE"] = StoreKeychain
	} else {
		values[s.Prefix+"REFRESH_TOKEN"] = cred.RefreshToken
		values[s.Prefix+"ACCESS_TOKEN"] = cred.AccessToken
	}
	if err := godotenv.Write(values, path); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Settings) DeleteCredential(cred Credential) error {
	if Keychain != nil && cred.Store == StoreKeychain {
		_ = Keychain.Delete(s.Name, filepath.Base(cred.Path))
	}
	err := os.Remove(cred.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Settings) LockCredential(endpoint, clientID string) (func(), error) {
	path, err := s.credentialPath(endpoint, clientID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock := path + credentialLock
	deadline := time.Now().Add(credentialLockWait)
	for {
		file, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			file.Close()
			return func() { os.Remove(lock) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if info, statErr := os.Stat(lock); statErr == nil && time.Since(info.ModTime()) > credentialLockWait {
			os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return nil, &Error{Code: ExitFailure, Message: "another " + s.Name + " holds " + lock, Hint: "Wait for it, or remove the file when no " + s.Name + " runs."}
		}
		time.Sleep(credentialLockPoll)
	}
}

func (s *Settings) readCredential(path string) (Credential, error) {
	values, err := godotenv.Read(path)
	if err != nil {
		return Credential{}, errors.New("read " + path + ": " + err.Error())
	}
	cred := Credential{
		Endpoint:     values[s.Prefix+"ENDPOINT"],
		AppSlug:      values[s.Prefix+"APP_SLUG"],
		ClientID:     values[s.Prefix+"CLIENT_ID"],
		RefreshToken: values[s.Prefix+"REFRESH_TOKEN"],
		AccessToken:  values[s.Prefix+"ACCESS_TOKEN"],
		Subject:      values[s.Prefix+"SUBJECT"],
		Path:         path,
		Store:        StoreFile,
	}
	if values[s.Prefix+"STORE"] == StoreKeychain {
		cred.Store = StoreKeychain
		if Keychain == nil {
			return cred, Unauthenticated("the login in "+path+" lives in a keychain this machine does not offer", "Run '"+s.Name+" login' again.")
		}
		encoded, err := Keychain.Get(s.Name, filepath.Base(path))
		if err != nil {
			return cred, Unauthenticated("the keychain holds no login for "+filepath.Base(path), "Run '"+s.Name+" login' again.")
		}
		var secret keychainSecret
		raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
		if err := json.Unmarshal(raw, &secret); err != nil {
			return cred, Unauthenticated("the keychain entry for "+filepath.Base(path)+" is not a login", "Run '"+s.Name+" login' again.")
		}
		cred.RefreshToken, cred.AccessToken = secret.RefreshToken, secret.AccessToken
	}
	if stamp, err := time.Parse(time.RFC3339, values[s.Prefix+"OBTAINED_AT"]); err == nil {
		cred.ObtainedAt = stamp
	}
	if stamp, err := time.Parse(time.RFC3339, values[s.Prefix+"ACCESS_EXPIRES_AT"]); err == nil {
		cred.AccessExpiresAt = stamp
	}
	return cred, nil
}

type securityKeychain struct{}

func (securityKeychain) Get(service, account string) (string, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", account, "-w").Output()
	return strings.TrimSpace(string(out)), err
}

func (securityKeychain) Put(service, account, secret string) error {
	put := exec.Command("security", "-i")
	put.Stdin = strings.NewReader("add-generic-password -U -s " + service + " -a " + account + " -w " + secret + "\n")
	if err := put.Run(); err != nil {
		return err
	}
	stored, err := securityKeychain{}.Get(service, account)
	if err != nil || stored != secret {
		return errors.New("the keychain did not keep " + account)
	}
	return nil
}

func (securityKeychain) Delete(service, account string) error {
	return exec.Command("security", "delete-generic-password", "-s", service, "-a", account).Run()
}

type secretTool struct{}

func (secretTool) Get(service, account string) (string, error) {
	out, err := exec.Command("secret-tool", "lookup", "service", service, "account", account).Output()
	return strings.TrimSpace(string(out)), err
}

func (secretTool) Put(service, account, secret string) error {
	put := exec.Command("secret-tool", "store", "--label="+service+" "+account, "service", service, "account", account)
	put.Stdin = strings.NewReader(secret)
	return put.Run()
}

func (secretTool) Delete(service, account string) error {
	return exec.Command("secret-tool", "clear", "service", service, "account", account).Run()
}

func systemKeychain() Keyring {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("security"); err == nil {
			return securityKeychain{}
		}
	case "linux":
		if _, err := exec.LookPath("secret-tool"); err == nil {
			return secretTool{}
		}
	}
	return nil
}
