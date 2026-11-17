package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Aiid0/Pyke/agent"
	"github.com/Aiid0/Pyke/db"
	"github.com/Aiid0/Pyke/fuzzing"
)

const fuzzStopTimeout = 45 * time.Second

func (s *Server) fuzzEnvironment(w http.ResponseWriter, r *http.Request) {
	if s.fuzz == nil {
		writeJSON(w, 200, map[string]any{"available": false, "reason": "Fuzz 服务未初始化", "image": ""})
		return
	}
	image, e := s.fuzz.Runtime.Check(r.Context())
	reason := ""
	if e != nil {
		reason = e.Error()
	}
	writeJSON(w, 200, map[string]any{"available": e == nil, "reason": reason, "image": image})
}

func (s *Server) initFuzz() {
	if s.m.pg == nil {
		return
	}
	store := fuzzing.Store{DB: s.m.pg}
	if e := store.Ensure(); e != nil {
		log.Printf("[fuzz] schema: %v", e)
		return
	}
	root, _ := filepath.Abs(s.m.dir)
	s.fuzz = &fuzzing.Manager{Store: store, Root: root, Runtime: fuzzing.Docker{Root: root, HostRoot: os.Getenv("PYKE_FUZZ_HOST_ROOT"), Image: os.Getenv("PYKE_FUZZ_IMAGE")}}
	s.fuzz.Allowed = func(id string) bool {
		t, ok := s.m.Task(id)
		return ok && !s.engine.IsPaused(id) && !s.engine.IsDeleting(id) && !s.engine.isSettling(id) && !t.lifecycleSnapshot().Queued
	}
	s.fuzz.Wake = func(id string) {
		if t, ok := s.m.Task(id); ok && !s.engine.IsPaused(id) && !s.engine.IsDeleting(id) {
			s.reviveTask(t)
			t.Notify()
		}
	}
	s.engine.fuzzPending = store.Pending
	s.engine.fuzzStop = func(id string) {
		ctx, c := context.WithTimeout(context.Background(), fuzzStopTimeout)
		defer c()
		if e := s.fuzz.StopTask(ctx, id); e != nil {
			log.Printf("[fuzz] stop task %s: %v", id, e)
		}
	}
	agent.FuzzCall = func(ctx context.Context, task int64, op string, raw json.RawMessage) (any, error) {
		return s.fuzzAction(ctx, strconv.FormatInt(task, 10), op, "agent", raw)
	}
	go s.fuzz.Run(s.ctx)
}
func (s *Server) fuzzAction(ctx context.Context, task, op, creator string, raw json.RawMessage) (result any, resultErr error) {
	if s.fuzz == nil {
		return nil, fmt.Errorf("fuzz service unavailable")
	}
	t, ok := s.m.Task(task)
	if !ok {
		return nil, fmt.Errorf("task not found")
	}
	if !s.engine.beginTaskOperation(task) {
		return nil, fmt.Errorf("task deleting")
	}
	defer s.engine.decInflight(task)
	defer func() {
		if resultErr == nil && (op == "start" || op == "stop" || op == "continue" || op == "replay" || op == "analyze" || op == "auto") {
			s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "text", Summary: fmt.Sprintf("Fuzz 操作已接受：%s（%s）", op, creator)})
		}
	}()
	var in struct {
		ID          int64  `json:"id"`
		GroupID     int64  `json:"group_id"`
		Offset      int64  `json:"offset"`
		AutoAnalyze bool   `json:"auto_analyze"`
		RequestID   string `json:"request_id"`
	}
	if len(raw) > 0 {
		if e := json.Unmarshal(raw, &in); e != nil {
			return nil, e
		}
	}
	switch op {
	case "environment":
		image, e := s.fuzz.Runtime.Check(ctx)
		reason := ""
		if e != nil {
			reason = e.Error()
		}
		return map[string]any{"available": e == nil, "image": image, "reason": reason}, nil
	case "list":
		return s.fuzz.Store.List(task)
	case "start":
		var c fuzzing.Config
		if e := json.Unmarshal(raw, &c); e != nil {
			return nil, e
		}
		if c.ParentID != 0 || c.Replay != "" {
			return nil, fmt.Errorf("use continue or replay for existing jobs")
		}
		j, e := s.fuzz.Create(ctx, task, t.ExpID, creator, c)
		if e == nil {
			s.fuzz.Wake(task)
		}
		return j, e
	}
	j, e := s.fuzz.Store.Get(task, in.ID)
	if e != nil {
		return nil, e
	}
	switch op {
	case "status":
		return j, nil
	case "logs":
		return s.fuzz.Logs(j, in.Offset)
	case "results":
		groups, e := s.fuzz.Store.Groups(j.ID)
		return map[string]any{"groups": groups, "workspace": filepath.ToSlash(filepath.Join("shared", "fuzz", strconv.FormatInt(j.ID, 10))), "job": j}, e
	case "stop":
		return map[string]bool{"ok": true}, s.fuzz.Stop(ctx, task, j.ID)
	case "auto":
		return map[string]bool{"ok": true}, s.fuzz.Store.Auto(task, j.ID, in.AutoAnalyze)
	case "continue":
		next, e := s.fuzz.Continue(ctx, task, j.ID, in.RequestID, "")
		if e == nil {
			s.fuzz.Wake(task)
		}
		return next, e
	case "analyze", "replay":
		groups, e := s.fuzz.Store.Groups(j.ID)
		if e != nil {
			return nil, e
		}
		for _, g := range groups {
			if g.ID != in.GroupID {
				continue
			}
			if op == "analyze" {
				e = s.fuzz.Store.RequestAnalysis(j.ID, g.ID)
				if e == nil && !s.engine.IsPaused(task) {
					s.fuzz.Wake(task)
				}
				return map[string]any{"requested": e == nil, "intent_id": g.IntentID}, e
			}
			next, e := s.fuzz.Continue(ctx, task, j.ID, in.RequestID, g.Sample)
			if e == nil {
				s.fuzz.Wake(task)
			}
			return next, e
		}
		return nil, fmt.Errorf("group not found")
	}
	return nil, fmt.Errorf("unknown fuzz action")
}
func (s *Server) fuzzAPI(w http.ResponseWriter, r *http.Request) {
	op := r.PathValue("action")
	if op == "" {
		if r.Method == http.MethodGet {
			op = "list"
		} else {
			op = "start"
		}
	}
	read := op == "list" || op == "status" || op == "results" || op == "logs" || op == "environment"
	if (r.Method == http.MethodGet) != read {
		writeErr(w, 405, "method not allowed")
		return
	}
	raw := json.RawMessage(`{}`)
	if r.Method != http.MethodGet {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if e := json.NewDecoder(r.Body).Decode(&raw); e != nil {
			writeErr(w, 400, "invalid JSON")
			return
		}
	}
	var data map[string]any
	if e := json.Unmarshal(raw, &data); e != nil || data == nil {
		writeErr(w, 400, "expected JSON object")
		return
	}
	if id := r.PathValue("jid"); id != "" {
		n, e := strconv.ParseInt(id, 10, 64)
		if e != nil || n <= 0 {
			writeErr(w, 400, "invalid job id")
			return
		}
		data["id"] = n
	}
	if r.Method == http.MethodGet {
		if off := r.URL.Query().Get("offset"); off != "" {
			n, e := strconv.ParseInt(off, 10, 64)
			if e != nil {
				writeErr(w, 400, "invalid offset")
				return
			}
			data["offset"] = n
		}
	}
	raw, _ = json.Marshal(data)
	result, e := s.fuzzAction(r.Context(), r.PathValue("id"), op, "human", raw)
	if e != nil {
		writeErr(w, 400, e.Error())
		return
	}
	writeJSON(w, 200, result)
}
