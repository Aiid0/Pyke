package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Aiid0/Pyke/db"
	"github.com/Aiid0/Pyke/projectsource"
	"github.com/google/uuid"
)

var projectImportSlots = make(chan struct{}, 2)

type projectDTO struct {
	*db.Project
	Directory string `json:"directory"`
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.m.pg.ListProjects()
	if err != nil {
		writeErr(w, 500, "无法加载项目")
		return
	}
	out := make([]projectDTO, 0, len(projects))
	for _, p := range projects {
		out = append(out, projectDTO{Project: p, Directory: filepath.Join(s.m.dir, "projects", p.ID, "source")})
	}
	writeJSON(w, 200, map[string]any{"projects": out})
}

func (s *Server) importProject(w http.ResponseWriter, r *http.Request) {
	select {
	case projectImportSlots <- struct{}{}:
		defer func() { <-projectImportSlots }()
	default:
		writeErr(w, http.StatusTooManyRequests, "已有项目正在导入，请稍后重试")
		return
	}
	root := filepath.Join(s.m.dir, "projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		writeErr(w, 500, "无法创建项目目录")
		return
	}
	stage, err := os.MkdirTemp(root, ".import-")
	if err != nil {
		writeErr(w, 500, "无法准备项目目录")
		return
	}
	defer os.RemoveAll(stage)
	source := filepath.Join(stage, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		writeErr(w, 500, "无法准备源码目录")
		return
	}
	p := db.Project{ID: uuid.NewString()}
	var historyUsername, historyToken string
	var stats projectsource.Stats
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, projectsource.MaxUploadBytes)
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
			writeErr(w, http.StatusBadRequest, "上传失败：项目文件总大小需小于 512 MiB")
			return
		}
		defer r.MultipartForm.RemoveAll()
		p.Name = strings.TrimSpace(r.FormValue("name"))
		files := r.MultipartForm.File["file"]
		if len(files) == 0 {
			writeErr(w, 400, "请选择 ZIP 或本地项目文件夹")
			return
		}
		if r.FormValue("source_type") == "zip" {
			if len(files) != 1 || !strings.HasSuffix(strings.ToLower(files[0].Filename), ".zip") {
				writeErr(w, 400, "请选择一个 ZIP 项目文件")
				return
			}
			p.SourceType = "zip"
			if p.Name == "" {
				p.Name = strings.TrimSuffix(files[0].Filename, filepath.Ext(files[0].Filename))
			}
			file, openErr := files[0].Open()
			if openErr != nil {
				writeErr(w, 400, "无法读取上传文件")
				return
			}
			stats, err = projectsource.ExtractZIP(file, files[0].Size, source)
			_ = file.Close()
			if err == nil {
				err = flattenProjectRoot(source)
			}
		} else {
			p.SourceType = "directory"
			paths := r.MultipartForm.Value["path"]
			if len(paths) != len(files) || len(files) > projectsource.MaxFiles {
				writeErr(w, 400, "项目目录结构无效或文件数量过多")
				return
			}
			for _, rel := range paths {
				if _, err := projectsource.RelativePath(rel); err != nil {
					writeErr(w, 400, err.Error())
					return
				}
			}
			prefix := strings.Split(paths[0], "/")[0] + "/"
			for _, rel := range paths {
				if !strings.HasPrefix(rel, prefix) {
					prefix = ""
					break
				}
			}
			if p.Name == "" && prefix != "" {
				p.Name = strings.TrimSuffix(prefix, "/")
			}
			for i, hdr := range files {
				rel := strings.TrimPrefix(paths[i], prefix)
				file, openErr := hdr.Open()
				if openErr != nil {
					err = openErr
					break
				}
				err = stats.WriteFile(source, rel, file)
				_ = file.Close()
				if err != nil {
					break
				}
			}
			if err == nil && stats.Files == 0 {
				err = errors.New("项目中没有可导入的文件")
			}
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var request struct {
			Name     string `json:"name"`
			URL      string `json:"repository_url"`
			Branch   string `json:"branch"`
			Username string `json:"username"`
			Token    string `json:"token"`
		}
		if err := decode(r, &request); err != nil {
			writeErr(w, 400, "项目导入参数无效")
			return
		}
		p.SourceType = "git"
		historyUsername, historyToken = request.Username, request.Token
		p.Name = strings.TrimSpace(request.Name)
		p.RepositoryURL, err = projectsource.RepositoryURL(request.URL)
		if err == nil {
			if p.Name == "" {
				u, _ := url.Parse(p.RepositoryURL)
				p.Name = strings.TrimSuffix(path.Base(u.Path), ".git")
			}
			clone := filepath.Join(stage, "clone")
			p.Branch, p.Revision, err = projectsource.Clone(r.Context(), clone, p.RepositoryURL, strings.TrimSpace(request.Branch), request.Username, request.Token)
			if err == nil {
				stats, err = projectsource.Copy(clone, source)
			}
		}
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if p.Name == "" {
		p.Name = "本地项目"
	}
	if utf8.RuneCountInString(p.Name) > 200 {
		writeErr(w, 400, "项目名称最多 200 个字符")
		return
	}
	p.FileCount, p.TotalBytes = stats.Files, stats.Bytes
	// Keep Git objects outside the validated source tree and task snapshots.
	final := filepath.Join(root, p.ID)
	if err := os.Mkdir(final, 0o755); err != nil {
		writeErr(w, 500, "无法保存项目")
		return
	}
	if err := os.Rename(source, filepath.Join(final, "source")); err != nil {
		_ = os.RemoveAll(final)
		writeErr(w, 500, "无法保存项目文件")
		return
	}
	if p.SourceType == "git" {
		if err := seedProjectHistory(r.Context(), filepath.Join(stage, "clone", ".git"), filepath.Join(final, "source")); err != nil {
			_ = os.RemoveAll(final)
			writeErr(w, 500, "无法保存项目 Git 历史")
			return
		}
	}
	created, err := s.m.pg.CreateProject(p)
	if err != nil {
		_ = os.RemoveAll(final)
		writeErr(w, 500, "无法保存项目信息")
		return
	}
	if created.SourceType == "git" && s.ctx.Err() == nil {
		_, _ = s.startHistorySync(created, historyUsername, historyToken)
	}
	writeJSON(w, http.StatusCreated, projectDTO{Project: created, Directory: filepath.Join(final, "source")})
}

