package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

func newRegistryClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil}, Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("registry redirects forbidden") },
	}
}

func (d *distribution) registryGet(ctx context.Context, path, token string) ([]byte, http.Header, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://ghcr.io"+path, nil)
	if err != nil {
		return nil, nil, 0, errors.New("invalid registry request")
	}
	if token == "" {
		if d.getenv("GH_TOKEN") == "" || d.getenv("GITHUB_ACTOR") == "" {
			return nil, nil, 0, errors.New("workflow registry credentials required")
		}
		req.SetBasicAuth(d.getenv("GITHUB_ACTOR"), d.getenv("GH_TOKEN"))
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json")
	}
	resp, err := d.registry.Do(req)
	if err != nil {
		return nil, nil, 0, errors.New("registry lookup failed; state is uncertain")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if err != nil || len(data) > 4<<20 {
		return nil, nil, 0, errors.New("invalid registry response")
	}
	return data, resp.Header, resp.StatusCode, nil
}

// A CLI inspection failure is not evidence of absence. Only an authenticated
// manifest lookup with an explicit MANIFEST_UNKNOWN response permits creation.
func (d *distribution) lookup(ctx context.Context, tag string) (string, error) {
	data, _, status, err := d.registryGet(ctx, "/token?service=ghcr.io&scope=repository:yaasha/near-e2ee-proxy:pull", "")
	if err != nil {
		return "", err
	}
	var auth struct{ Token string }
	if status != 200 || json.Unmarshal(data, &auth) != nil || auth.Token == "" {
		return "", errors.New("registry authentication failed; refusing publication")
	}
	data, headers, status, err := d.registryGet(ctx, "/v2/yaasha/near-e2ee-proxy/manifests/"+tag, auth.Token)
	if err != nil {
		return "", err
	}
	if status == 404 {
		var result struct{ Errors []struct{ Code string } }
		if json.Unmarshal(data, &result) == nil && len(result.Errors) == 1 && result.Errors[0].Code == "MANIFEST_UNKNOWN" {
			return "", nil
		}
	}
	if status != 200 || !digestPattern.MatchString(headers.Get("Docker-Content-Digest")) {
		return "", errors.New("registry tag state is uncertain; refusing publication")
	}
	return headers.Get("Docker-Content-Digest"), nil
}
