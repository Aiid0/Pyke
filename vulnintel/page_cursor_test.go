package vulnintel

import (
	"testing"
	"time"
)

// 页级游标：NVD 同步的窗口可能大到单轮跑不完，游标只按「窗口」推进的话，
// 窗口内失败就整窗重跑——游标永远卡在同一个窗口，每轮都白跑。
//
// 实测（2026-10-21）：NVD 的 120 天窗口能到 14 万条（71 页 × 约 17 秒 +
// 限速延时），单轮 30 分钟的 MaxRuntime 跑不完；游标停在 2026-06-10，
// 最后一个窗口反复超时，40 万条的目录补到 14 万就再也补不上。
func TestPageCursorRoundTrip(t *testing.T) {
	winStart := time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)
	raw := formatPageCursor(winStart, 10000)
	gotWin, gotIdx, ok := parsePageCursor(raw)
	if !ok {
		t.Fatalf("round trip failed: %q", raw)
	}
	if !gotWin.Equal(winStart) {
		t.Errorf("window = %v, want %v", gotWin, winStart)
	}
	if gotIdx != 10000 {
		t.Errorf("index = %d, want 10000", gotIdx)
	}
}

func TestParsePageCursorRejectsGarbage(t *testing.T) {
	cases := []string{
		"",
		"no-separator",
		"not-a-time|100",
		"2026-06-09T00:00:00Z|notanumber",
		"2026-06-09T00:00:00Z|-5",
		"|100",
	}
	for _, raw := range cases {
		if _, _, ok := parsePageCursor(raw); ok {
			t.Errorf("must reject %q", raw)
		}
	}
}

// 页级游标里的窗口起点必须和实际窗口严格相等——差一点就会把上一个窗口的
// 进度套到下一个窗口上（跳页 = 漏数据）。
func TestPageCursorWindowMustMatchExactly(t *testing.T) {
	winStart := time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)
	raw := formatPageCursor(winStart, 2000)
	gotWin, _, ok := parsePageCursor(raw)
	if !ok {
		t.Fatal("parse failed")
	}
	other := winStart.AddDate(0, 0, 120)
	if gotWin.Equal(other) {
		t.Fatal("different windows must not compare equal")
	}
	// 同一时刻不同表示（UTC vs 本地时区）也要能对上
	local := winStart.In(time.FixedZone("CST", 8*3600))
	rawLocal := formatPageCursor(local, 2000)
	gotLocal, _, ok := parsePageCursor(rawLocal)
	if !ok {
		t.Fatal("parse failed for local-zone cursor")
	}
	if !gotLocal.Equal(winStart) {
		t.Errorf("same instant in another zone must compare equal: %v vs %v", gotLocal, winStart)
	}
}

// 有页级断点时，本轮网格必须锚回断点窗口的起点：默认的 cursor-24h 回扫会把
// 网格挪一天，页游标对不上，大窗口整窗重爬（2026-11-16 实测回灌卡死）。
func TestNvdResumeAnchorPrefersPageCursor(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	ws := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	cursor := ws.Format(time.RFC3339) // 窗口没跑完，cursor 停在窗口起点
	pc := formatPageCursor(ws, 188000)

	got := nvdResumeAnchor(cursor, pc, now)
	if !got.Equal(ws) {
		t.Fatalf("有页级断点时应锚回断点窗口起点 %v，got %v", ws, got)
	}
}

// 没有页级断点时保持原语义：从 cursor 回扫 24h。
func TestNvdResumeAnchorCursorOverlapWithoutPageCursor(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	cursor := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)

	got := nvdResumeAnchor(cursor, "", now)
	want := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("应从 cursor 回扫 24h 到 %v，got %v", want, got)
	}
}

// 断点窗口在未来（时钟异常/脏数据）时不采用，回落 cursor-24h。
func TestNvdResumeAnchorRejectsFuturePageCursor(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	cursorT := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	pc := formatPageCursor(now.AddDate(0, 0, 7), 2000)

	got := nvdResumeAnchor(cursorT.Format(time.RFC3339), pc, now)
	want := cursorT.Add(-24 * time.Hour)
	if !got.Equal(want) {
		t.Fatalf("未来断点应回落 cursor-24h = %v，got %v", want, got)
	}
}

// cursor 与页级断点都无效时返回零值，调用方回落 BackfillDays/1988。
func TestNvdResumeAnchorZeroOnGarbage(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	if got := nvdResumeAnchor("garbage", "also-garbage", now); !got.IsZero() {
		t.Fatalf("脏数据应返回零值，got %v", got)
	}
}
