package vulnintel

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// Config controls one sync run.
type Config struct {
	NvdAPIKey    string        // optional; lifts the 5 req/30s anonymous cap
	ProxyURL     string        // global egress proxy for outbound fetches (empty = direct)
	GitHubToken  string        // optional; lifts GitHub search limits for PoC aggregation
	PocEnabled   bool          // aggregate GitHub PoC repos for recently synced CVEs
	BackfillDays int           // first-run lookback; 0 (default) = full catalog (~409k records,
	                           // ≈205 pages: ~22 min anonymous / ~3 min with a key, split across runs)
	PageDelay    time.Duration // inter-request delay; 0 = derived from the key
	MaxRuntime   time.Duration // per-run ceiling (default 30m) — the rest waits for the next tick
}

// Run executes one full sync: KEV flags first (cheap, high signal), then the
// PageDelayFor derives the inter-request delay for the NVD API from the key.
//
// NVD 的限速：匿名约 5 次/30s，有 key 约 50 次/30s。所以间隔分别是
// 6.5s 与 0.7s。
//
// 单独抽出来是因为**主同步与快通道共用同一份限速额度**——两处各写一份
// 数字、其中一处忘了按 key 调整，快的那个就会把慢的挤到 429
// （2026-10-22 实测：快通道硬编码 1s，匿名访问下第一页就 rate limited）。
func PageDelayFor(apiKey string) time.Duration {
	if strings.TrimSpace(apiKey) != "" {
		return 700 * time.Millisecond // 50 req / 30s
	}
	return 6500 * time.Millisecond // 5 req / 30s
}

// NVD window (incremental when a cursor exists, backfill otherwise). Errors are
// recorded per source into vuln_sync_state and also returned joined.
func Run(ctx context.Context, st *Store, cfg Config) error {
	if cfg.BackfillDays < 0 {
		cfg.BackfillDays = 0
	}
	if cfg.PageDelay <= 0 {
		cfg.PageDelay = PageDelayFor(cfg.NvdAPIKey)
	}
	if cfg.MaxRuntime <= 0 {
		cfg.MaxRuntime = 30 * time.Minute
	}
	deadline := time.Now().Add(cfg.MaxRuntime)
	if d, ok := ctx.Deadline(); !ok || d.After(deadline) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}

	var kevErr, nvdErr, pocErr error
	if err := syncKEV(ctx, st, cfg); err != nil {
		kevErr = err
		log.Printf("[vulnintel] kev sync: %v", err)
	}
	if err := syncNVD(ctx, st, cfg); err != nil {
		nvdErr = err
		log.Printf("[vulnintel] nvd sync: %v", err)
	}
	if cfg.PocEnabled {
		if err := syncPoCs(ctx, st, cfg); err != nil {
			pocErr = err
			log.Printf("[vulnintel] poc sync: %v", err)
		}
	}
	return errors.Join(kevErr, nvdErr, pocErr)
}

