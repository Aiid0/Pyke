package hosttools

import (
	"encoding/json"
	"strings"
	"testing"
)

// graph_overview 里带用途的工具清单不能把上下文撑爆——它每轮都进 prompt。
func TestSummaryStaysCompact(t *testing.T) {
	s := Summary()
	b, _ := json.Marshal(s)
	// v0.4.69 目录扩到 90 件后实测 ~5.1KB（约 1.3K token），4000 旧上限已不现实；
	// 5500 仍保留增长护栏（每项含用途一句话）。
	if len(b) > 5500 {
		t.Errorf("工具清单 %d 字节，太大会撑爆每轮 prompt", len(b))
	}
	// 每项都必须带用途（模型要知道这个工具是干嘛的）
	avail, _ := s["available"].([]string)
	withPurpose := 0
	for _, a := range avail {
		if strings.Contains(a, ": ") {
			withPurpose++
		}
	}
	if len(avail) > 0 && withPurpose < len(avail) {
		t.Errorf("只有 %d/%d 项带用途", withPurpose, len(avail))
	}
	t.Logf("available=%d missing=%d bytes=%d", len(avail), len(s["missing"].([]string)), len(b))
}
