package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Config holds the application configuration.
type Config struct {
	GitHubToken     string `json:"github_token"`
	GitHubOrg       string `json:"github_org"`
	ListenAddr      string `json:"listen_addr"`
	CacheTTLSeconds int    `json:"cache_ttl_seconds"`
}

// RundeckOption matches Rundeck's Remote URL option format:
// [{"name": "Display Label", "value": "actual_value"}]
type RundeckOption struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// GitHub API response structures
type ghMember struct {
	Login string `json:"login"`
}

type ghTeam struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// Cache stores the fetched options safely across concurrent HTTP requests.
type Cache struct {
	mu          sync.RWMutex
	members     []RundeckOption
	teams       []RundeckOption
	lastUpdated time.Time
	lastError   string
}

func (c *Cache) Update(members, teams []RundeckOption) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.members = members
	c.teams = teams
	c.lastUpdated = time.Now().UTC()
	c.lastError = ""
}

func (c *Cache) SetError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastError = err.Error()
}

func (c *Cache) GetMembers() ([]RundeckOption, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.members, c.lastUpdated
}

func (c *Cache) GetTeams() ([]RundeckOption, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.teams, c.lastUpdated
}

func (c *Cache) Stats() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	status := "healthy"
	if c.lastError != "" {
		status = "degraded"
	}
	return map[string]any{
		"status":       status,
		"member_count": len(c.members),
		"team_count":   len(c.teams),
		"last_updated": c.lastUpdated,
		"last_error":   c.lastError,
	}
}

// GitHubClient handles paginated requests to the GitHub REST API.
type GitHubClient struct {
	token      string
	org        string
	httpClient *http.Client
}

var nextLinkRegex = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func (g *GitHubClient) fetchAllPages(ctx context.Context, initialURL string, decodePage func(body []byte) error) error {
	nextURL := initialURL

	for nextURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			return fmt.Errorf("creating request: %w", err)
		}

		req.Header.Set("Authorization", "Bearer "+g.token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "rundeck-github-options-provider")

		resp, err := g.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("executing request: %w", err)
		}

		body, readErr := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil {
			readErr = errors.Join(readErr, fmt.Errorf("closing response body: %w", closeErr))
		}
		if readErr != nil {
			return fmt.Errorf("reading response: %w", readErr)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("github api error (%d): %s", resp.StatusCode, string(body))
		}

		if err := decodePage(body); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}

		// Parse RFC 5988 Link header for pagination
		nextURL = ""
		if linkHeader := resp.Header.Get("Link"); linkHeader != "" {
			if matches := nextLinkRegex.FindStringSubmatch(linkHeader); len(matches) == 2 {
				nextURL = matches[1]
			}
		}
	}

	return nil
}

