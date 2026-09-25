// sbxctl is the command-line client for sandboxd. It is a thin HTTP client:
// every operation maps to one REST call, errors surface the server's stable
// error code, and `exec` propagates the remote exit code.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	apiURL = envOr("SBX_URL", "http://127.0.0.1:8080")
	apiKey = os.Getenv("SBX_API_KEY")
	client = &http.Client{Timeout: 10 * time.Minute}
)

const usage = `sbxctl — control a sandboxd runtime

Usage:
  sbxctl [flags] <command> [args]

Sandbox commands:
  create    [--template id | --snapshot id | --image ref] [--cpu m] [--mem m] [--timeout s]
            [--env K=V]... [--wait=false] [--meta k=v]...    create a sandbox
  list      [--state running]                                 list sandboxes
  get       <id>                                              inspect one sandbox
  exec      <id> [--cwd dir] [--user u] [--timeout 30s] -- argv...    run a command
  sh        <id> [--timeout 30s] [-- <shell command>]         run via /bin/sh -c
  cp        <id> <local|- > <remote-path>                     put file (stdin with -)
  cat       <id> <remote-path>                                fetch a file to stdout
  ls        <id> <remote-dir>                                 list a directory
  rm        <id> <remote-path>                                delete a path
  pause / resume / stop [--force] / delete [--force]  <id>
  timeout   <id> [seconds]                                    extend TTL

Snapshots & templates:
  snapshots | snapshot <id> | snapshot-create <sandbox> <name> | snapshot-delete <id>
  templates

Platform (admin):
  tenants | tenant-create <name> | tenant <id> | tenant-patch <id> <k=v>...
  keys <tenant> | key-create <tenant> <name> [--ttl 86400] | key-revoke <tenant> <key-id>
  events [--tenant id] [--sandbox id] [--action a] [--limit n]

Environment:
  SBX_URL      daemon base URL (default http://127.0.0.1:8080)
  SBX_API_KEY  API key (also --key)
`

func main() {
	fs := flag.NewFlagSet("sbxctl", flag.ContinueOnError)
	key := fs.String("key", "", "API key (overrides SBX_API_KEY)")
	url := fs.String("url", "", "daemon URL (overrides SBX_URL)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *key != "" {
		apiKey = *key
	}
	if *url != "" {
		apiURL = *url
	}
	args := fs.Args()
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	code := run(args[0], args[1:])
	os.Exit(code)
}

func run(cmd string, args []string) int {
	if apiKey == "" && cmd != "help" {
		fmt.Fprintln(os.Stderr, "sbxctl: SBX_API_KEY or --key is required")
		return 2
	}
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	case "create":
		return cmdCreate(args)
	case "list", "ls-sandboxes":
		return cmdList(args)
	case "get":
		return cmdGet(arg1(args))
	case "exec":
		return cmdExec(args)
	case "sh":
		return cmdSh(args)
	case "cp":
		return cmdCp(args)
	case "cat":
		return cmdCat(args)
	case "ls":
		return cmdLs(args)
	case "rm":
		return cmdRm(args)
	case "pause", "resume", "stop", "delete":
		return cmdLifecycle(cmd, args)
	case "timeout":
		return cmdTimeout(args)
	case "snapshots":
		return cmdSnapshots()
	case "snapshot":
		return cmdSnapshot(arg1(args))
	case "snapshot-create":
		return cmdSnapshotCreate(args)
	case "snapshot-delete":
		return cmdDelete("snapshots", arg1(args))
	case "templates":
		return cmdTemplates()
	case "tenants":
		return cmdTenants()
	case "tenant-create":
		return cmdTenantCreate(args)
	case "tenant":
		return cmdTenant(arg1(args))
	case "tenant-patch":
		return cmdTenantPatch(args)
	case "keys":
		return cmdKeys(arg1(args))
	case "key-create":
		return cmdKeyCreate(args)
	case "key-revoke":
		return cmdKeyRevoke(args)
	case "events":
		return cmdEvents(args)
	default:
		fmt.Fprintf(os.Stderr, "sbxctl: unknown command %q\n\n", cmd)
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
}

