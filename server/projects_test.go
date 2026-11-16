package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Aiid0/Pyke/agent"
	"github.com/Aiid0/Pyke/db"
)

func projectTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(ctx, m, t.TempDir(), m.dir, t.TempDir())
	s.archiveWG.Wait()
	t.Cleanup(func() { _ = m.Close() })
	token, err := signJWT(s.currentJWTKey())
	if err != nil {
		t.Fatal(err)
	}
	return s, token
}

func projectRequest(t *testing.T, s *Server, token, method, route, contentType string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, route, body)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func uploadProjectFixture(t *testing.T, s *Server, token string, directory bool) *db.Project {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if directory {
		_ = form.WriteField("source_type", "directory")
		for _, rel := range []string{"demo/main.go", "demo/lib/helper.go"} {
			_ = form.WriteField("path", rel)
			file, err := form.CreateFormFile("file", filepath.Base(rel))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(file, "project source")
		}
	} else {
		_ = form.WriteField("source_type", "zip")
		var archive bytes.Buffer
		zw := zip.NewWriter(&archive)
		for _, rel := range []string{"demo/main.go", "demo/lib/helper.go"} {
			f, err := zw.Create(rel)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(f, "project source")
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		file, err := form.CreateFormFile("file", "demo.zip")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.Write(archive.Bytes())
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	w := projectRequest(t, s, token, "POST", "/api/projects/import", form.FormDataContentType(), &body)
	if w.Code != http.StatusCreated {
		t.Fatalf("import status=%d body=%s", w.Code, w.Body.String())
	}
	var p db.Project
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM projects WHERE id=$1`, p.ID) })
	return &p
}

func TestProjectUploadBindsTaskAndSurvivesReload(t *testing.T) {
	s, token := projectTestServer(t)
	p := uploadProjectFixture(t, s, token, false)
	if p.SourceType != "zip" || p.FileCount != 2 {
		t.Fatalf("bad import: %+v", p)
	}
	request, _ := json.Marshal(map[string]any{"name": "project binding test", "description": "fixture", "goal": "review source", "project_id": p.ID})
	w := projectRequest(t, s, token, "POST", "/api/tasks", "application/json", bytes.NewReader(request))
	if w.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	var task TaskDTO
	if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	t.Cleanup(func() { _ = s.m.pg.DeleteTask(id) })
	wantDir := filepath.Join(s.m.dir, "tasks", task.ID, "source")
	if task.Project == nil || task.Project.ID != p.ID || task.ProjectDirectory != wantDir {
		t.Fatalf("binding missing: %+v", task)
	}
	if got, err := os.ReadFile(filepath.Join(wantDir, "lib", "helper.go")); err != nil || string(got) != "project source" {
		t.Fatalf("source tree missing: %s %v", got, err)
	}
	// A later change to the imported project cannot alter this task's snapshot.
	if err := os.WriteFile(filepath.Join(s.m.dir, "projects", p.ID, "source", "main.go"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(wantDir, "main.go")); err != nil || string(got) != "project source" {
		t.Fatalf("snapshot changed: %s %v", got, err)
	}
	stored, err := s.m.pg.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := taskFromPG(stored, s.m.pg.Exploration(stored.ExplorationID), s.m.interceptor, s.m.dir)
	if reloaded.Project == nil || reloaded.Project.ID != p.ID || reloaded.ProjectDirectory != wantDir {
		t.Fatal("reload lost project binding")
	}
	project, err := reloaded.Store.TaskProject()
	if err != nil || project == nil || project.ID != p.ID {
		t.Fatalf("agent project context missing: %+v %v", project, err)
	}
	if context := agent.ProjectContext(project, wantDir, filepath.Dir(wantDir)); !strings.Contains(context, wantDir) {
		t.Fatal("agent context lacks canonical source path")
	}
	listed := projectRequest(t, s, token, "GET", "/api/projects", "", nil)
	if listed.Code != 200 || !strings.Contains(listed.Body.String(), p.ID) {
		t.Fatal("imported project is not reusable")
	}
}

func TestProjectFolderUploadPreservesTree(t *testing.T) {
	s, token := projectTestServer(t)
	p := uploadProjectFixture(t, s, token, true)
	if p.SourceType != "directory" || p.Name != "demo" || p.FileCount != 2 {
		t.Fatalf("folder metadata: %+v", p)
	}
	if _, err := os.Stat(filepath.Join(s.m.dir, "projects", p.ID, "source", "lib", "helper.go")); err != nil {
		t.Fatal(err)
	}
}

func TestProjectBindingFailureRollsBackTaskAndPreparedFiles(t *testing.T) {
	s, token := projectTestServer(t)
	p := uploadProjectFixture(t, s, token, false)
	var before int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM tasks`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	_, err := s.m.CreateTaskWithOptions("rollback project", "review", db.TaskCreateOptions{Project: p, SourceTaskIDs: []int64{9223372036854775000}})
	if err == nil {
		t.Fatal("invalid task relation accepted")
	}
	var after int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM tasks`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("failed creation retained a task row")
	}
	staging, err := filepath.Glob(filepath.Join(s.m.dir, "projects", ".snapshot-*"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("failed snapshot retained: %v %v", staging, err)
	}
	w := projectRequest(t, s, token, "POST", "/api/tasks", "application/json", strings.NewReader(`{"project_id":"missing","goal":"review"}`))
	if w.Code != 400 {
		t.Fatalf("missing project accepted: %d", w.Code)
	}
}

func TestProjectFolderRejectsTraversalAndCleansImport(t *testing.T) {
	s, token := projectTestServer(t)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("source_type", "directory")
	_ = form.WriteField("path", "../escape")
	f, err := form.CreateFormFile("file", "escape")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprint(f, "bad")
	_ = form.Close()
	w := projectRequest(t, s, token, "POST", "/api/projects/import", form.FormDataContentType(), &body)
	if w.Code != 400 {
		t.Fatalf("unsafe path accepted: %d", w.Code)
	}
	entries, err := os.ReadDir(filepath.Join(s.m.dir, "projects"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed import retained files: %v %v", entries, err)
	}
}