// syncPoCs aggregates GitHub PoC repos for CVEs synced since the previous PoC
// watermark (or, first run, recent high-impact records). Only CVEs present in
// vuln_cves are queried — the pool of KEV + recent high-severity first, since
// 409k × 1 search each is neither feasible nor useful.
func syncPoCs(ctx context.Context, st *Store, cfg Config) error {
	client, err := NewGitHubClient(cfg.GitHubToken, cfg.ProxyURL)
	if err != nil {
		return err
	}
	state, err := st.GetSyncState(ctx, "poc")
	if err != nil {
		return err
	}
	// Query pool: KEV first (exploited in the wild → PoC most valuable), then
	// the latest NVD window records, capped hard by the unauthenticated budget.
	rows, err := st.db.QueryContext(ctx, `SELECT cve_id FROM vuln_cves
		WHERE kev = true
		   OR published_at > now() - interval '30 days'
		ORDER BY kev DESC, GREATEST(COALESCE(cvss_score,0), 0) DESC, published_at DESC
		LIMIT 400`)
	if err != nil {
		return err
	}
	var targets []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			targets = append(targets, id)
		}
	}
	rows.Close()
	fetched := 0
	failures := 0
	for _, cveID := range targets {
		if err := ctx.Err(); err != nil {
			break
		}
		pocs, err := client.SearchPoCs(ctx, cveID)
		if err != nil {
			var rl *RateLimited
			if errors.As(err, &rl) {
				wait := rl.RetryAfter
				if wait <= 0 {
					wait = 70 * time.Second // unauthenticated search: 10/min window resets
				}
				select {
				case <-ctx.Done():
					break // outer
				case <-time.After(wait):
				}
				continue
			}
			failures++
			if failures > 5 { // source down / quirk — abandon for this run
				log.Printf("[vulnintel] poc sync aborting after %d failures: %v", failures, err)
				break
			}
			continue
		}
		for _, p := range pocs {
			if err := st.UpsertPoC(ctx, p); err != nil {
				log.Printf("[vulnintel] poc upsert %s: %v", p.CVEID, err)
			}
		}
		fetched++
		time.Sleep(2 * time.Second) // stay far under the 10/min search cap
	}
	state.LastSyncAt = nowPtr()
	state.TotalSynced = int64(fetched)
	state.Enabled = true
	if ctx.Err() != nil {
		state.LastError = ctx.Err().Error()
	} else if failures > 0 {
		state.LastError = fmt.Sprintf("%d/%d lookups failed", failures, fetched+failures)
	} else {
		state.LastError = ""
	}
	if err := st.SaveSyncState(ctx, state); err != nil {
		return err
	}
	log.Printf("[vulnintel] poc synced: %d CVE lookups", fetched)
	return nil
}

// syncKEV overwrites the in-the-wild flags from the full catalog.KEV moves
// faster than NVD, so entries absent from vuln_cves get a stub row visible to
// search until the NVD sync fills in details.
func syncKEV(ctx context.Context, st *Store, cfg Config) error {
	client, err := NewNvdClient(cfg.NvdAPIKey, cfg.ProxyURL)
	if err != nil {
		return err
	}
	cat, err := client.FetchKEV(ctx)
	if err != nil {
		_ = st.recordError(ctx, "kev", err)
		return err
	}
	marked := 0
	for _, e := range cat.Vulnerabilities {
		if ctx.Err() != nil {
			break
		}
		if err := st.MarkKEV(ctx, e.CVEID, e.DateAdded); err != nil {
			log.Printf("[vulnintel] kev mark %s: %v", e.CVEID, err)
			continue
		}
		marked++
	}
	state, _ := st.GetSyncState(ctx, "kev")
	state.LastSyncAt = nowPtr()
	state.TotalSynced = int64(marked)
	state.LastError = ""
	state.Enabled = true
	if ctx.Err() != nil {
		state.LastError = ctx.Err().Error()
	}
	if err := st.SaveSyncState(ctx, state); err != nil {
		return err
	}
	log.Printf("[vulnintel] kev synced: %d/%d entries (catalog %s)", marked, cat.Count, cat.Version)
	return ctx.Err()
}