func arg1(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// ---- HTTP plumbing ------------------------------------------------------------

func do(method, path string, body any, out any) int {
	var reader io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			fmt.Fprintln(os.Stderr, "marshal:", err)
			return 2
		}
		reader = bytes.NewReader(blob)
	}
	req, err := http.NewRequest(method, strings.TrimRight(apiURL, "/")+path, reader)
	if err != nil {
		fmt.Fprintln(os.Stderr, "request:", err)
		return 2
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		return 2
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 400 {
		return printAPIError(resp.StatusCode, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			fmt.Fprintln(os.Stderr, "decode:", err)
			return 2
		}
	}
	return 0
}

type apiError struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func printAPIError(status int, data []byte) int {
	var ae apiError
	if err := json.Unmarshal(data, &ae); err == nil && ae.Error.Code != "" {
		fmt.Fprintf(os.Stderr, "error [%s] (http %d): %s\n", ae.Error.Code, status, ae.Error.Message)
		if ae.Error.RequestID != "" {
			fmt.Fprintf(os.Stderr, "  request_id: %s\n", ae.Error.RequestID)
		}
		return 1
	}
	fmt.Fprintf(os.Stderr, "error: http %d: %s\n", status, strings.TrimSpace(string(data)))
	return 1
}

func pp(v any) {
	blob, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(blob))
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func parseKV(spec string) (string, string, bool) {
	for i := 0; i < len(spec); i++ {
		if spec[i] == '=' {
			return spec[:i], spec[i+1:], true
		}
	}
	return spec, "", false
}

// ---- commands -------------------------------------------------------------------

func cmdCreate(args []string) int {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	template := fs.String("template", "", "")
	snapshot := fs.String("snapshot", "", "")
	image := fs.String("image", "", "")
	cpu := fs.Int("cpu", 0, "")
	mem := fs.Int("mem", 0, "")
	timeout := fs.Int("timeout", 0, "")
	wait := fs.Bool("wait", true, "")
	envs := multiFlag{}
	metas := multiFlag{}
	fs.Var(&envs, "env", "K=V (repeatable)")
	fs.Var(&metas, "meta", "k=v (repeatable)")
	fs.Parse(args)

	body := map[string]any{}
	if *template != "" {
		body["template_id"] = *template
	}
	if *snapshot != "" {
		body["snapshot_id"] = *snapshot
	}
	if *image != "" {
		body["base_image"] = *image
	}
	spec := map[string]any{}
	if *cpu > 0 {
		spec["cpu_millicores"] = *cpu
	}
	if *mem > 0 {
		spec["memory_mib"] = *mem
	}
	if *timeout > 0 {
		spec["timeout_seconds"] = *timeout
	}
	if len(envs) > 0 {
		env := map[string]string{}
		for _, e := range envs {
			k, v, ok := parseKV(e)
			if !ok {
				fmt.Fprintf(os.Stderr, "bad --env %q (want K=V)\n", e)
				return 2
			}
			env[k] = v
		}
		spec["env"] = env
	}
	if len(spec) > 0 {
		body["spec"] = spec
	}
	if len(metas) > 0 {
		md := map[string]string{}
		for _, m := range metas {
			k, v, ok := parseKV(m)
			if !ok {
				fmt.Fprintf(os.Stderr, "bad --meta %q (want k=v)\n", m)
				return 2
			}
			md[k] = v
		}
		body["metadata"] = md
	}
	var out struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if code := do("POST", "/v1/sandboxes?wait="+boolStr(*wait), body, &out); code != 0 {
		return code
	}
	pp(out)
	return 0
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func cmdList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	state := fs.String("state", "", "")
	fs.Parse(args)
	q := ""
	if *state != "" {
		q = "?state=" + *state
	}
	var out struct {
		Sandboxes []struct {
			ID        string            `json:"id"`
			State     string            `json:"state"`
			TenantID  string            `json:"tenant_id"`
			CreatedAt time.Time         `json:"created_at"`
			ExpiresAt time.Time         `json:"expires_at"`
			Metadata  map[string]string `json:"metadata"`
		} `json:"sandboxes"`
	}
	if code := do("GET", "/v1/sandboxes"+q, nil, &out); code != 0 {
		return code
	}
	fmt.Printf("%-30s %-13s %-27s %s\n", "ID", "STATE", "CREATED", "EXPIRES")
	for _, sb := range out.Sandboxes {
		fmt.Printf("%-30s %-13s %-27s %s\n", sb.ID, sb.State, sb.CreatedAt.Format(time.RFC3339), sb.ExpiresAt.Format(time.RFC3339))
	}
	return 0
}

