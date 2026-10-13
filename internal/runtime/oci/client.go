// Package oci implements runtime.Backend on the Docker Engine API.
//
// Security posture of containers created here (see docs/THREAT-MODEL.md):
// network=none, all capabilities dropped, no-new-privileges, PID limit,
// hard memory cap with swap disabled, CPU quota. This is strong process
// isolation on a shared kernel — one tier below microVMs, and the backend
// says so: what it cannot enforce (disk quotas on most filesystems, egress
// allowlists) it refuses, never fakes.
package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
)

const (
	apiVersion    = "v1.44"
	pseudoHost    = "sandboxd-docker"
	respLimit     = 8 << 20 // cap JSON response reads
	defaultExecTO = 5 * time.Minute
)

// Client is a minimal Docker Engine API client over a unix socket (or TCP).
type Client struct {
	hc   *http.Client
	base string // "http://" + pseudoHost
}

// NewClient resolves the daemon endpoint: explicit host arg, then
// DOCKER_HOST, then the usual socket paths (docker, colima, Docker Desktop).
func NewClient(host string) (*Client, error) {
	candidates := []string{}
	if host != "" {
		candidates = append(candidates, host)
	}
	if env := os.Getenv("DOCKER_HOST"); env != "" {
		candidates = append(candidates, env)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			"unix://"+filepath.Join(home, ".colima", "default", "docker.sock"),
			"unix://"+filepath.Join(home, ".docker", "run", "docker.sock"),
		)
	}
	candidates = append(candidates, "unix:///var/run/docker.sock")

	for _, cand := range candidates {
		network, addr, err := parseHost(cand)
		if err != nil {
			continue
		}
		c := &Client{base: "http://" + pseudoHost}
		c.hc = &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					d := net.Dialer{Timeout: 5 * time.Second}
					return d.DialContext(ctx, network, addr)
				},
				DisableKeepAlives: false,
			},
			Timeout: 10 * time.Minute, // execs may legitimately run long; per-call ctx deadlines do the real work
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.ping(ctx); err == nil {
			return c, nil
		}
	}
	return nil, apperr.Unavailable("no reachable docker daemon (tried: %s)", strings.Join(candidates, ", "))
}

func parseHost(host string) (network, addr string, err error) {
	u := host
	switch {
	case strings.HasPrefix(u, "unix://"):
		return "unix", strings.TrimPrefix(u, "unix://"), nil
	case strings.HasPrefix(u, "tcp://"):
		return "tcp", strings.TrimPrefix(u, "tcp://"), nil
	default:
		return "", "", fmt.Errorf("unsupported docker host %q (use unix:// or tcp://)", host)
	}
}

func (c *Client) ping(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/_ping", nil, nil)
	return err
}

// infoBody is the subset of GET /info that the isolation posture depends on.
type infoBody struct {
	SecurityOptions []string                   `json:"SecurityOptions"`
	Runtimes        map[string]json.RawMessage `json:"Runtimes"`
	DefaultRuntime  string                     `json:"DefaultRuntime"`
}

func (c *Client) info(ctx context.Context) (*infoBody, error) {
	b, err := c.do(ctx, http.MethodGet, "/info", nil, nil)
	if err != nil {
		return nil, err
	}
	var ib infoBody
	if err := json.Unmarshal(b, &ib); err != nil {
		return nil, apperr.InternalErr("docker /info: %v", err)
	}
	return &ib, nil
}

// hasUserns reports whether this daemon has a user-namespace remap configured.
//
// This is the ONLY trustworthy gate for userns. HostConfig.UsernsMode="private"
// is not a request that can be honoured on demand: a daemon without a remap
// accepts the field, stores it, and echoes it back from
// GET /containers/<id>/json verbatim — while the container still runs with the
// identity map. Measured on Docker 29.5.2 with no userns-remap:
//
//	HostConfig.UsernsMode   = "private"   (read back from /json)
//	/proc/self/uid_map      = 0 0 4294967295   (no mapping; identical to host mode)
//
// So setting "private" unconditionally does not fail — it silently does
// nothing, which is strictly worse than leaving it off, because the control
// plane would then report a user namespace that does not exist. The daemon's
// own /info is the authoritative statement of whether remapping is available.
func (ib *infoBody) hasUserns() bool { return ib.hasSecurityOption("userns") }

