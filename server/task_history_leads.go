package server

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) taskHistoryLeads(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.PathValue("id"))
	if t == nil {
		writeErr(w, 404, "任务不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	items, err := t.Store.SearchTaskHistoryLeads(ctx, 5, 0, "")
	if err != nil {
		writeErr(w, 500, "无法读取历史线索")
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
