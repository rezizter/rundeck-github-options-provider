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