// hasSecurityOption reports whether /info advertises a named security feature.
// The daemon lists these as "name=<feature>" entries (optionally followed by
// ",key=value" details, e.g. "name=seccomp,profile=builtin").
func (ib *infoBody) hasSecurityOption(name string) bool {
	prefix := "name=" + name
	for _, s := range ib.SecurityOptions {
		if s == prefix || strings.HasPrefix(s, prefix+",") {
			return true
		}
	}
	return false
}

// imageSummary is the subset of GET /images/json used to pick probe images.
type imageSummary struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Size     int64    `json:"Size"`
}

// smallestUsableImage returns the smallest local image that carries a real
// repository tag. Used only as a fallback when the operator has not named a
// probe image: the isolation self-check needs nothing but a shell, so the
// cheapest local image is the right default.
func (c *Client) smallestUsableImage(ctx context.Context) (string, error) {
	b, err := c.do(ctx, http.MethodGet, "/images/json", map[string]string{"all": "0"}, nil)
	if err != nil {
		return "", err
	}
	var imgs []imageSummary
	if err := json.Unmarshal(b, &imgs); err != nil {
		return "", apperr.InternalErr("docker /images/json: %v", err)
	}
	best, bestSize := "", int64(-1)
	for _, im := range imgs {
		for _, tag := range im.RepoTags {
			if tag == "" || strings.HasPrefix(tag, "<none>") {
				continue
			}
			if bestSize < 0 || im.Size < bestSize {
				best, bestSize = tag, im.Size
			}
			break
		}
	}
	if best == "" {
		return "", apperr.Unavailable("no tagged local image to probe with")
	}
	return best, nil
}

// do performs an API call returning the (bounded) body on 2xx. Callers that
// stream (exec, archive get) use doStream instead.
func (c *Client) do(ctx context.Context, method, path string, query map[string]string, body io.Reader) ([]byte, error) {
	resp, closer, err := c.raw(ctx, method, path, query, body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return nil, err
	}
	defer closer()
	b, err := io.ReadAll(io.LimitReader(resp.Body, respLimit))
	if err != nil {
		return nil, apperr.Unavailable("docker %s %s: read body: %v", method, path, err)
	}
	if resp.StatusCode >= 400 {
		return nil, statusErr(resp.StatusCode, path, b)
	}
	return b, nil
}

func (c *Client) raw(ctx context.Context, method, path string, query map[string]string, body io.Reader, headers map[string]string) (*http.Response, func(), error) {
	q := ""
	if len(query) > 0 {
		var parts []string
		for k, v := range query {
			parts = append(parts, k+"="+urlQueryEscape(v))
		}
		q = "?" + strings.Join(parts, "&")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/"+apiVersion+path+q, body)
	if err != nil {
		return nil, nil, apperr.InternalErr("build docker request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, apperr.Unavailable("docker unreachable (%s %s): %v", method, path, err)
	}
	closer := func() { resp.Body.Close() }
	return resp, closer, nil
}

// statusErr converts a Docker error response into an *apperr.E with codes
// the service layer can act on.
func statusErr(status int, path string, body []byte) error {
	msg := strings.TrimSpace(string(body))
	var dj struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &dj); err == nil && dj.Message != "" {
		msg = dj.Message
	}
	switch status {
	case http.StatusNotFound:
		return apperr.New(apperr.NotFound, fmt.Sprintf("docker: not found on %s: %s", path, msg))
	case http.StatusConflict:
		return apperr.ConflictErr("docker conflict on %s: %s", path, msg)
	case http.StatusBadRequest:
		return apperr.Invalid("docker rejected %s: %s", path, msg)
	default:
		return apperr.Unavailable("docker error %d on %s: %s", status, path, msg)
	}
}

func urlQueryEscape(s string) string {
	// minimal escaper: Docker paths/queries need %XX for reserved chars
	const hex = "0123456789ABCDEF"
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') ||
			ch == '-' || ch == '_' || ch == '.' || ch == '~' {
			sb.WriteByte(ch)
		} else if ch == ' ' {
			sb.WriteByte('+')
		} else {
			sb.WriteByte('%')
			sb.WriteByte(hex[ch>>4])
			sb.WriteByte(hex[ch&0xF])
		}
	}
	return sb.String()
}
