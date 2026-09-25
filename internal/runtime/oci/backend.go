package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/runtime"
)

const (
	labelProduct = "product"
	labelSandbox = "sandbox.id"
	labelTenant  = "sandbox.tenant"
	prodValue    = "sandboxd"

	defaultOutputCap = 1 << 20 // 1 MiB per stream
	startProbeDelay  = 100 * time.Millisecond
	startProbeMax    = 10 * time.Second
)

// Backend implements runtime.Backend with Docker containers.
type Backend struct {
	c *Client
}

func New(c *Client) *Backend { return &Backend{c: c} }

func (b *Backend) Name() string { return "oci" }

func (b *Backend) Ping(ctx context.Context) error { return b.c.ping(ctx) }

// ---- container API payload types -------------------------------------------

type createBody struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd"`
	Env        []string          `json:"Env,omitempty"`
	User       string            `json:"User,omitempty"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	Labels     map[string]string `json:"Labels"`
	HostConfig hostConfig        `json:"HostConfig"`
}

type hostConfig struct {
	Resources     resources `json:"Resources"`
	NetworkMode   string    `json:"NetworkMode"`
	CapDrop       []string  `json:"CapDrop"`
	SecurityOpt   []string  `json:"SecurityOpt"`
	Init          *bool     `json:"Init,omitempty"`
	RestartPolicy struct {
		Name string `json:"Name"`
	} `json:"RestartPolicy"`
}

type resources struct {
	NanoCPUs   int64  `json:"NanoCpus"`   // 1e9 = one core
	Memory     int64  `json:"Memory"`     // bytes
	MemorySwap int64  `json:"MemorySwap"` // == Memory → swap disabled
	PidsLimit  *int64 `json:"PidsLimit"`
}

type inspectBody struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status     string `json:"Status"` // created|running|paused|restarting|removing|exited|dead
		Running    bool   `json:"Running"`
		Paused     bool   `json:"Paused"`
		ExitCode   int    `json:"ExitCode"`
		OOMKilled  bool   `json:"OOMKilled"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Error      string `json:"Error"`
	} `json:"State"`
	Labels map[string]string `json:"Config.Labels"`
}

func (i *inspectBody) toStatus() runtime.Status {
	st := runtime.Status{
		Running:   i.State.Running,
		Paused:    i.State.Paused,
		Raw:       i.State.Status,
		ExitCode:  i.State.ExitCode,
		OOMKilled: i.State.OOMKilled,
	}
	if t, err := time.Parse(time.RFC3339Nano, i.State.StartedAt); err == nil && !t.IsZero() {
		st.StartedAt = t
	}
	return st
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// ---- lifecycle ---------------------------------------------------------------

func (b *Backend) Create(ctx context.Context, req runtime.CreateRequest) (string, error) {
	pids := int64(512)
	body := createBody{
		Image:      req.Image,
		Cmd:        []string{"/bin/sh", "-c", "exec sleep infinity"}, // stable PID 1; workloads run via exec
		Env:        envSlice(req.Env),
		User:       req.User,
		WorkingDir: req.WorkDir,
		Labels:     withProduct(req.Labels),
		HostConfig: hostConfig{
			Resources: resources{
				NanoCPUs:   int64(req.CPUMillicores) * 1_000_000,
				Memory:     int64(req.MemoryMiB) << 20,
				MemorySwap: int64(req.MemoryMiB) << 20, // swap off: an OOM must hit this sandbox only, and visibly
				PidsLimit:  &pids,
			},
			NetworkMode: "none",
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			Init:        boolPtr(true), // reap zombies from workload execs
			RestartPolicy: struct {
				Name string `json:"Name"`
			}{Name: "no"},
		},
	}
	payload, _ := json.Marshal(body)
	respBody, err := b.c.do(ctx, "POST", "/containers/create?name="+urlQueryEscape(req.Name), nil, bytes.NewReader(payload))
	if err != nil {
		if ae, ok := asAppErr(err); ok && ae.Code == apperr.NotFound {
			return "", apperr.Invalid("base image %q not present on this node; import a rootfs or reference an available image", req.Image)
		}
		return "", err
	}
	var cr struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(respBody, &cr); err != nil || cr.ID == "" {
		return "", apperr.InternalErr("docker create returned unusable body: %v", err)
	}
	return req.Name, nil
}

func withProduct(labels map[string]string) map[string]string {
	out := map[string]string{labelProduct: prodValue}
	for k, v := range labels {
		out[k] = v
	}
	return out
}

func boolPtr(v bool) *bool { return &v }

// Start flips the container to running and then verifies the transition.
// Docker's 204 means "request accepted": PID 1 can still die instantly
// (broken image, instant OOM) and callers must not be told "running".
func (b *Backend) Start(ctx context.Context, ref string) error {
	if _, err := b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/start", nil, nil); err != nil {
		if ae, ok := asAppErr(err); ok && ae.Code == apperr.Conflict {
			return nil // already running — idempotent
		}
		return err
	}
	deadline := time.Now().Add(startProbeMax)
	for {
		ins, err := b.inspect(ctx, ref)
		if err != nil {
			return err
		}
		switch {
		case ins.State.Running:
			return nil
		case ins.State.OOMKilled:
			return apperr.ConflictErr("sandbox process was killed by the memory limiter (OOM) on start; raise memory_mib")
		case ins.State.Status == "exited" || ins.State.Status == "dead":
			msg := fmt.Sprintf("sandbox process exited immediately with code %d", ins.State.ExitCode)
			switch {
			case ins.State.ExitCode == 137:
				msg += " (exit 137 = killed by the memory limiter; raise memory_mib)"
			case ins.State.ExitCode == 126 || ins.State.ExitCode == 127:
				msg += " (command not executable — image is missing its interpreter or linker)"
			}
			return apperr.ConflictErr("%s", msg)
		}
		if time.Now().After(deadline) {
			return apperr.Unavailable("container %s did not reach running within %s", ref, startProbeMax)
		}
		select {
		case <-ctx.Done():
			return apperr.From(ctx.Err())
		case <-time.After(startProbeDelay):
		}
	}
}

