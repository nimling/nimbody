package registry

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

const GitHubAPI = "https://api.github.com"

type Release struct {
	Repository string
	Token      func(ctx context.Context) (string, error)
	API        string
	Client     *http.Client
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

type releaseBody struct {
	Assets []asset `json:"assets"`
}

type release struct {
	assets    map[string]asset
	sums      map[string]string
	platforms []Platform
}

func (r Release) get(ctx context.Context, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if r.Token != nil {
		token, err := r.Token(ctx)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req)
}

func (r Release) open(ctx context.Context, item asset) (io.ReadCloser, error) {
	res, err := r.get(ctx, item.URL, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("asset %s answered %d", item.Name, res.StatusCode)
	}
	return res.Body, nil
}

func (s *service) release(ctx context.Context) (*release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil {
		return s.cached, nil
	}
	r := s.registry.Release
	if r.Repository == "" || s.version == "" {
		return nil, nil
	}
	api := r.API
	if api == "" {
		api = GitHubAPI
	}
	res, err := r.get(ctx, strings.TrimSuffix(api, "/")+"/repos/"+r.Repository+"/releases/tags/v"+s.version, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release v%s of %s answered %d", s.version, r.Repository, res.StatusCode)
	}
	var body releaseBody
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("release v%s of %s: %w", s.version, r.Repository, err)
	}
	out := &release{assets: map[string]asset{}, sums: map[string]string{}}
	for _, item := range body.Assets {
		out.assets[item.Name] = item
	}
	files := GetFiles(s.registry.Type, s.version, "", "")
	if item, ok := out.assets[files.Sums]; ok {
		raw, err := r.open(ctx, item)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(raw)
		raw.Close()
		if err != nil {
			return nil, err
		}
		out.sums, out.platforms = parseSums(data, s.registry.Type, s.version, out.assets)
	}
	s.cached = out
	return out, nil
}

func parseSums(data []byte, providerType, version string, assets map[string]asset) (map[string]string, []Platform) {
	sums := map[string]string{}
	var platforms []Platform
	prefix := "terraform-provider-" + providerType + "_" + version + "_"
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".zip") {
			continue
		}
		if _, ok := assets[name]; !ok {
			continue
		}
		platform := strings.SplitN(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".zip"), "_", 2)
		if len(platform) != 2 {
			continue
		}
		sums[name] = fields[0]
		platforms = append(platforms, Platform{OS: platform[0], Arch: platform[1]})
	}
	sort.Slice(platforms, func(i, j int) bool {
		if platforms[i].OS != platforms[j].OS {
			return platforms[i].OS < platforms[j].OS
		}
		return platforms[i].Arch < platforms[j].Arch
	})
	return sums, platforms
}