func cmdGet(id string) int {
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: sbxctl get <id>")
		return 2
	}
	var out any
	if code := do("GET", "/v1/sandboxes/"+id, nil, &out); code != 0 {
		return code
	}
	pp(out)
	return 0
}

func cmdExec(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	cwd := fs.String("cwd", "", "")
	user := fs.String("user", "", "")
	timeout := fs.Duration("timeout", 60*time.Second, "")
	fs.Parse(args)
	rest := fs.Args()
	// a leading "--" survives when the id precedes it; drop it
	if len(rest) > 1 && rest[1] == "--" {
		rest = append(rest[:1], rest[2:]...)
	}
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sbxctl exec <id> -- argv...")
		return 2
	}
	body := map[string]any{
		"argv":       rest[1:],
		"timeout_ms": timeout.Milliseconds(),
	}
	if *cwd != "" {
		body["workdir"] = *cwd
	}
	if *user != "" {
		body["user"] = *user
	}
	var out struct {
		ExitCode  int     `json:"exit_code"`
		Stdout    string  `json:"stdout"`
		Stderr    string  `json:"stderr"`
		Duration  float64 `json:"duration_ms"`
		Truncated bool    `json:"truncated"`
		TimedOut  bool    `json:"timed_out"`
	}
	// duration_ms arrives as a number of ns in ExecResult.Duration
	code := do("POST", "/v1/sandboxes/"+rest[0]+"/exec", body, &out)
	if code != 0 {
		return code
	}
	fmt.Print(out.Stdout)
	fmt.Fprint(os.Stderr, out.Stderr)
	return out.ExitCode
}

func cmdSh(args []string) int {
	fs := flag.NewFlagSet("sh", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 60*time.Second, "")
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) > 1 && rest[1] == "--" {
		rest = append(rest[:1], rest[2:]...)
	}
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: sbxctl sh <id> [-- command]")
		return 2
	}
	command := "exec sh"
	if len(rest) > 1 {
		command = strings.Join(rest[1:], " ")
	}
	body := map[string]any{
		"command":    command,
		"timeout_ms": timeout.Milliseconds(),
	}
	var out struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	code := do("POST", "/v1/sandboxes/"+rest[0]+"/exec", body, &out)
	if code != 0 {
		return code
	}
	fmt.Print(out.Stdout)
	fmt.Fprint(os.Stderr, out.Stderr)
	return out.ExitCode
}

func cmdCp(args []string) int {
	rest := nonFlags(args)
	if len(rest) != 3 {
		fmt.Fprintln(os.Stderr, "usage: sbxctl cp <id> <local-file|-> <remote-path>")
		return 2
	}
	id, local, remote := rest[0], rest[1], rest[2]
	var data []byte
	var err error
	if local == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(local)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		return 2
	}
	req, _ := http.NewRequest("PUT",
		strings.TrimRight(apiURL, "/")+"/v1/sandboxes/"+id+"/files?path="+remotePathEscape(remote),
		bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		return 2
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		blob, _ := io.ReadAll(resp.Body)
		return printAPIError(resp.StatusCode, blob)
	}
	fmt.Printf("uploaded %d bytes to %s:%s\n", len(data), id, remote)
	return 0
}

func remotePathEscape(p string) string {
	return strings.ReplaceAll(strings.ReplaceAll(p, "%", "%25"), " ", "%20")
}

func nonFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			out = append(out, args[i+1:]...)
			break
		}
		if strings.HasPrefix(args[i], "-") {
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func cmdCat(args []string) int {
	rest := nonFlags(args)
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: sbxctl cat <id> <remote-path>")
		return 2
	}
	req, _ := http.NewRequest("GET",
		strings.TrimRight(apiURL, "/")+"/v1/sandboxes/"+rest[0]+"/files?path="+remotePathEscape(rest[1]), nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		return 2
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		blob, _ := io.ReadAll(resp.Body)
		return printAPIError(resp.StatusCode, blob)
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	if err != nil {
		return 1
	}
	return 0
}

func cmdLs(args []string) int {
	rest := nonFlags(args)
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: sbxctl ls <id> <dir>")
		return 2
	}
	var out struct {
		Files []struct {
			Name  string `json:"name"`
			IsDir bool   `json:"is_dir"`
		} `json:"files"`
	}
	if code := do("GET", "/v1/sandboxes/"+rest[0]+"/files/list?path="+remotePathEscape(rest[1]), nil, &out); code != 0 {
		return code
	}
	for _, f := range out.Files {
		suffix := ""
		if f.IsDir {
			suffix = "/"
		}
		fmt.Println(f.Name + suffix)
	}
	return 0
}

func cmdRm(args []string) int {
	rest := nonFlags(args)
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: sbxctl rm <id> <path>")
		return 2
	}
	return do("DELETE", "/v1/sandboxes/"+rest[0]+"/files?path="+remotePathEscape(rest[1]), nil, nil)
}

func cmdLifecycle(cmd string, args []string) int {
	force := hasFlag(args, "--force")
	id := arg1(args)
	if id == "" {
		fmt.Fprintf(os.Stderr, "usage: sbxctl %s <id>\n", cmd)
		return 2
	}
	switch cmd {
	case "pause":
		var out any
		if code := do("POST", "/v1/sandboxes/"+id+"/pause", nil, &out); code != 0 {
			return code
		}
		pp(out)
		return 0
	case "resume":
		var out any
		if code := do("POST", "/v1/sandboxes/"+id+"/resume", nil, &out); code != 0 {
			return code
		}
		pp(out)
		return 0
	case "stop":
		var out any
		if code := do("POST", "/v1/sandboxes/"+id+"/stop?force="+boolStr(force), nil, &out); code != 0 {
			return code
		}
		pp(out)
		return 0
	case "delete":
		return do("DELETE", "/v1/sandboxes/"+id+"?force="+boolStr(force), nil, nil)
	}
	return 2
}

func hasFlag(args []string, f string) bool {
	for _, a := range args {
		if a == f {
			return true
		}
	}
	return false
}

func cmdTimeout(args []string) int {
	rest := nonFlags(args)
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: sbxctl timeout <id> [seconds]")
		return 2
	}
	body := map[string]any{}
	if len(rest) > 1 {
		n, err := strconv.Atoi(rest[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad seconds:", err)
			return 2
		}
		body["timeout_seconds"] = n
	}
	var out any
	if code := do("POST", "/v1/sandboxes/"+rest[0]+"/timeout", body, &out); code != 0 {
		return code
	}
	pp(out)
	return 0
}

func cmdSnapshots() int {
	var out struct {
		Snapshots []map[string]any `json:"snapshots"`
	}
	if code := do("GET", "/v1/snapshots", nil, &out); code != 0 {
		return code
	}
	for _, sn := range out.Snapshots {
		pp(sn)
	}
	return 0
}

func cmdSnapshot(id string) int {
	var out any
	return do("GET", "/v1/snapshots/"+id, nil, &out)
}

func cmdSnapshotCreate(args []string) int {
	rest := nonFlags(args)
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: sbxctl snapshot-create <sandbox-id> <name>")
		return 2
	}
	var out any
	if code := do("POST", "/v1/snapshots", map[string]any{"sandbox_id": rest[0], "name": rest[1]}, &out); code != 0 {
		return code
	}
	pp(out)
	return 0
}

func cmdDelete(resource, id string) int {
	if id == "" {
		return 2
	}
	return do("DELETE", "/v1/"+resource+"/"+id, nil, nil)
}

func cmdTemplates() int {
	var out struct {
		Templates []map[string]any `json:"templates"`
	}
	if code := do("GET", "/v1/templates", nil, &out); code != 0 {
		return code
	}
	for _, t := range out.Templates {
		pp(t)
	}
	return 0
}

func cmdTenants() int {
	var out struct {
		Tenants []map[string]any `json:"tenants"`
	}
	if code := do("GET", "/v1/tenants", nil, &out); code != 0 {
		return code
	}
	for _, t := range out.Tenants {
		pp(t)
	}
	return 0
}

