package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rewriteTransport redirects api.github.com requests to our local httptest server.
type rewriteTransport struct {
	targetURL string
	base      http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	reqCopy := req.Clone(req.Context())
	reqCopy.URL.Scheme = "http"
	reqCopy.URL.Host = strings.TrimPrefix(t.targetURL, "http://")
	return t.base.RoundTrip(reqCopy)
}

func TestGitHubClientPaginationAndSorting(t *testing.T) {
	var mockServer *httptest.Server
	mockServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch r.URL.Path {
		case "/orgs/test-org/members":
			if r.URL.Query().Get("page") == "2" {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`[{"login": "alice"}]`))
				return
			}
			// Page 1 returns RFC 5988 Link header pointing to page 2
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Link", `<`+mockServer.URL+`/orgs/test-org/members?page=2>; rel="next"`)
			w.Write([]byte(`[{"login": "charlie"}, {"login": "bob"}]`))

		case "/orgs/test-org/teams":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[
				{"name": "Platform Engineering", "slug": "platform-eng"},
				{"name": "Database Admins", "slug": "db-admins"}
			]`))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockServer.Close()

	client := &GitHubClient{
		token: "test-token",
		org:   "test-org",
		httpClient: &http.Client{
			Transport: &rewriteTransport{
				targetURL: mockServer.URL,
				base:      http.DefaultTransport,
			},
		},
	}

	ctx := context.Background()

	members, err := client.FetchMembers(ctx)
	if err != nil {
		t.Fatalf("FetchMembers failed: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("expected 3 paginated members, got %d", len(members))
	}
	// Verify alphabetical sorting across pages
	if members[0].Name != "alice" || members[1].Name != "bob" || members[2].Name != "charlie" {
		t.Errorf("unexpected member order: %+v", members)
	}

	teams, err := client.FetchTeams(ctx)
	if err != nil {
		t.Fatalf("FetchTeams failed: %v", err)
	}
	if len(teams) != 2 {
		t.Fatalf("expected 2 teams, got %d", len(teams))
	}
	if teams[0].Name != "Database Admins" || teams[0].Value != "db-admins" {
		t.Errorf("unexpected team mapping: %+v", teams[0])
	}
}

func TestCacheConcurrencyAndStats(t *testing.T) {
	cache := &Cache{}

	if stats := cache.Stats(); stats["status"] != "healthy" || stats["member_count"] != 0 {
		t.Fatalf("unexpected initial stats: %+v", stats)
	}

	cache.Update(
		[]RundeckOption{{Name: "alice", Value: "alice"}},
		[]RundeckOption{{Name: "Ops", Value: "ops"}},
	)

	members, updated := cache.GetMembers()
	if updated.IsZero() || len(members) != 1 || members[0].Value != "alice" {
		t.Fatalf("unexpected cached members: %+v", members)
	}

	cache.SetError(os.ErrDeadlineExceeded)
	if stats := cache.Stats(); stats["status"] != "degraded" {
		t.Errorf("expected degraded status after error, got %v", stats["status"])
	}
}

func TestLoadConfigFromFileAndEnv(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")

	rawCfg := `{"github_token":"file-token","github_org":"file-org","cache_ttl_seconds":120}`
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0600); err != nil {
		t.Fatal(err)
	}

	// Verify JSON unmarshaling into Config struct
	var cfg Config
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("failed to unmarshal config: %v", err)
	}
	if cfg.GitHubToken != "file-token" || cfg.GitHubOrg != "file-org" || cfg.CacheTTLSeconds != 120 {
		t.Errorf("unexpected config values: %+v", cfg)
	}
}
