// Package projectsource imports repository and uploaded project snapshots.
package projectsource

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	MaxFiles       = 30000
	MaxBytes       = int64(1 << 30)
	MaxUploadBytes = int64(512 << 20)
)

type Stats struct {
	Files int
	Bytes int64
}

// RelativePath rejects archive/upload paths that could leave their project.
func RelativePath(name string) (string, error) {
	if strings.ContainsAny(name, "\\\x00:") || strings.HasPrefix(name, "/") {
		return "", errors.New("项目文件路径无效")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", errors.New("项目文件路径不得包含 ..")
		}
	}
	clean := path.Clean(name)
	if clean == "." || clean == "" {
		return "", errors.New("项目文件路径不能为空")
	}
	return clean, nil
}

func gitMetadata(name string) bool {
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if strings.EqualFold(part, ".git") {
			return true
		}
	}
	return false
}

// WriteFile counts actual decompressed bytes rather than trusting client sizes.
func (s *Stats) WriteFile(root, name string, source io.Reader) error {
	rel, err := RelativePath(name)
	if err != nil {
		return err
	}
	if gitMetadata(rel) {
		return nil
	}
	if s.Files >= MaxFiles {
		return errors.New("项目文件数量超过 30000 个")
	}
	dest := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("创建项目文件 %q 失败: %w", rel, err)
	}
	n, copyErr := io.Copy(f, io.LimitReader(source, MaxBytes-s.Bytes+1))
	closeErr := f.Close()
	s.Bytes += n
	s.Files++
	if s.Bytes > MaxBytes {
		return errors.New("项目解压后超过 1 GiB")
	}
	return errors.Join(copyErr, closeErr)
}

func ExtractZIP(reader io.ReaderAt, size int64, dest string) (Stats, error) {
	var stats Stats
	zr, err := zip.NewReader(reader, size)
	if err != nil {
		return stats, errors.New("无法读取 ZIP 项目文件")
	}
	if len(zr.File) > MaxFiles*2 {
		return stats, errors.New("ZIP 项目条目过多")
	}
	for _, file := range zr.File {
		rel, err := RelativePath(strings.TrimSuffix(file.Name, "/"))
		if err != nil {
			return stats, err
		}
		if file.Mode()&os.ModeSymlink != 0 || (!file.FileInfo().IsDir() && !file.Mode().IsRegular()) {
			return stats, fmt.Errorf("项目不支持符号链接或特殊文件: %s", rel)
		}
		if file.FileInfo().IsDir() || gitMetadata(rel) {
			continue
		}
		if file.UncompressedSize64 > uint64(MaxBytes-stats.Bytes) {
			return stats, errors.New("项目解压后超过 1 GiB")
		}
		r, err := file.Open()
		if err != nil {
			return stats, err
		}
		err = stats.WriteFile(dest, rel, r)
		closeErr := r.Close()
		if err != nil || closeErr != nil {
			return stats, errors.Join(err, closeErr)
		}
	}
	if stats.Files == 0 {
		return stats, errors.New("项目中没有可导入的文件")
	}
	return stats, nil
}

// Copy creates a separate task snapshot and rejects symlink escapes.
func Copy(source, dest string) (Stats, error) {
	var stats Stats
	err := filepath.WalkDir(source, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, current)
		if err != nil || rel == "." {
			return err
		}
		if gitMetadata(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("项目不支持符号链接: %s", rel)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("项目不支持特殊文件: %s", rel)
		}
		f, err := os.Open(current)
		if err != nil {
			return err
		}
		err = stats.WriteFile(dest, filepath.ToSlash(rel), f)
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Chmod(filepath.Join(dest, rel), info.Mode().Perm()&0o111|0o644)
		}
		return err
	})
	if err == nil && stats.Files == 0 {
		err = errors.New("项目中没有可导入的文件")
	}
	return stats, err
}

func RepositoryURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.Path == "" || u.Path == "/" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("请填写 HTTP(S) Git 仓库地址，账号与令牌使用独立认证字段")
	}
	return u.String(), nil
}

// Clone uses argument lists and ephemeral Git configuration; credentials never
// appear in the URL, process arguments, repository config, or returned errors.
func Clone(ctx context.Context, dest, remote, branch, username, token string) (string, string, error) {
	remote, err := RepositoryURL(remote)
	if err != nil {
		return "", "", err
	}
	if strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, "\r\n\x00") || len(branch) > 250 {
		return "", "", errors.New("分支名称无效")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	args := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "protocol.file.allow=never", "-c", "protocol.ext.allow=never", "clone", "--quiet", "--depth=1", "--single-branch", "--no-tags"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, "--", remote, dest)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.WaitDelay = 5 * time.Second
	env := []string{}
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "GIT_") {
			env = append(env, item)
		}
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if token != "" {
		if username == "" {
			username = "oauth2"
		}
		auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + token))
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+auth)
	}
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", "", errors.New("仓库导入超时或已取消，请重试或上传 ZIP")
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", "", errors.New("服务端未安装 git")
		}
		return "", "", errors.New("克隆失败，请检查仓库地址、分支、访问权限和服务端网络")
	}
	readGit := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dest}, args...)...)
		cmd.Env = env
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	revision, err := readGit("rev-parse", "HEAD")
	if err != nil {
		return "", "", errors.New("无法识别仓库提交版本")
	}
	actualBranch, err := readGit("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", "", errors.New("无法识别仓库分支")
	}
	return actualBranch, revision, nil
}