func cmdTenantCreate(args []string) int {
	rest := nonFlags(args)
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: sbxctl tenant-create <name> [quota.max_sandboxes=10]...")
		return 2
	}
	body := map[string]any{"name": rest[0], "quota": map[string]any{}}
	for _, kv := range rest[1:] {
		k, v, ok := parseKV(kv)
		if !ok {
			continue
		}
		switch k {
		case "quota.max_sandboxes":
			n, _ := strconv.Atoi(v)
			body["quota"].(map[string]any)["max_sandboxes"] = n
		case "quota.max_cpu_millicores":
			n, _ := strconv.Atoi(v)
			body["quota"].(map[string]any)["max_cpu_millicores"] = n
		case "quota.max_memory_mib":
			n, _ := strconv.Atoi(v)
			body["quota"].(map[string]any)["max_memory_mib"] = n
		case "quota.max_snapshots":
			n, _ := strconv.Atoi(v)
			body["quota"].(map[string]any)["max_snapshots"] = n
		}
	}
	var out any
	if code := do("POST", "/v1/tenants", body, &out); code != 0 {
		return code
	}
	pp(out)
	return 0
}

func cmdTenant(id string) int {
	var out any
	if code := do("GET", "/v1/tenants/"+id, nil, &out); code != 0 {
		return code
	}
	pp(out)
	return 0
}

func cmdTenantPatch(args []string) int {
	_ = 0
	rest := nonFlags(args)
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sbxctl tenant-patch <id> status=suspended|active")
		return 2
	}
	body := map[string]any{"base_version": 0}
	for _, kv := range rest[1:] {
		k, v, _ := parseKV(kv)
		if k == "status" {
			body["status"] = v
		}
	}
	var out any
	if code := do("PATCH", "/v1/tenants/"+rest[0], body, &out); code != 0 {
		return code
	}
	pp(out)
	return 0
}

func cmdKeys(tenant string) int {
	if tenant == "" {
		fmt.Fprintln(os.Stderr, "usage: sbxctl keys <tenant>")
		return 2
	}
	var out struct {
		Keys []map[string]any `json:"keys"`
	}
	if code := do("GET", "/v1/tenants/"+tenant+"/keys", nil, &out); code != 0 {
		return code
	}
	for _, k := range out.Keys {
		pp(k)
	}
	return 0
}

func cmdKeyCreate(args []string) int {
	fs := flag.NewFlagSet("key-create", flag.ContinueOnError)
	ttl := fs.Int("ttl", 0, "seconds")
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sbxctl key-create <tenant> <name> [--ttl seconds]")
		return 2
	}
	body := map[string]any{"name": rest[1]}
	if *ttl > 0 {
		body["ttl_seconds"] = *ttl
	}
	var out any
	if code := do("POST", "/v1/tenants/"+rest[0]+"/keys", body, &out); code != 0 {
		return code
	}
	pp(out)
	return 0
}

func cmdKeyRevoke(args []string) int {
	rest := nonFlags(args)
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: sbxctl key-revoke <tenant> <key-id>")
		return 2
	}
	return do("DELETE", "/v1/tenants/"+rest[0]+"/keys/"+rest[1], nil, nil)
}

func cmdEvents(args []string) int {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	tenant := fs.String("tenant", "", "")
	sandbox := fs.String("sandbox", "", "")
	action := fs.String("action", "", "")
	limit := fs.Int("limit", 50, "")
	fs.Parse(args)
	q := fmt.Sprintf("?limit=%d", *limit)
	if *tenant != "" {
		q += "&tenant=" + *tenant
	}
	if *sandbox != "" {
		q += "&sandbox=" + *sandbox
	}
	if *action != "" {
		q += "&action=" + *action
	}
	var out struct {
		Events []map[string]any `json:"events"`
	}
	if code := do("GET", "/v1/events"+q, nil, &out); code != 0 {
		return code
	}
	for i := len(out.Events) - 1; i >= 0; i-- {
		ev := out.Events[i]
		fmt.Printf("%s %s tenant=%v sandbox=%v %v\n",
			ev["ts"], ev["action"], ev["tenant_id"], ev["sandbox_id"], ev["detail"])
	}
	sort.Strings(nil)
	return 0
}