// seedProjectHistory gives the validated source tree the clone's Git metadata.
// The transient clone uses environment-only credentials.
func seedProjectHistory(ctx context.Context, gitDir, source string) error {
	if err := os.Chmod(gitDir, 0700); err != nil {
		return err
	}
	if err := os.Rename(gitDir, filepath.Join(source, ".git")); err != nil {
		return err
	}
	head, err := historyGit(ctx, source, "", "", "rev-parse", "HEAD")
	if err != nil || !historySHAPattern.MatchString(head) {
		return errors.New("无法验证项目 Git 工作目录")
	}
	return nil
}

func flattenProjectRoot(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return err
	}
	wrapper := root + ".wrapper"
	if err := os.Rename(root, wrapper); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(wrapper, entries[0].Name()), root); err != nil {
		return err
	}
	return os.Remove(wrapper)
}

// prepareProjectSnapshot copies before the DB transaction, then publishes with
// one rename inside it. Failure leaves neither a half-created task nor its files.
func (m *Manager) prepareProjectSnapshot(opts *db.TaskCreateOptions) (func(), error) {
	if opts.Project == nil {
		return func() {}, nil
	}
	id, err := uuid.Parse(opts.Project.ID)
	if err != nil {
		return nil, errors.New("项目 ID 无效")
	}
	root := filepath.Join(m.dir, "projects")
	stage, err := os.MkdirTemp(root, ".snapshot-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(stage) }
	if _, err := projectsource.Copy(filepath.Join(root, id.String(), "source"), filepath.Join(stage, "source")); err != nil {
		cleanup()
		return nil, fmt.Errorf("无法准备任务源码快照: %w", err)
	}
	opts.PrepareProject = func(taskID int64) (func(), error) {
		taskDir := filepath.Join(m.dir, "tasks", strconv.FormatInt(taskID, 10))
		if err := os.MkdirAll(taskDir, 0o755); err != nil {
			return nil, err
		}
		target := filepath.Join(taskDir, "source")
		if _, err := os.Lstat(target); err == nil {
			return nil, errors.New("任务源码目录已存在")
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		if err := os.Rename(filepath.Join(stage, "source"), target); err != nil {
			return nil, err
		}
		if opts.Project.SourceType == "git" {
			projectSource := filepath.Join(root, id.String(), "source")
			if err := ensureTaskGitView(context.Background(), opts.Project, projectSource, target); err != nil {
				// Git metadata is auxiliary; the validated source snapshot remains usable.
				log.Printf("[project-git] task %d Git view: %v", taskID, err)
			}
		}
		return func() { _ = os.RemoveAll(target); _ = os.Remove(taskDir) }, nil
	}
	return cleanup, nil
}
