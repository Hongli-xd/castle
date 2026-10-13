package tests

// Typed API client shared by every end-to-end suite.
//
// Response *domain* types are imported from internal/domain on purpose: a
// Sandbox decoded here is the same value the service layer persisted, so a
// field rename breaks the suites at compile time instead of at 3am.
//
// Request *body* types are restated locally. That looks like duplication and
// is not: the wire shape is itself the contract under test, so a suite that
// compiled against an unexported server-side struct could never notice the
// wire name changing under it.
//
// The client holds no testing.T. Assertions take one explicitly. An earlier
// iteration captured the *parent* test in the client, so the first failure
// inside a subtest tore down the whole parent and silently skipped every
// scenario after it — the worst possible behaviour for a suite whose entire
// job is to report the status of many independent claims.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
)

// ---- wire shapes ------------------------------------------------------------

// apiError is the envelope built by httpapi.writeError. It is its own type
// rather than apperr.E because the wire keys are lowercase and hand-assembled
// while apperr.E is the internal shape — but the Code constants are shared,
// which is the part that must not drift.
type apiError struct {
	Code      apperr.Code `json:"code"`
	Message   string      `json:"message"`
	Retryable bool        `json:"retryable"`
	RequestID string      `json:"request_id"`
}

// keyMint is the only response in the product that carries a plaintext key.
type keyMint struct {
	Key    keyInfo `json:"key"`
	Secret string  `json:"secret"`
	Note   string  `json:"note"`
}

type keyInfo struct {
	ID        string     `json:"id"`
	TenantID  string     `json:"tenant_id"`
	Name      string     `json:"name"`
	Scope     string     `json:"scope"`
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// execReq mirrors httpapi.execBody.
type execReq struct {
	Argv           []string          `json:"argv,omitempty"`
	Command        string            `json:"command,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	User           string            `json:"user,omitempty"`
	WorkDir        string            `json:"workdir,omitempty"`
	TimeoutMs      int               `json:"timeout_ms,omitempty"`
	MaxOutput      int               `json:"max_output,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
}

func (e execReq) describe() string {
	if e.Command != "" {
		return fmt.Sprintf("%q", e.Command)
	}
	return fmt.Sprintf("%v", e.Argv)
}

// createBody mirrors httpapi.createSandboxBody.
type createBody struct {
	TemplateID string             `json:"template_id,omitempty"`
	SnapshotID string             `json:"snapshot_id,omitempty"`
	BaseImage  string             `json:"base_image,omitempty"`
	Spec       domain.SandboxSpec `json:"spec"`
	Metadata   map[string]string  `json:"metadata,omitempty"`
}

// list envelopes.
type sandboxPage struct {
	Sandboxes []*domain.Sandbox `json:"sandboxes"`
	Count     int               `json:"count"`
}
type usagePage struct {
	Usage []*domain.Usage `json:"usage"`
}
type eventPage struct {
	Events []*domain.AuditEvent `json:"events"`
}
type snapshotPage struct {
	Snapshots []*domain.Snapshot `json:"snapshots"`
}
type tenantPage struct {
	Tenants []*domain.Tenant `json:"tenants"`
}
type keyPage struct {
	Keys []keyInfo `json:"keys"`
}
type templatePage struct {
	Templates []*domain.Template `json:"templates"`
}
type filePage struct {
	Files []struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size"`
	} `json:"files"`
}

// readyInfo mirrors the body of GET /readyz.
type readyInfo struct {
	Status             string   `json:"status"`
	Backend            string   `json:"backend"`
	Isolation          string   `json:"isolation"`
	IsolationNote      string   `json:"isolation_note"`
	IsolationVerified  bool     `json:"isolation_verified"`
	IsolationNotRunMsg string   `json:"isolation_verified_note"`
	IsolationUserns    string   `json:"isolation_userns"`
	IsolationFailures  []string `json:"isolation_failures"`
	Detail             string   `json:"detail"`
}

// ---- response ---------------------------------------------------------------

// Resp wraps one HTTP exchange. Status 0 with a TransportErr means no
// exchange happened at all, which must never be confused with a refusal the
// server chose to send: one is a broken test, the other is a working product.
type Resp struct {
	Method       string
	Path         string
	Status       int
	Header       http.Header
	Raw          []byte
	TransportErr error
}

func (r *Resp) describe() string {
	if r.TransportErr != nil {
		return fmt.Sprintf("%s %s: no response (transport error: %v)", r.Method, r.Path, r.TransportErr)
	}
	return fmt.Sprintf("%s %s: status %d", r.Method, r.Path, r.Status)
}

func (r *Resp) snippet() string {
	s := strings.TrimSpace(string(r.Raw))
	if s == "" {
		return "(empty body)"
	}
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	return s
}

// Decode unmarshals the body, failing the test with the raw body on error.
// A pointer type parameter yields a pointer: decode[*domain.Sandbox](t, r).
func (r *Resp) Decode(t *testing.T, v any) *Resp {
	t.Helper()
	if r.TransportErr != nil {
		t.Fatalf("%s\nthe value under test was not measured", r.describe())
	}
	if len(r.Raw) == 0 {
		t.Fatalf("%s: expected JSON, got an empty body", r.describe())
	}
	if err := json.Unmarshal(r.Raw, v); err != nil {
		t.Fatalf("%s: cannot decode body into %T: %v\nbody: %s", r.describe(), v, err, r.snippet())
	}
	return r
}

