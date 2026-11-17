package server

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Git imports start immediately. This poller also picks up older projects,
// resumes interrupted shallow history, and checks for new commits periodically.
func (s *Server) runAutoHistoryScheduler() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		s.scheduleAutoHistory()
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) scheduleAutoHistory() {
	if s.ctx.Err() != nil {
		return
	}
	projects, err := s.m.pg.ListProjects()
	if err != nil {
		return
	}
	for _, p := range projects {
		if s.ctx.Err() != nil {
			return
		}
		if p.SourceType != "git" {
			continue
		}
		var state, lastError string
		var updated time.Time
		var lease sql.NullTime
		err := s.m.pg.QueryRow(`SELECT state,last_error,updated_at,lease_until FROM project_history WHERE project_id=$1`, p.ID).
			Scan(&state, &lastError, &updated, &lease)
		if err == nil {
			s.queueAutoHistoryAnalyses(p.ID)
		}
		eligible := errors.Is(err, sql.ErrNoRows)
		if err != nil && !eligible {
			continue
		}
		if err == nil {
			age := time.Since(updated)
			switch state {
			case "ready":
				eligible = age >= 24*time.Hour
			case "running":
				eligible = !lease.Valid || lease.Time.Before(time.Now())
			case "interrupted":
				if strings.Contains(lastError, "历史尚未拉取到更早提交") {
					eligible = age >= time.Minute
				} else {
					eligible = age >= 6*time.Hour
				}
			default:
				eligible = age >= time.Minute
			}
		}
		if eligible {
			_, _ = s.startHistorySync(p, "", "")
		}
	}
}