func (b *Backend) Pause(ctx context.Context, ref string) error {
	ins, err := b.inspect(ctx, ref)
	if err != nil {
		return err
	}
	if ins.State.Paused {
		return nil // idempotent by real phase, not by error-code guessing
	}
	if !ins.State.Running {
		return apperr.ConflictErr("cannot pause sandbox in state %q", ins.State.Status)
	}
	_, err = b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/pause", nil, nil)
	return err
}

func (b *Backend) Resume(ctx context.Context, ref string) error {
	ins, err := b.inspect(ctx, ref)
	if err != nil {
		return err
	}
	if !ins.State.Paused {
		return nil
	}
	_, err = b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/unpause", nil, nil)
	return err
}

// Stop terminates the container. grace gives clean shutdown a window before
// the signal escalates; force kills immediately.
func (b *Backend) Stop(ctx context.Context, ref string, force bool) error {
	if force {
		_, err := b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/kill", nil, nil)
		if ae, ok := asAppErr(err); ok && ae.Code == apperr.NotFound {
			return nil
		}
		return err
	}
	if _, err := b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/stop?t=10", nil, nil); err != nil {
		ae, ok := asAppErr(err)
		if ok && (ae.Code == apperr.NotFound || ae.Code == apperr.Conflict) {
			return nil // gone already, or stopped already
		}
		return err
	}
	return nil
}