// Err returns the error envelope, or nil when the body is not one. The
// envelope is nested under "error"; the first version of this helper decoded
// the body into the flat struct, so Err() returned nil for every real error
// and a dozen assertions failed claiming the server had sent no envelope
// while the envelope sat in the message.
func (r *Resp) Err() *apiError {
	if len(r.Raw) == 0 {
		return nil
	}
	var env struct {
		Error *apiError `json:"error"`
	}
	if err := json.Unmarshal(r.Raw, &env); err != nil || env.Error == nil || env.Error.Code == "" {
		return nil
	}
	return env.Error
}

// StatusIs reports the status without failing, for branching scenarios.
func (r *Resp) StatusIs(want int) bool { return r.Status == want }

func (r *Resp) MustStatus(t *testing.T, want ...int) *Resp {
	t.Helper()
	if r.TransportErr != nil {
		t.Fatalf("%s\nerror: %v\nthe value under test was not measured", r.describe(), r.TransportErr)
	}
	for _, w := range want {
		if r.Status == w {
			return r
		}
	}
	t.Fatalf("%s, want one of %v\nbody: %s", r.describe(), want, r.snippet())
	return r
}

func (r *Resp) MustOK(t *testing.T) *Resp {
	t.Helper()
	if r.TransportErr != nil {
		t.Fatalf("%s\nerror: %v", r.describe(), r.TransportErr)
	}
	if r.Status < 200 || r.Status > 299 {
		t.Fatalf("%s, want 2xx\nbody: %s", r.describe(), r.snippet())
	}
	return r
}

// MustErr asserts the governed error code rather than the status: the API
// documents that clients branch on Code, so the suite must too. It then
// checks the status agrees with the code's own mapping, because a code
// travelling on the wrong transport is its own kind of lie.
func (r *Resp) MustErr(t *testing.T, want apperr.Code) *Resp {
	t.Helper()
	if r.TransportErr != nil {
		t.Fatalf("%s\nerror: %v\nthe refusal under test never reached the server", r.describe(), r.TransportErr)
	}
	e := r.Err()
	if e == nil {
		t.Fatalf("%s, want error %q but the body carried no error envelope: %s", r.describe(), want, r.snippet())
	}
	if e.Code != want {
		t.Fatalf("%s: error code %q, want %q (message: %s)", r.describe(), e.Code, want, e.Message)
	}
	if got := e.Code.HTTPStatus(); got != r.Status {
		t.Fatalf("%s: code %q maps to HTTP %d but the response arrived as %d", r.describe(), e.Code, got, r.Status)
	}
	return r
}

// ---- client -----------------------------------------------------------------

// Client is one credential against one daemon. Transport failures are
// recorded on the response instead of raised, so the caller decides whether a
// missing response is the point of the scenario or a broken test.
type Client struct {
	base  string
	key   string
	extra map[string]string
	hc    *http.Client
}

func newClient(base, key string) *Client {
	return &Client{
		base: base, key: key, extra: map[string]string{},
		hc: &http.Client{Timeout: 5 * time.Minute},
	}
}

// As returns a client against the same daemon with a different credential.
func (c *Client) As(key string) *Client {
	cp := *c
	cp.key = key
	return &cp
}

// WithHeader returns a client that adds a header to every request.
func (c *Client) WithHeader(k, v string) *Client {
	cp := *c
	cp.extra = map[string]string{}
	for kk, vv := range c.extra {
		cp.extra[kk] = vv
	}
	cp.extra[k] = v
	return &cp
}

func (c *Client) Get(path string) *Resp          { return c.do("GET", path, nil) }
func (c *Client) Post(path string, b any) *Resp  { return c.do("POST", path, b) }
func (c *Client) Patch(path string, b any) *Resp { return c.do("PATCH", path, b) }
func (c *Client) Put(path string, b any) *Resp   { return c.do("PUT", path, b) }
func (c *Client) Delete(path string) *Resp       { return c.do("DELETE", path, nil) }

func (c *Client) do(method, path string, body any) *Resp {
	ct, buf, err := encodeBody(body)
	if err != nil {
		return &Resp{Method: method, Path: path, TransportErr: err}
	}
	var rdr io.Reader
	if buf != nil {
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return &Resp{Method: method, Path: path, TransportErr: err}
	}
	if ct != "" && len(buf) > 0 {
		req.Header.Set("Content-Type", ct)
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	for k, v := range c.extra {
		req.Header.Set(k, v)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return &Resp{Method: method, Path: path, TransportErr: err}
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return &Resp{Method: method, Path: path, Status: res.StatusCode, Header: res.Header, TransportErr: err}
	}
	return &Resp{Method: method, Path: path, Status: res.StatusCode, Header: res.Header, Raw: raw}
}

func encodeBody(body any) (string, []byte, error) {
	switch b := body.(type) {
	case nil:
		return "", nil, nil
	case []byte:
		return "application/octet-stream", b, nil
	case string:
		return "application/json", []byte(b), nil
	default:
		blob, err := json.Marshal(b)
		if err != nil {
			return "", nil, err
		}
		return "application/json", blob, nil
	}
}

// decode unmarshals into T and returns it. Pass a pointer type parameter for
// a pointer result: decode[*domain.Sandbox](t, r).
func decode[T any](t *testing.T, r *Resp) T {
	t.Helper()
	var v T
	r.Decode(t, &v)
	return v
}