// syncNVD pulls the lastModified window: incremental when a cursor exists
// (24h overlap to absorb NVD-side indexing lag), a full-catalog crawl on first
// run. 120-day API window is split into chunks; each chunk pages by startIndex.
// The cursor advances per completed WINDOW (not per run), so a run cut short by
// MaxRuntime resumes from the next window instead of starting over.
func syncNVD(ctx context.Context, st *Store, cfg Config) error {
	client, err := NewNvdClient(cfg.NvdAPIKey, cfg.ProxyURL)
	if err != nil {
		return err
	}
	state, err := st.GetSyncState(ctx, "nvd")
	if err != nil {
		return err
	}
	now := time.Now()
	from := nvdResumeAnchor(state.Cursor, state.PageCursor, now)
	if from.IsZero() && cfg.BackfillDays > 0 {
		from = now.AddDate(0, 0, -cfg.BackfillDays)
	}
	// Full-catalog backfill anchors at NVD's own epoch (earliest published record
	// is 1988) — a literal zero time would spin hundreds of thousands of empty
	// 120-day windows down to year 1.
	if from.IsZero() {
		from = time.Date(1988, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	total := 0
	for winStart := from; winStart.Before(now); {
		winEnd := winStart.AddDate(0, 0, 120)
		if !winEnd.Before(now) {
			winEnd = now
		}
		// Resume inside the window when a previous run stopped mid-window.
		// Without this the window restarts at page 0 every time, and a window
		// that needs more than one run's MaxRuntime NEVER completes — the
		// cursor stays pinned to the same window forever.
		startIndex := 0
		if ws, idx, ok := parsePageCursor(state.PageCursor); ok && ws.Equal(winStart) {
			startIndex = idx
			log.Printf("[vulnintel] nvd 续跑窗口 %s，startIndex=%d", winStart.Format("2006-01-02"), idx)
		}
		n, err := syncNVDWindow(ctx, client, st, winStart, winEnd, cfg.PageDelay, startIndex, &state)
		total += n
		state.TotalSynced += int64(n)
		if err != nil {
			state.LastError = err.Error()
			_ = st.SaveSyncState(ctx, state)
			return err
		}
		// Window completed: advance the cursor so an interrupted later window
		// doesn't replay this one (the 24h overlap on resume keeps it safe).
		state.Cursor = winEnd.UTC().Format(time.RFC3339)
		state.PageCursor = ""
		state.LastError = ""
		if err := st.SaveSyncState(ctx, state); err != nil {
			return err
		}
		winStart = winEnd
	}
	state.LastSyncAt = nowPtr()
	state.Cursor = now.UTC().Format(time.RFC3339)
	state.PageCursor = ""
	state.LastError = ""
	state.Enabled = true
	if err := st.SaveSyncState(ctx, state); err != nil {
		return err
	}
	log.Printf("[vulnintel] nvd synced: %d records this run", total)
	return nil
}

// nvdResumeAnchor 决定本轮窗口网格的起点。
//
// 默认从 cursor 回扫 24h（吸收 NVD 索引延迟：上一轮回扫期间披露的 CVE 可能
// 带着窗口起点之前的 lastMod 入库）。但**有页级断点**（上个窗口没跑完）时必须
// 锚回断点窗口的起点：回扫会把本轮网格整体挪一天，页游标的窗口起点对不上，
// 断点作废，大窗口只能从第 0 页重爬——2026-11-16 实测 19 万条的窗口每轮重爬
// 前几千条就超时，回灌永远差最后一公里。锚回去的代价只是这一个窗口边界的
// 24h 少扫一遍（重要 CVE 有快通道兜底），窗口完成后 cursor 推进，后续窗口的
// 24h 回扫照常。
//
// cursor 无法解析且没有页级断点时返回零值，调用方回落到 BackfillDays/1988。
func nvdResumeAnchor(cursor, pageCursor string, now time.Time) time.Time {
	if ws, _, ok := parsePageCursor(pageCursor); ok && ws.Before(now) {
		return ws
	}
	if cursor != "" {
		if t, e := time.Parse(time.RFC3339, cursor); e == nil {
			return t.Add(-24 * time.Hour)
		}
	}
	return time.Time{}
}

// formatPageCursor encodes "window start | next startIndex" for the page-level
// resume watermark.
func formatPageCursor(winStart time.Time, startIndex int) string {
	return winStart.UTC().Format(time.RFC3339) + "|" + strconv.Itoa(startIndex)
}

// parsePageCursor decodes the page-level watermark. ok=false when absent or
// malformed (the caller then starts the window from page 0).
func parsePageCursor(raw string) (time.Time, int, bool) {
	if raw == "" {
		return time.Time{}, 0, false
	}
	i := strings.LastIndex(raw, "|")
	if i <= 0 {
		return time.Time{}, 0, false
	}
	t, err := time.Parse(time.RFC3339, raw[:i])
	if err != nil {
		return time.Time{}, 0, false
	}
	idx, err := strconv.Atoi(raw[i+1:])
	if err != nil || idx < 0 {
		return time.Time{}, 0, false
	}
	return t, idx, true
}

// syncNVDWindow pages one ≤120-day window to exhaustion, upserting each record.
//
// startIndex lets the caller resume mid-window; state (a *SyncState) receives the
// page-level watermark after every successful page, so a run cut short by
// MaxRuntime continues from the next page instead of replaying the whole window.
func syncNVDWindow(ctx context.Context, client *NvdClient, st *Store, start, end time.Time, delay time.Duration, startIndex int, state *SyncState) (int, error) {
	const pageSize = 2000
	total := 0
	for idx := startIndex; ; idx += pageSize {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		resp, err := fetchNVDPageWithRetry(ctx, client, idx, pageSize, start, end)
		if err != nil {
			var rl *RateLimited
			if errors.As(err, &rl) {
				wait := rl.RetryAfter
				if wait <= 0 {
					wait = delay * 2
				}
				select {
				case <-ctx.Done():
					return total, ctx.Err()
				case <-time.After(wait):
				}
				continue // retry same page
			}
			return total, fmt.Errorf("window %s..%s @%d: %w", start.Format("2006-01-02"), end.Format("2006-01-02"), idx, err)
		}
		for _, w := range resp.Vulnerabilities {
			c, ok := ToCVE(w)
			if !ok {
				continue
			}
			if err := st.UpsertCVE(ctx, c); err != nil {
				return total, err
			}
			total++
		}
		// Page done: persist the resume point before moving on. The next page
		// is the one to re-fetch if this run dies.
		if state != nil {
			state.PageCursor = formatPageCursor(start, idx+pageSize)
			if serr := st.SaveSyncState(ctx, *state); serr != nil {
				return total, serr
			}
		}
		if idx+resp.ResultsPerPage >= resp.TotalResults || len(resp.Vulnerabilities) == 0 {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(delay):
		}
	}
}

// fetchNVDPageWithRetry 拉一页，瞬时错误按 5s/15s/45s 退避重试（最多 3 次）。
//
// 为什么需要：密窗口一页是 2000 条约 5MB 的 JSON，走代理传一半被掐断
// （HTTP/2 INTERNAL_ERROR）、或 NVD 生成慢到触发超时，都是瞬时故障——按
// 原设计直接 return，整轮作废，下轮又从第 0 页重爬（2026-11-16 实测）。
// 429/503 的 RateLimited 不在此重试，交给主循环按限流语义等窗口。
// ctx 取消（MaxRuntime 到点/用户中断）不重试，立即向上传。
func fetchNVDPageWithRetry(ctx context.Context, client *NvdClient, startIndex, pageSize int, start, end time.Time) (*nvdResponse, error) {
	backoff := 5 * time.Second
	for attempt := 1; ; attempt++ {
		resp, err := client.FetchPage(ctx, startIndex, pageSize, start, end)
		if err == nil {
			return resp, nil
		}
		var rl *RateLimited
		if errors.As(err, &rl) {
			return nil, err
		}
		if ctx.Err() != nil || attempt >= 3 {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 3
	}
}

// recordError persists a source-level failure without touching the cursor —
// the next run retries the same window.
func (s *Store) recordError(ctx context.Context, source string, err error) error {
	state, e := s.GetSyncState(ctx, source)
	if e != nil {
		return e
	}
	state.LastError = err.Error()
	state.Enabled = true
	return s.SaveSyncState(ctx, state)
}

func nowPtr() *time.Time { t := time.Now(); return &t }