func (b *Backend) Remove(ctx context.Context, ref string) error {
	// A wedged container (e.g. process table exhausted) can answer removal
	// with 409 "removal already in progress" while it tears down — retry
	// briefly before giving up; force-delete must always eventually win.
	var lastErr error
	for attempt := 0; attempt < 16; attempt++ {
		_, err := b.c.do(ctx, "DELETE", "/containers/"+urlQueryEscape(ref)+"?force=true&v=true", nil, nil)
		if err == nil {
			return nil
		}
		if ae, ok := asAppErr(err); ok && ae.Code == apperr.NotFound {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return apperr.From(ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	return lastErr
}

func (b *Backend) inspect(ctx context.Context, ref string) (*inspectBody, error) {
	body, err := b.c.do(ctx, "GET", "/containers/"+urlQueryEscape(ref)+"/json", nil, nil)
	if err != nil {
		return nil, err
	}
	var ins inspectBody
	if err := json.Unmarshal(body, &ins); err != nil {
		return nil, apperr.InternalErr("docker inspect unparseable: %v", err)
	}
	return &ins, nil
}

func (b *Backend) Inspect(ctx context.Context, ref string) (runtime.Status, error) {
	ins, err := b.inspect(ctx, ref)
	if err != nil {
		return runtime.Status{}, err
	}
	return ins.toStatus(), nil
}

func (b *Backend) Rename(ctx context.Context, ref, newName string) error {
	_, err := b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/rename?name="+urlQueryEscape(newName), nil, nil)
	return err
}

// ---- exec ----------------------------------------------------------------

type execCreateBody struct {
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env,omitempty"`
	User         string   `json:"User,omitempty"`
	WorkingDir   string   `json:"WorkingDir,omitempty"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
}

func (b *Backend) Exec(ctx context.Context, ref string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	if len(req.Argv) == 0 {
		return nil, apperr.Invalid("exec requires a non-empty argv")
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultExecTO
	}
	// Outer ctx covers create+inspect calls; the exec stream itself runs on
	// execCtx so its read loop unblocks exactly at the deadline instead of
	// hanging on a silent stream until the outer context expires.
	execCtx, cancelExec := context.WithTimeout(ctx, timeout)
	defer cancelExec()
	ctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()

	payload, _ := json.Marshal(execCreateBody{
		Cmd:          req.Argv,
		Env:          envSlice(req.Env),
		User:         req.User,
		WorkingDir:   req.WorkDir,
		AttachStdout: true,
		AttachStderr: true,
	})
	body, err := b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/exec", nil, bytes.NewReader(payload))
	if err != nil {
		// A workload that exhausted the pids limit wedges the container:
		// even docker exec cannot setns to spawn. Surface an actionable
		// conflict instead of leaking OCI internals.
		if ae, ok := asAppErr(err); ok && ae.Code == apperr.Conflict &&
			(strings.Contains(ae.Message, "setns") || strings.Contains(ae.Message, "unable to start container process")) {
			return nil, apperr.ConflictErr(
				"sandbox process table is exhausted (fork bomb or process leak); "+
					"it recovers when its processes exit, or stop/delete and recreate the sandbox",
			)
		}
		return nil, err
	}
	var ec struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(body, &ec); err != nil || ec.ID == "" {
		return nil, apperr.InternalErr("docker exec create returned unusable body")
	}
	started := time.Now()
	res, execErr := b.execStart(execCtx, ec.ID, started, timeout, req.MaxOutput)
	if execErr == nil {
		// final state read gives the authoritative exit code — must happen
		// BEFORE OCI-start-failure detection, which keys on exit code 128
		if ins, err := b.execInspect(ctx, ec.ID); err == nil && ins.ExitCode != nil {
			res.ExitCode = *ins.ExitCode
		}
		// Docker reports exec-START failures (OCI runtime errors) as a
		// normal stream with exit code 128 and the error in the output —
		// recognize them and translate into actionable conflicts.
		combined := string(res.Stdout) + string(res.Stderr)
		if res.ExitCode == 128 && strings.Contains(combined, "OCI runtime exec failed") {
			if strings.Contains(combined, "setns") || strings.Contains(combined, "unable to start container process") {
				execErr = apperr.ConflictErr(
					"sandbox process table is exhausted (fork bomb or process leak); "+
						"it recovers when its processes exit, or stop/delete and recreate the sandbox",
				)
			} else {
				execErr = apperr.ConflictErr("process could not be started in the sandbox: %s",
					strings.TrimSpace(strings.SplitN(combined, "\n", 2)[0]))
			}
		}
	}
	if execErr != nil {
		// best-effort kill so a timed-out process does not keep burning quota
		_, _ = b.c.do(context.Background(), "DELETE", "/exec/"+urlQueryEscape(ec.ID), nil, nil)
		return nil, execErr
	}
	res.Duration = time.Since(started)
	return res, nil
}

type execStartBody struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}

func (b *Backend) execStart(execCtx context.Context, execID string, started time.Time, timeout time.Duration, maxOut int) (*runtime.ExecResult, error) {
	payload, _ := json.Marshal(execStartBody{})
	resp, closer, err := b.c.raw(execCtx, "POST", "/exec/"+urlQueryEscape(execID)+"/start", nil, bytes.NewReader(payload),
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return nil, err
	}
	defer closer()

	cap := int64(maxOut)
	if cap <= 0 {
		cap = defaultOutputCap
	}
	deadline := started.Add(timeout)
	res := &runtime.ExecResult{}
	var stdout, stderr bytes.Buffer

	buf := make([]byte, 64*1024)
	for {
		// The deadline is enforced by execCtx cancelling the underlying
		// read; check it here too for the no-data fast path.
		if execCtx.Err() != nil || time.Now().After(deadline) {
			res.TimedOut = true
			res.Stdout = stdout.Bytes()
			res.Stderr = stderr.Bytes()
			res.Truncated = stdout.Len() >= int(cap) || stderr.Len() >= int(cap)
			return res, nil
		}
		header := make([]byte, 8)
		if _, err := io.ReadFull(resp.Body, header); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break // stream finished
			}
			if execCtx.Err() != nil {
				res.TimedOut = true
				res.Stdout = stdout.Bytes()
				res.Stderr = stderr.Bytes()
				return res, nil
			}
			return nil, apperr.Unavailable("docker exec stream: %v", err)
		}
		streamType := header[0]
		size := int64(header[4])<<24 | int64(header[5])<<16 | int64(header[6])<<8 | int64(header[7])
		if size <= 0 {
			continue
		}
		dst := &stdout
		if streamType == 2 {
			dst = &stderr
		}
		for remaining := size; remaining > 0; {
			n := int64(len(buf))
			if n > remaining {
				n = remaining
			}
			nr, err := io.ReadFull(resp.Body, buf[:n])
			if nr > 0 {
				remaining -= int64(nr)
				if int64(dst.Len()) < cap {
					room := cap - int64(dst.Len())
					if room > int64(nr) {
						room = int64(nr)
					}
					dst.Write(buf[:room])
				}
				if int64(dst.Len()) >= cap {
					res.Truncated = true
				}
			}
			if err != nil {
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					return nil, apperr.Unavailable("docker exec stream cut off")
				}
				return nil, apperr.Unavailable("docker exec stream: %v", err)
			}
		}
	}
	res.Stdout = stdout.Bytes()
	res.Stderr = stderr.Bytes()
	return res, nil
}

func (b *Backend) execInspect(ctx context.Context, execID string) (*struct {
	Running  *bool `json:"Running"`
	ExitCode *int  `json:"ExitCode"`
}, error) {
	body, err := b.c.do(ctx, "GET", "/exec/"+urlQueryEscape(execID)+"/json", nil, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Running  *bool `json:"Running"`
		ExitCode *int  `json:"ExitCode"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, apperr.InternalErr("docker exec inspect unparseable: %v", err)
	}
	return &out, nil
}

// ---- images & snapshots ------------------------------------------------------

func (b *Backend) Commit(ctx context.Context, ref, tag string) (string, int64, error) {
	if _, err := b.c.do(ctx, "POST",
		"/commit?container="+urlQueryEscape(ref)+"&repo="+urlQueryEscape(tag), nil, nil); err != nil {
		return "", 0, err
	}
	size, err := b.imageSize(ctx, tag)
	if err != nil {
		return tag, 0, err
	}
	return tag, size, nil
}

func (b *Backend) HasImage(ctx context.Context, imageRef string) (bool, error) {
	_, err := b.c.do(ctx, "GET", "/images/"+urlQueryEscape(imageRef)+"/json", nil, nil)
	if err != nil {
		if ae, ok := asAppErr(err); ok && ae.Code == apperr.NotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (b *Backend) RemoveImage(ctx context.Context, imageRef string) error {
	_, err := b.c.do(ctx, "DELETE", "/images/"+urlQueryEscape(imageRef)+"?force=true", nil, nil)
	if ae, ok := asAppErr(err); ok && ae.Code == apperr.NotFound {
		return nil
	}
	return err
}

func (b *Backend) imageSize(ctx context.Context, tag string) (int64, error) {
	body, err := b.c.do(ctx, "GET", "/images/"+urlQueryEscape(tag)+"/json", nil, nil)
	if err != nil {
		return 0, err
	}
	var im struct {
		Size int64 `json:"Size"`
	}
	if err := json.Unmarshal(body, &im); err != nil {
		return 0, apperr.InternalErr("image inspect unparseable: %v", err)
	}
	return im.Size, nil
}

// Orphans returns every container labeled as ours, regardless of state.
func (b *Backend) Orphans(ctx context.Context) ([]runtime.Orphan, error) {
	filters := fmt.Sprintf(`{"label":["%s=%s"]}`, labelProduct, prodValue)
	body, err := b.c.do(ctx, "GET", "/containers/json", map[string]string{"all": "1", "filters": filters}, nil)
	if err != nil {
		return nil, err
	}
	var list []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		State  string            `json:"State"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, apperr.InternalErr("docker container list unparseable: %v", err)
	}
	out := make([]runtime.Orphan, 0, len(list))
	for _, it := range list {
		ref := it.ID
		if len(it.Names) > 0 && len(it.Names[0]) > 1 {
			ref = strings.TrimPrefix(it.Names[0], "/")
		}
		ins, err := b.inspect(ctx, it.ID)
		if err != nil {
			continue
		}
		out = append(out, runtime.Orphan{
			Ref:    ref,
			Labels: it.Labels,
			Status: ins.toStatus(),
		})
	}
	return out, nil
}
