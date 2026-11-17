package server

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Aiid0/Pyke/db"
)

func projectSourceDir(dataDir, projectID string) string {
	return filepath.Join(dataDir, "projects", projectID, "source")
}

// ensureProjectGit moves a previous bare object store into the project's
// working tree. A pending rename is resumable after a process interruption.
func (s *Server) ensureProjectGit(ctx context.Context, p *db.Project, username, token string) error {
	source := projectSourceDir(s.m.dir, p.ID)
	if err := os.MkdirAll(source, 0755); err != nil {
		return err
	}
	gitDir := filepath.Join(source, ".git")
	if info, err := os.Lstat(gitDir); err == nil {
		if !info.IsDir() {
			return errors.New("项目 Git 元数据不是目录")
		}
		_, err = historyGit(ctx, source, "", "", "rev-parse", "--git-dir")
		if err == nil && historySHAPattern.MatchString(p.Revision) {
			if head, e := historyGit(ctx, source, "", "", "rev-parse", "--verify", "HEAD^{commit}"); e == nil && head == p.Revision {
				if _, indexErr := os.Stat(filepath.Join(gitDir, "index")); errors.Is(indexErr, os.ErrNotExist) {
					_, err = historyGit(ctx, source, "", "", "read-tree", p.Revision)
				}
			}
		}
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pending := filepath.Join(s.m.dir, "projects", p.ID, ".git.pending")
	if _, err := os.Lstat(pending); errors.Is(err, os.ErrNotExist) {
		legacy := filepath.Join(s.m.dir, "projects", p.ID, "history.git")
		if info, legacyErr := os.Lstat(legacy); legacyErr == nil {
			if !info.IsDir() {
				return errors.New("旧项目历史库不是目录")
			}
			if _, headErr := os.Stat(filepath.Join(legacy, "HEAD")); errors.Is(headErr, os.ErrNotExist) {
				entries, readErr := os.ReadDir(legacy)
				if readErr != nil || len(entries) != 0 {
					return errors.New("旧项目历史库不完整")
				}
				if err := os.Remove(legacy); err != nil {
					return err
				}
			} else if headErr != nil {
				return headErr
			} else if err := os.Rename(legacy, pending); err != nil {
				return err
			}
		} else if !errors.Is(legacyErr, os.ErrNotExist) {
			return legacyErr
		}
	} else if err != nil {
		return err
	}
	if info, err := os.Lstat(pending); err == nil {
		if !info.IsDir() {
			return errors.New("项目 Git 迁移目录无效")
		}
		if !historySHAPattern.MatchString(p.Revision) {
			return errors.New("项目缺少有效的导入版本，无法迁移 Git 历史")
		}
		if _, err := historyGit(ctx, pending, "", "", "cat-file", "-e", p.Revision+"^{commit}"); err != nil {
			_, _ = historyGit(ctx, pending, username, token, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--depth=1", "--", p.RepositoryURL, p.Revision)
			if _, err := historyGit(ctx, pending, "", "", "cat-file", "-e", p.Revision+"^{commit}"); err != nil {
				return errors.New("旧项目历史库缺少导入版本，请检查仓库权限或重新提供令牌")
			}
		}
		for _, args := range [][]string{
			{"update-ref", "refs/heads/pyke-snapshot", p.Revision},
			{"symbolic-ref", "HEAD", "refs/heads/pyke-snapshot"},
			{"config", "core.worktree", ".."},
			{"config", "core.bare", "false"},
		} {
			if _, err := historyGit(ctx, pending, "", "", args...); err != nil {
				return errors.New("无法转换项目 Git 工作目录")
			}
		}
		if err := os.Rename(pending, gitDir); err != nil {
			return err
		}
		if _, err := historyGit(ctx, source, "", "", "read-tree", p.Revision); err != nil {
			return errors.New("无法恢复项目 Git 索引")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := historyGit(ctx, source, "", "", "init", "--quiet", "--initial-branch=pyke-snapshot"); err != nil {
		return errors.New("无法初始化项目 Git 工作目录")
	}
	return os.Chmod(gitDir, 0700)
}

func (s *Server) pinProjectRevision(ctx context.Context, p *db.Project, username, token string) error {
	if p.Revision == "" {
		return nil
	}
	if !historySHAPattern.MatchString(p.Revision) {
		return errors.New("项目导入版本无效")
	}
	source := projectSourceDir(s.m.dir, p.ID)
	if _, err := historyGit(ctx, source, "", "", "cat-file", "-e", p.Revision+"^{commit}"); err != nil {
		_, _ = historyGit(ctx, source, username, token, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--depth=1", "--", p.RepositoryURL, p.Revision)
		if _, err := historyGit(ctx, source, "", "", "cat-file", "-e", p.Revision+"^{commit}"); err != nil {
			return errors.New("Git 历史中找不到项目导入版本，请检查仓库权限")
		}
	}
	head, err := historyGit(ctx, source, "", "", "rev-parse", "--verify", "HEAD^{commit}")
	if err == nil {
		if head != p.Revision {
			return errors.New("项目 Git 工作树版本与导入版本不一致")
		}
		return nil
	}
	if _, err := historyGit(ctx, source, "", "", "update-ref", "refs/heads/pyke-snapshot", p.Revision); err != nil {
		return err
	}
	if _, err := historyGit(ctx, source, "", "", "symbolic-ref", "HEAD", "refs/heads/pyke-snapshot"); err != nil {
		return err
	}
	_, err = historyGit(ctx, source, "", "", "read-tree", p.Revision)
	return err
}

// ensureTaskGitView uses task-local refs and index with a relative object
// alternate. It never gives the task a remote that could update the project.
func ensureTaskGitView(ctx context.Context, p *db.Project, projectSource, taskSource string) error {
	if p == nil || p.SourceType != "git" {
		return nil
	}
	if !historySHAPattern.MatchString(p.Revision) {
		return errors.New("任务绑定的项目提交无效")
	}
	if head, err := historyGit(ctx, projectSource, "", "", "rev-parse", "--verify", "HEAD^{commit}"); err != nil || head != p.Revision {
		return errors.New("项目 Git 工作目录尚未准备好")
	}
	taskGit := filepath.Join(taskSource, ".git")
	if info, err := os.Lstat(taskGit); err == nil {
		if !info.IsDir() {
			return errors.New("任务已有其他 Git 元数据")
		}
		owner, e := historyGit(ctx, taskSource, "", "", "config", "--get", "pyke.projectId")
		if e != nil || owner != p.ID {
			return errors.New("任务已有其他 Git 仓库")
		}
		if _, err := os.Stat(filepath.Join(taskGit, "index")); errors.Is(err, os.ErrNotExist) {
			if _, err := historyGit(ctx, taskSource, "", "", "read-tree", p.Revision); err != nil {
				return err
			}
		}
		return refreshTaskShallow(projectSource, taskGit)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pending, err := os.MkdirTemp(taskSource, ".git-pyke-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(pending)
	if _, err := historyGit(ctx, pending, "", "", "init", "--bare", "--quiet"); err != nil {
		return err
	}
	objects := filepath.Join(taskGit, "objects")
	projectObjects := filepath.Join(projectSource, ".git", "objects")
	rel, err := filepath.Rel(objects, projectObjects)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pending, "objects", "info", "alternates"), []byte(filepath.ToSlash(rel)+"\n"), 0600); err != nil {
		return err
	}
	if err := refreshTaskShallow(projectSource, pending); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"update-ref", "refs/heads/pyke-snapshot", p.Revision},
		{"symbolic-ref", "HEAD", "refs/heads/pyke-snapshot"},
		{"config", "pyke.projectId", p.ID},
		{"config", "core.worktree", ".."},
		{"config", "core.hooksPath", os.DevNull},
		{"config", "core.bare", "false"},
	} {
		if _, err := historyGit(ctx, pending, "", "", args...); err != nil {
			return err
		}
	}
	if err := os.Rename(pending, taskGit); err != nil {
		return err
	}
	_, err = historyGit(ctx, taskSource, "", "", "read-tree", p.Revision)
	return err
}

func refreshTaskShallow(projectSource, taskGit string) error {
	want, err := os.ReadFile(filepath.Join(projectSource, ".git", "shallow"))
	if errors.Is(err, os.ErrNotExist) {
		err = os.Remove(filepath.Join(taskGit, "shallow"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	target := filepath.Join(taskGit, "shallow")
	current, _ := os.ReadFile(target)
	if bytes.Equal(current, want) {
		return nil
	}
	tmp, err := os.CreateTemp(taskGit, ".shallow-pyke-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(want); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		// Windows cannot replace an existing file with os.Rename.
		if removeErr := os.Remove(target); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return err
		}
		return os.Rename(tmp.Name(), target)
	}
	return nil
}

func (s *Server) reconcileProjectTaskGitViews(p *db.Project) {
	if p == nil || p.SourceType != "git" {
		return
	}
	source := projectSourceDir(s.m.dir, p.ID)
	if _, err := os.Stat(filepath.Join(source, ".git", "HEAD")); err != nil {
		return
	}
	rows, err := s.m.pg.Query(`SELECT id FROM tasks WHERE deleted_at IS NULL AND project->>'id'=$1`, p.ID)
	if err != nil {
		log.Printf("[project-git] list tasks for project %s: %v", p.ID, err)
		return
	}
	var taskIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			taskIDs = append(taskIDs, id)
		}
	}
	_ = rows.Close()
	for _, id := range taskIDs {
		taskSource := filepath.Join(s.m.dir, "tasks", strconv.FormatInt(id, 10), "source")
		if _, err := os.Stat(taskSource); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		err := ensureTaskGitView(ctx, p, source, taskSource)
		cancel()
		if err != nil {
			log.Printf("[project-git] task %d: %v", id, err)
		}
	}
}

func (s *Server) reconcileExistingTaskGitViews() {
	projects, err := s.m.pg.ListProjects()
	if err != nil {
		return
	}
	for _, p := range projects {
		if s.ctx.Err() != nil {
			return
		}
		s.reconcileProjectTaskGitViews(p)
	}
}

func (s *Server) reconcileTaskGitViewByID(taskID int64) {
	task, err := s.m.pg.GetTask(taskID)
	if err != nil || task == nil || task.Project == nil || task.Project.SourceType != "git" {
		return
	}
	projectSource := projectSourceDir(s.m.dir, task.Project.ID)
	taskSource := filepath.Join(s.m.dir, "tasks", strconv.FormatInt(taskID, 10), "source")
	if _, err := os.Stat(filepath.Join(projectSource, ".git", "HEAD")); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	if err := ensureTaskGitView(ctx, task.Project, projectSource, taskSource); err != nil {
		log.Printf("[project-git] restored task %d: %v", taskID, err)
	}
}
