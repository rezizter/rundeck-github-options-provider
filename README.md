# Rundeck GitHub Options Provider

A zero-dependency Go service that periodically fetches and caches members and teams from a GitHub organization, serving them over HTTP in the exact JSON format expected by Rundeck's **Remote URL** job options provider.

## Features

- **Zero External Dependencies:** Built strictly using the Go standard library (`net/http`, `encoding/json`, `sync`, `flag`).
- **Automatic Pagination:** Follows GitHub REST API RFC 5988 `Link` headers to retrieve organizations of any size.
- **Thread-Safe In-Memory Cache:** Serves concurrent Rundeck UI requests instantly without hitting GitHub API rate limits, refreshing in the background on a configurable TTL.
- **Rundeck-Native Formatting:** Returns `[{"name": "...", "value": "..."}]` with `application/json` headers—mapping human-readable team names to URL-safe team slugs.
- **Observability Included:** Exposes both a JSON `/health` endpoint and a Prometheus-compatible `/metrics` endpoint.

---

## Prerequisites

- **Go 1.22+** (uses method-based routing in `net/http`)
- **GitHub Personal Access Token (PAT):**
  - Classic PAT: Requires the `read:org` scope (to read private organization members and teams).
  - Fine-grained PAT: Requires **Organization permissions → Members (Read-only)**.

---

## Installation & Running

1. **Initialize and build:**
   ```bash
   go mod init github-rundeck-options
   go build -o github-rundeck-options main.go
   ```

2. **Create a <mark>config.json</mark> file:**
   ```bash
   {
     "github_token": "ghp_your_personal_access_token",
     "github_org": "your-organization-name",
     "listen_addr": ":8080",
     "cache_ttl_seconds": 300
   }
   ```

3. **Start the service:**
   ```bash
   ./github-rundeck-options -config config.json
   ```

You may also start the service using flags:
   ```bash
   ./github-rundeck-options -token ghp_xxx -org my-org -addr :8080 -ttl 600
   ```

## Configuration Precedence
Settings are loaded in the following order (later sources override earlier ones):

| Setting	      | Key    		  | Environment Variable | CLI Flag |
| :------------------ | :---------------: | :------------------: | -------: |
| GitHub Token 	      | github_token	  | GITHUB_TOKEN	 | -token   |
| GitHub Organization | github_org	  | GITHUB_ORG		 | -org	    |
| Listen Address      | listen_addr	  | -			 | -addr    |
| Cache Refresh TTL   | cache_ttl_seconds | -			 | -ttl	    |
