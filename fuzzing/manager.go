package fuzzing

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Runtime interface {
	Check(context.Context) (string, error)
	Start(context.Context, Job, string, string) error
	Inspect(context.Context, Job) (bool, string, error)
	Stop(context.Context, Job) error
}
type Docker struct{ Image, HostRoot, Root string }

func (d Docker) command(ctx context.Context, args ...string) ([]byte, error) {
	ctx, c := context.WithTimeout(ctx, 30*time.Second)
	defer c()
	b, e := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if e != nil {
		return nil, fmt.Errorf("docker %s: %w: %.1000s", args[0], e, b)
	}
	return b, nil
}
func (d Docker) Check(ctx context.Context) (string, error) {
	if d.Image == "" {
		return "", fmt.Errorf("LibAFL 未启用：设置 PYKE_FUZZ_IMAGE 并准备 runner 镜像")
	}
	b, e := d.command(ctx, "image", "inspect", "--format", "{{.Id}}", d.Image)
	return strings.TrimSpace(string(b)), e
}
func containerName(j Job) string { return fmt.Sprintf("pyke-fuzz-%s-%d", j.TaskID, j.ID) }
func (d Docker) Start(ctx context.Context, j Job, dir, image string) error {
	host := dir
	if d.HostRoot != "" {
		rel, e := filepath.Rel(d.Root, dir)
		if e != nil {
			return e
		}
		host = filepath.Join(d.HostRoot, rel)
	}
	// Verify the daemon sees the same volume before launching a build.
	b, e := d.command(ctx, "run", "--rm", "--network=none", "--mount", "type=bind,src="+host+",dst=/job,readonly", "--entrypoint", "cat", image, "/job/mount-token")
	if e != nil {
		return e
	}
	if strings.TrimSpace(string(b)) != j.Snapshot {
		return fmt.Errorf("container workspace mapping mismatch")
	}
	_, e = d.command(ctx, "run", "-d", "--name", containerName(j), "--label", "pyke.fuzz.job="+strconv.FormatInt(j.ID, 10), "--label", "pyke.fuzz.task="+j.TaskID, "--network=none", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=256", "--cpus="+strconv.Itoa(j.Config.CPUs), "--memory="+strconv.Itoa(j.Config.MemoryMB)+"m", "--memory-swap="+strconv.Itoa(j.Config.MemoryMB)+"m", "--read-only", "--tmpfs", "/tmp:rw,nosuid,size=256m", "--mount", "type=bind,src="+host+",dst=/job", "--log-driver=none", image)
	return e
}
func (d Docker) Inspect(ctx context.Context, j Job) (bool, string, error) {
	b, e := d.command(ctx, "inspect", "--format", `{{json .State}}`, containerName(j))
	if e != nil { // Distinguish a missing container from an unavailable daemon.
		if _, err := d.command(ctx, "info", "--format", "{{.ID}}"); err != nil {
			return false, "", err
		}
		return false, "container_missing", nil
	}
	var s struct {
		Running   bool
		OOMKilled bool
		ExitCode  int
	}
	if e = json.Unmarshal(b, &s); e != nil {
		return false, "", e
	}
	reason := fmt.Sprintf("container_exit_%d", s.ExitCode)
	if s.OOMKilled {
		reason = "container_oom"
	}
	return s.Running, reason, nil
}
func (d Docker) Stop(ctx context.Context, j Job) error {
	_, e := d.command(ctx, "rm", "-f", containerName(j))
	if e != nil {
		alive, reason, err := d.Inspect(ctx, j)
		if err == nil && !alive && reason == "container_missing" {
			return nil
		}
	}
	return e
}

type Manager struct {
	Store   Store
	Root    string
	Runtime Runtime
	// Allowed checks the server's pause/deletion/queue barriers. Wake re-admits a
	// task after explicit user work and notifies its existing worker pool.
	Allowed func(string) bool
	Wake    func(string)
	mu      sync.Mutex
}

func (m *Manager) Dir(j Job) string {
	return filepath.Join(m.Root, "tasks", j.TaskID, "shared", "fuzz", strconv.FormatInt(j.ID, 10))
}
func sampleName(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, e := hex.DecodeString(s)
	return e == nil
}
func inside(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("expected workspace-relative path")
	}
	p, e := filepath.EvalSymlinks(filepath.Join(root, rel))
	if e != nil {
		return "", e
	}
	r, e := filepath.Rel(root, p)
	if e != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes task workspace")
	}
	return p, nil
}
func (m *Manager) Create(ctx context.Context, task string, exp int64, creator string, c Config) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := c.Defaults(); e != nil {
		return Job{}, e
	}
	if m.Allowed != nil && !m.Allowed(task) {
		return Job{}, fmt.Errorf("任务已暂停、排队或正在删除")
	}
	if _, e := m.Runtime.Check(ctx); e != nil {
		return Job{}, e
	}
	root, e := filepath.EvalSymlinks(filepath.Join(m.Root, "tasks", task))
	if e != nil {
		return Job{}, e
	}
	source, e := inside(root, c.Source)
	if e != nil {
		return Job{}, e
	}
	outputRel, e := filepath.Rel(source, filepath.Join(root, "shared", "fuzz"))
	if e != nil || (outputRel != ".." && !strings.HasPrefix(outputRel, ".."+string(filepath.Separator))) {
		return Job{}, fmt.Errorf("source must not contain fuzz output directory")
	}
	if strings.Contains(filepath.ToSlash(c.Source), "shared/fuzz") {
		return Job{}, fmt.Errorf("source must not contain fuzz output")
	}
	for _, p := range []string{c.Build, c.Seeds} {
		if _, e = inside(source, p); e != nil {
			return Job{}, e
		}
	}
	if c.Dictionary != "" {
		if _, e = inside(source, c.Dictionary); e != nil {
			return Job{}, e
		}
	}
	return m.Store.Create(task, exp, creator, c)
}
func copySnapshot(source, dest string) (string, error) {
	h := sha256.New()
	var total int64
	e := filepath.WalkDir(source, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dest, rel), 0755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("snapshot contains symlink: %s", rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("snapshot requires regular files")
		}
		total += info.Size()
		if total > 1<<30 {
			return fmt.Errorf("snapshot exceeds 1 GiB")
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(filepath.Join(dest, rel), os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), info.Size())
		_, err = io.Copy(io.MultiWriter(out, h), in)
		ce := out.Close()
		if err != nil {
			return err
		}
		return ce
	})
	return hex.EncodeToString(h.Sum(nil)), e
}
func (m *Manager) prepare(ctx context.Context, j Job) error {
	dir := m.Dir(j)
	if e := os.MkdirAll(dir, 0755); e != nil {
		return e
	}
	root, e := filepath.EvalSymlinks(filepath.Join(m.Root, "tasks", j.TaskID))
	if e != nil {
		return e
	}
	source, e := inside(root, j.Config.Source)
	if e != nil {
		return e
	}
	j.Snapshot, e = copySnapshot(source, filepath.Join(dir, "source"))
	if e != nil {
		return e
	}
	j.Image, e = m.Runtime.Check(ctx)
	if e != nil {
		return e
	}
	if j.Config.ParentID > 0 {
		parent, e := m.Store.Get(j.TaskID, j.Config.ParentID)
		if e != nil {
			return e
		}
		pd := m.Dir(parent)
		if j.Config.Replay != "" {
			if !sampleName(j.Config.Replay) {
				return fmt.Errorf("invalid replay sample")
			}
			b, e := os.ReadFile(filepath.Join(pd, "samples", j.Config.Replay))
			if e != nil {
				return e
			}
			if e = os.WriteFile(filepath.Join(dir, "replay"), b, 0644); e != nil {
				return e
			}
		} else if _, e = os.Stat(filepath.Join(pd, "corpus")); e == nil {
			if _, e = copySnapshot(filepath.Join(pd, "corpus"), filepath.Join(dir, "resume")); e != nil {
				return e
			}
		}
	}
	b, _ := json.Marshal(j.Config)
	if e = os.WriteFile(filepath.Join(dir, "config.json"), b, 0644); e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(dir, "mount-token"), []byte(j.Snapshot), 0644); e != nil {
		return e
	}
	if _, e = m.Store.DB.Exec(`UPDATE fuzz_jobs SET image=$2,snapshot=$3 WHERE id=$1`, j.ID, j.Image, j.Snapshot); e != nil {
		return e
	}
	return m.Runtime.Start(ctx, j, dir, j.Image)
}
func (m *Manager) Stop(ctx context.Context, task string, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, e := m.Store.Get(task, id)
	if e != nil {
		return e
	}
	if !Active(j.State) {
		return nil
	}
	if j.State != "queued" {
		if e = m.Store.State(id, "stopping", "user_stop"); e != nil {
			return e
		}
		if e = m.Runtime.Stop(ctx, j); e != nil {
			return e
		}
		_ = m.collect(ctx, j)
	}
	return m.Store.State(id, "stopped", "user_stop")
}
func (m *Manager) StopTask(ctx context.Context, task string) error {
	js, e := m.Store.List(task)
	if e != nil {
		return e
	}
	for _, j := range js {
		if Active(j.State) {
			if e = m.Stop(ctx, task, j.ID); e != nil {
				return e
			}
		}
	}
	return nil
}
func (m *Manager) Continue(ctx context.Context, task string, id int64, request, sample string) (Job, error) {
	j, e := m.Store.Get(task, id)
	if e != nil {
		return Job{}, e
	}
	c := j.Config
	c.RequestID = request
	c.ParentID = id
	c.Replay = sample
	c.AutoAnalyze = false
	// Rebuild the frozen source, not the potentially edited original workspace.
	// Create validates user source paths separately; this trusted continuation only
	// refers to a previously owned job snapshot.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Allowed != nil && !m.Allowed(task) {
		return Job{}, fmt.Errorf("task unavailable")
	}
	if !sampleName(sample) && sample != "" {
		return Job{}, fmt.Errorf("invalid sample")
	}
	if sample != "" {
		if _, e = os.Stat(filepath.Join(m.Dir(j), "samples", sample)); e != nil {
			return Job{}, e
		}
	}
	c.Source = filepath.ToSlash(filepath.Join("shared", "fuzz", strconv.FormatInt(id, 10), "source"))
	if e = c.Defaults(); e != nil {
		return Job{}, e
	}
	return m.Store.Create(task, j.ExplorationID, "continuation", c)
}
func (m *Manager) collect(ctx context.Context, j Job) error {
	f, e := os.Open(filepath.Join(m.Dir(j), "events.jsonl"))
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 128<<10)
	for {
		line, e := r.ReadSlice('\n')
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return e
		}
		if len(line) > 128<<10 {
			return fmt.Errorf("runner event too large")
		}
		var ev Event
		if e = json.Unmarshal(line, &ev); e != nil {
			return e
		}
		if ev.Seq > j.Cursor {
			if e = m.Store.Apply(ctx, j.ID, ev); e != nil {
				return e
			}
		}
	}
}
func (m *Manager) Tick(ctx context.Context) {
	m.mu.Lock()
	wakes := map[string]bool{}
	defer func() {
		m.mu.Unlock()
		if m.Wake != nil {
			for task := range wakes {
				m.Wake(task)
			}
		}
	}()
	jobs, e := m.Store.List("")
	if e != nil {
		return
	}
	running := 0
	tasks := map[string]bool{}
	for _, j := range jobs {
		if j.State != "queued" && Active(j.State) {
			running++
			tasks[j.TaskID] = true
		}
	}
	// Oldest queued job first.
	for i := len(jobs) - 1; i >= 0; i-- {
		j := jobs[i]
		allowed := m.Allowed == nil || m.Allowed(j.TaskID)
		if !allowed {
			continue
		}
		if j.State == "queued" && running < 2 && !tasks[j.TaskID] {
			if e = m.Store.State(j.ID, "preparing", ""); e != nil {
				continue
			}
			if e = m.prepare(ctx, j); e != nil {
				_ = m.Store.State(j.ID, "failed", e.Error())
			} else {
				running++
				tasks[j.TaskID] = true
			}
			continue
		}
		if Active(j.State) && j.State != "queued" {
			if e = m.collect(ctx, j); e != nil {
				if stopErr := m.Runtime.Stop(ctx, j); stopErr == nil {
					_ = m.Store.State(j.ID, "failed", "event_stream: "+e.Error())
					wakes[j.TaskID] = true
				}
				continue
			}
			alive, reason, err := m.Runtime.Inspect(ctx, j)
			if err != nil {
				continue
			}
			fresh, err := m.Store.Get(j.TaskID, j.ID)
			if err != nil {
				continue
			}
			if !alive && Active(fresh.State) {
				_ = m.Store.State(j.ID, "interrupted", reason)
			}
			if !alive || !Active(fresh.State) {
				_ = m.Runtime.Stop(ctx, j)
				wakes[j.TaskID] = true
			}
		}
		fresh, err := m.Store.Get(j.TaskID, j.ID)
		if err == nil {
			if ok, err := m.Store.Dispatch(fresh); err == nil && ok {
				wakes[j.TaskID] = true
			}
		}
	}
}
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Tick(ctx)
		}
	}
}
func (m *Manager) Logs(j Job, offset int64) (map[string]any, error) {
	if offset < 0 {
		return nil, fmt.Errorf("invalid offset")
	}
	f, e := os.Open(filepath.Join(m.Dir(j), "run.log"))
	if errors.Is(e, os.ErrNotExist) {
		return map[string]any{"text": "", "next_offset": offset}, nil
	}
	if e != nil {
		return nil, e
	}
	defer f.Close()
	if _, e = f.Seek(offset, 0); e != nil {
		return nil, e
	}
	b, e := io.ReadAll(io.LimitReader(f, 64<<10))
	return map[string]any{"text": string(b), "next_offset": offset + int64(len(b))}, e
}