func (g *GitHubClient) FetchMembers(ctx context.Context) ([]RundeckOption, error) {
	url := fmt.Sprintf("https://api.github.com/orgs/%s/members?per_page=100", g.org)
	var options []RundeckOption

	err := g.fetchAllPages(ctx, url, func(body []byte) error {
		var page []ghMember
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		for _, m := range page {
			options = append(options, RundeckOption{
				Name:  m.Login,
				Value: m.Login,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(options, func(i, j int) bool {
		return options[i].Name < options[j].Name
	})
	return options, nil
}

func (g *GitHubClient) FetchTeams(ctx context.Context) ([]RundeckOption, error) {
	url := fmt.Sprintf("https://api.github.com/orgs/%s/teams?per_page=100", g.org)
	var options []RundeckOption

	err := g.fetchAllPages(ctx, url, func(body []byte) error {
		var page []ghTeam
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		for _, t := range page {
			options = append(options, RundeckOption{
				Name:  t.Name, // Human-readable team name shown in Rundeck UI
				Value: t.Slug, // URL-safe slug passed to Rundeck job steps
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(options, func(i, j int) bool {
		return options[i].Name < options[j].Name
	})
	return options, nil
}

func loadConfig() (Config, error) {
	cfg := Config{
		ListenAddr:      ":8080",
		CacheTTLSeconds: 300,
	}

	var configPath string
	var flagToken string
	var flagOrg string
	var flagAddr string
	var flagTTL int

	flag.StringVar(&configPath, "config", "config.json", "Path to JSON config file")
	flag.StringVar(&flagToken, "token", "", "GitHub Personal Access Token (overrides config & env)")
	flag.StringVar(&flagOrg, "org", "", "GitHub Organization login (overrides config & env)")
	flag.StringVar(&flagAddr, "addr", "", "HTTP listen address (overrides config)")
	flag.IntVar(&flagTTL, "ttl", 0, "Cache refresh interval in seconds (overrides config)")
	flag.Parse()

	// 1. Load from config file if present
	if data, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("invalid config file %s: %w", configPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return cfg, fmt.Errorf("reading config file %s: %w", configPath, err)
	}

	// 2. Override with environment variables if set
	if envToken := os.Getenv("GITHUB_TOKEN"); envToken != "" {
		cfg.GitHubToken = envToken
	}
	if envOrg := os.Getenv("GITHUB_ORG"); envOrg != "" {
		cfg.GitHubOrg = envOrg
	}

	// 3. Override with CLI flags if explicitly provided
	if flagToken != "" {
		cfg.GitHubToken = flagToken
	}
	if flagOrg != "" {
		cfg.GitHubOrg = flagOrg
	}
	if flagAddr != "" {
		cfg.ListenAddr = flagAddr
	}
	if flagTTL > 0 {
		cfg.CacheTTLSeconds = flagTTL
	}

	if cfg.GitHubToken == "" || cfg.GitHubOrg == "" {
		return cfg, errors.New("github_token and github_org are required (via -config, flags, or env)")
	}

	return cfg, nil
}

func refreshCache(ctx context.Context, client *GitHubClient, cache *Cache) {
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	members, err := client.FetchMembers(reqCtx)
	if err != nil {
		log.Printf("ERROR fetching members: %v", err)
		cache.SetError(err)
		return
	}

	teams, err := client.FetchTeams(reqCtx)
	if err != nil {
		log.Printf("ERROR fetching teams: %v", err)
		cache.SetError(err)
		return
	}

	cache.Update(members, teams)
	log.Printf("Cache refreshed: %d members, %d teams", len(members), len(teams))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("ERROR encoding JSON response: %v", err)
	}
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	client := &GitHubClient{
		token:      cfg.GitHubToken,
		org:        cfg.GitHubOrg,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
	cache := &Cache{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Perform initial synchronous fetch before accepting traffic
	refreshCache(ctx, client, cache)

	// Background refresh loop
	ticker := time.NewTicker(time.Duration(cfg.CacheTTLSeconds) * time.Second)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ticker.C:
				refreshCache(ctx, client, cache)
			case <-ctx.Done():
				return
			}
		}
	}()

	mux := http.NewServeMux()

	// Rundeck Option Endpoint: Organization Members
	mux.HandleFunc("GET /options/members", func(w http.ResponseWriter, r *http.Request) {
		members, updated := cache.GetMembers()
		if updated.IsZero() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cache not yet populated"})
			return
		}
		if members == nil {
			members = []RundeckOption{}
		}
		writeJSON(w, http.StatusOK, members)
	})

	// Rundeck Option Endpoint: Organization Teams
	mux.HandleFunc("GET /options/teams", func(w http.ResponseWriter, r *http.Request) {
		teams, updated := cache.GetTeams()
		if updated.IsZero() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cache not yet populated"})
			return
		}
		if teams == nil {
			teams = []RundeckOption{}
		}
		writeJSON(w, http.StatusOK, teams)
	})

	// Manual force-refresh endpoint
	mux.HandleFunc("POST /refresh", func(w http.ResponseWriter, r *http.Request) {
		refreshCache(r.Context(), client, cache)
		writeJSON(w, http.StatusOK, cache.Stats())
	})

	// Health & Cache Metrics endpoint (JSON)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, cache.Stats())
	})

	// Prometheus-compatible metrics endpoint
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		stats := cache.Stats()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")

		var buf bytes.Buffer
		_, _ = fmt.Fprintf(&buf, "# HELP github_cache_members_total Number of cached GitHub org members\n")
		_, _ = fmt.Fprintf(&buf, "# TYPE github_cache_members_total gauge\n")
		_, _ = fmt.Fprintf(&buf, "github_cache_members_total{org=%q} %d\n", cfg.GitHubOrg, stats["member_count"])
		_, _ = fmt.Fprintf(&buf, "# HELP github_cache_teams_total Number of cached GitHub org teams\n")
		_, _ = fmt.Fprintf(&buf, "# TYPE github_cache_teams_total gauge\n")
		_, _ = fmt.Fprintf(&buf, "github_cache_teams_total{org=%q} %d\n", cfg.GitHubOrg, stats["team_count"])
		if t, ok := stats["last_updated"].(time.Time); ok && !t.IsZero() {
			_, _ = fmt.Fprintf(&buf, "# HELP github_cache_last_updated_timestamp_seconds Unix timestamp of last successful refresh\n")
			_, _ = fmt.Fprintf(&buf, "# TYPE github_cache_last_updated_timestamp_seconds gauge\n")
			_, _ = fmt.Fprintf(&buf, "github_cache_last_updated_timestamp_seconds{org=%q} %d\n", cfg.GitHubOrg, t.Unix())
		}

		if _, err := w.Write(buf.Bytes()); err != nil {
			log.Printf("ERROR writing metrics response: %v", err)
		}
	})

	log.Printf("Serving Rundeck GitHub options for org %q on %s", cfg.GitHubOrg, cfg.ListenAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, mux); err != nil {
		log.Fatalf("HTTP server failed: %v", err)
	}
}
