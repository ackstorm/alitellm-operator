// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jwt"
)

// providerTypeVertex is the spec.type literal for Gemini on Vertex AI.
const providerTypeVertex = "vertex"

// vertexTokenURLKey is the SetTestBaseURL key for the OAuth token endpoint.
// Production always uses googleTokenURL: the key's token_uri is IGNORED so a
// crafted key cannot make the operator POST signed assertions to an
// arbitrary host.
const (
	vertexTokenURLKey = "vertex-token"                        // #nosec G101 -- test-seam map key, not a credential
	googleTokenURL    = "https://oauth2.googleapis.com/token" // #nosec G101 -- public OAuth endpoint, not a credential
	vertexScope       = "https://www.googleapis.com/auth/cloud-platform"
)

// vertexLocationRe gates spec.region before it is spliced into a hostname
// (defense in depth; the CRD carries the same pattern).
var vertexLocationRe = regexp.MustCompile(`^[a-z0-9-]+$`)

var (
	errVertexLocation = errors.New("vertex: spec.region must be a Vertex location (e.g. eu, us, global, europe-west1)")
	errVertexKey      = errors.New("vertex: VERTEX_CREDENTIALS is not a service-account JSON key (needs client_email, project_id and a PEM PKCS#8/PKCS#1 private_key)")
)

// vertexKey is the subset of a service-account JSON key the provider reads.
type vertexKey struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	ProjectID   string `json:"project_id"`
}

type vertexProvider struct {
	key        vertexKey
	location   string
	httpClient *http.Client
}

func newVertexImpl(_ context.Context, cfg ProviderConfig) (Provider, error) {
	if !vertexLocationRe.MatchString(cfg.Region) {
		return nil, errVertexLocation
	}
	if cfg.HTTPClient == nil {
		return nil, errNilHTTPClient
	}
	var k vertexKey
	// The decode error is dropped: json syntax errors can quote key bytes.
	if json.Unmarshal(cfg.VertexCredentials, &k) != nil ||
		k.ClientEmail == "" || k.ProjectID == "" || !parsablePrivateKey(k.PrivateKey) {
		return nil, errVertexKey
	}
	return &vertexProvider{key: k, location: cfg.Region, httpClient: cfg.HTTPClient}, nil
}

func (p *vertexProvider) Type() string { return providerTypeVertex }

// parsablePrivateKey validates up front what jwt would otherwise reject at
// token time: jwt wraps its errors with %v, so a bad key and a token
// endpoint outage would be indistinguishable there.
func parsablePrivateKey(pemKey string) bool {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return false
	}
	if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return true
	}
	_, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	return err == nil
}

// vertexBaseURL maps a Vertex location to its API host:
// global → aiplatform.googleapis.com; multi-region eu/us → the regional
// endpoint aiplatform.<loc>.rep.googleapis.com; else <loc>-aiplatform.
func vertexBaseURL(location string) string {
	switch location {
	case "global":
		return "https://aiplatform.googleapis.com"
	case "eu", "us":
		return "https://aiplatform." + location + ".rep.googleapis.com"
	}
	return "https://" + location + "-aiplatform.googleapis.com"
}

// token exchanges a self-signed JWT for an access token (one per List;
// refresh cadence is minutes, so no caching).
func (p *vertexProvider) token(ctx context.Context) (string, error) {
	tokenURL := baseURLFor(vertexTokenURLKey, "")
	if tokenURL == "" {
		tokenURL = googleTokenURL
	}
	conf := &jwt.Config{
		Email:      p.key.ClientEmail,
		PrivateKey: []byte(p.key.PrivateKey),
		Scopes:     []string{vertexScope},
		TokenURL:   tokenURL,
	}
	tok, err := conf.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, p.httpClient)).Token()
	if err != nil {
		// RetrieveError.Error() embeds the response body: never surface it.
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			status := 0
			if re.Response != nil {
				status = re.Response.StatusCode
			}
			return "", &ProviderAuthError{
				Provider: providerTypeVertex,
				Cause:    fmt.Errorf("token exchange: status %d %s", status, re.ErrorCode),
			}
		}
		// The key was parsed in the constructor, so what is left is transport
		// or token-response decoding (jwt wraps with %v: no *url.Error to
		// match). Text carries the pinned token URL at most — no key bytes.
		return "", fmt.Errorf("vertex: token exchange: %v", err)
	}
	return tok.AccessToken, nil
}

// List pages GET <base>/v1beta1/publishers/google/models. The listing is
// location-aware (an EU endpoint returns only EU-served models). Each
// candidate carries vertex_location / vertex_project for the child.
//
// Error classification mirrors gemini.List: 401/403 → *ProviderAuthError
// (no body); other ≥400 → plain error with status; 4MB page cap; REL-04
// drain+close per page; hard page cap refuses a truncated result.
func (p *vertexProvider) List(ctx context.Context) ([]Candidate, error) {
	const maxPages = 1000
	tok, err := p.token(ctx)
	if err != nil {
		return nil, err
	}
	base := baseURLFor(providerTypeVertex, "")
	if base == "" {
		base = vertexBaseURL(p.location)
	}
	base += "/v1beta1/publishers/google/models"
	params := map[string]string{"vertex_location": p.location, "vertex_project": p.key.ProjectID}

	var candidates []Candidate
	pageToken := ""
	for range maxPages {
		q := url.Values{"pageSize": {"100"}}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+q.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("vertex: build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("x-goog-user-project", p.key.ProjectID)
		req.Header.Set("Accept", "application/json")

		resp, err := p.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("vertex: transport error: %w", err)
		}
		next, err := func() (string, error) {
			defer drainAndClose(resp.Body)
			switch {
			case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
				return "", &ProviderAuthError{Provider: providerTypeVertex, Cause: fmt.Errorf("status %d", resp.StatusCode)}
			case resp.StatusCode >= 400:
				return "", fmt.Errorf("vertex: list models: status %d", resp.StatusCode)
			}
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			if rerr != nil {
				return "", fmt.Errorf("vertex: read response: %w", rerr)
			}
			var decoded struct {
				PublisherModels []struct {
					Name string `json:"name"`
				} `json:"publisherModels"`
				NextPageToken string `json:"nextPageToken"`
			}
			if jerr := json.Unmarshal(body, &decoded); jerr != nil {
				return "", fmt.Errorf("vertex: decode response: %w", jerr)
			}
			for _, m := range decoded.PublisherModels {
				// "publishers/google/models/gemini-3.8-flash" → "gemini-3.8-flash"
				id := path.Base(m.Name)
				// Gemini only (Imagen, Veo, Chirp, text-embedding are not chat
				// models); embeddings dropped like Bedrock's EMBEDDING filter.
				if !strings.HasPrefix(id, "gemini-") || strings.Contains(id, "embedding") {
					continue
				}
				candidates = append(candidates, Candidate{ID: id, Params: params})
			}
			return decoded.NextPageToken, nil
		}()
		if err != nil {
			return nil, err
		}
		if next == "" {
			return candidates, nil
		}
		pageToken = next
	}
	return nil, fmt.Errorf("vertex: model list exceeded %d pages; refusing truncated result", maxPages)
}
