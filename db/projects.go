package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Project describes one imported source snapshot. Tasks retain their own copy
// of both this metadata and the source files, including in cold archives.
type Project struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	SourceType    string    `json:"source_type"`
	RepositoryURL string    `json:"repository_url,omitempty"`
	Branch        string    `json:"branch,omitempty"`
	Revision      string    `json:"revision,omitempty"`
	FileCount     int       `json:"file_count"`
	TotalBytes    int64     `json:"total_bytes"`
	CreatedAt     time.Time `json:"created_at"`
}

const projectCols = `id,name,source_type,repository_url,branch,revision,file_count,total_bytes,created_at`

func scanProject(row interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.Name, &p.SourceType, &p.RepositoryURL, &p.Branch, &p.Revision, &p.FileCount, &p.TotalBytes, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

func (d *DB) CreateProject(p Project) (*Project, error) {
	return scanProject(d.QueryRow(`INSERT INTO projects(id,name,source_type,repository_url,branch,revision,file_count,total_bytes)
VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+projectCols, p.ID, p.Name, p.SourceType, p.RepositoryURL, p.Branch, p.Revision, p.FileCount, p.TotalBytes))
}

func (d *DB) ListProjects() ([]*Project, error) {
	rows, err := d.Query(`SELECT ` + projectCols + ` FROM projects ORDER BY created_at DESC,id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) GetProject(id string) (*Project, error) {
	return scanProject(d.QueryRow(`SELECT `+projectCols+` FROM projects WHERE id=$1`, id))
}

func (s *ExplorationStore) TaskProject() (*Project, error) {
	var raw []byte
	err := s.db.QueryRow(`SELECT project FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL`, s.expID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && len(raw) == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p *Project
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return p, nil
}
