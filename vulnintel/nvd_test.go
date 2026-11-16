package vulnintel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestToCVEPicksPrimaryV31AndExtractsCriteria(t *testing.T) {
	raw := `{"cve":{
		"id":"CVE-2021-41773",
		"published":"2021-10-05T14:15:00.000",
		"lastModified":"2021-10-08T02:11:00.000",
		"descriptions":[
			{"lang":"es","value":"Spanish text."},
			{"lang":"en","value":"A path traversal vulnerability exists in Apache HTTP Server 2.4.49. An attacker may use it."}
		],
		"metrics":{"cvssMetricV31":[
			{"source":"nvd@nist.gov","type":"Secondary","cvssData":{"baseScore":7.5,"baseSeverity":"HIGH","vectorString":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:N/A:N"}},
			{"source":"nvd@nist.gov","type":"Primary","cvssData":{"baseScore":9.8,"baseSeverity":"CRITICAL","vectorString":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}}
		]},
		"configurations":[{"nodes":[{"operator":"OR","cpeMatch":[
			{"vulnerable":true,"criteria":"cpe:2.3:a:apache:http_server:2.4.49:*:*:*:*:*:*:*"},
			{"vulnerable":false,"criteria":"cpe:2.3:a:apache:http_server:2.4.50:*:*:*:*:*:*:*"}
		]}]}],
		"references":[{"url":"https://httpd.apache.org/security/vulnerabilities_24.html","source":"mlist","tags":["Patch"]}]
	}}`
	var w nvdWrapper
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatal(err)
	}
	c, ok := ToCVE(w)
	if !ok {
		t.Fatal("expected record to convert")
	}
	if c.ID != "CVE-2021-41773" {
		t.Fatalf("id=%s", c.ID)
	}
	if c.Score == nil || *c.Score != 9.8 {
		t.Fatalf("want Primary V31 score 9.8, got %v", c.Score)
	}
	if c.Severity != "CRITICAL" || c.Vector != "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H" {
		t.Fatalf("severity=%s vector=%s", c.Severity, c.Vector)
	}
	// Title should be the first sentence, not the whole description.
	if want := "A path traversal vulnerability exists in Apache HTTP Server 2.4.49."; c.Title != want {
		t.Fatalf("title=%q want %q", c.Title, want)
	}
	// Non-vulnerable CPE entries must be excluded.
	if len(c.Criteria) != 1 || c.Criteria[0] != "cpe:2.3:a:apache:http_server:2.4.49:*:*:*:*:*:*:*" {
		t.Fatalf("criteria=%v", c.Criteria)
	}
	if len(c.Refs) != 1 {
		t.Fatalf("refs=%v", c.Refs)
	}
	if c.Published.IsZero() || c.Modified.IsZero() {
		t.Fatalf("timestamps not parsed: pub=%v mod=%v", c.Published, c.Modified)
	}
}

func TestToCVEFallsBackAcrossMetricVersions(t *testing.T) {
	// No V31: V30 wins. No V30 either: V2 (with textual severity from cvssData).
	raw := `{"cve":{"id":"CVE-2010-0001",
		"descriptions":[{"lang":"en","value":"Old record. More text."}],
		"metrics":{"cvssMetricV2":[
			{"source":"nvd@nist.gov","type":"Secondary","cvssData":{"baseScore":7.5,"vectorString":"AV:N/AC:L/Au:N/C:P/I:P/A:P"}}
		]}}}`
	var w nvdWrapper
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatal(err)
	}
	c, ok := ToCVE(w)
	if !ok {
		t.Fatal("expected record to convert")
	}
	if c.Score == nil || *c.Score != 7.5 {
		t.Fatalf("score=%v", c.Score)
	}
	// v2 7.5 lands in HIGH on the folded numeric ladder; no textual severity in input.
	if c.Severity != "HIGH" {
		t.Fatalf("severity=%s", c.Severity)
	}
}

func TestToCVENoMetrics(t *testing.T) {
	raw := `{"cve":{"id":"CVE-2099-0001","descriptions":[{"lang":"en","value":"No scoring. Nothing."}]}}`
	var w nvdWrapper
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatal(err)
	}
	c, _ := ToCVE(w)
	if c.Severity != "UNKNOWN" || c.Score != nil {
		t.Fatalf("severity=%s score=%v", c.Severity, c.Score)
	}
}

func TestCPEVendorProduct(t *testing.T) {
	cases := []struct {
		in             string
		vendor, product string
		ok             bool
	}{
		{"cpe:2.3:a:apache:http_server:2.4.49:*:*:*:*:*:*:*", "apache", "http_server", true},
		{"cpe:2.3:o:linux:linux_kernel:5.4:*:*:*:*:*:*:*", "linux", "linux_kernel", true},
		{"cpe:2.3:h:cisco:ios:15\\(1\\):*:*:*:*:*:*:*", "cisco", "ios", true}, // escaped parens in version, plain vendor/product
		{"cpe:/a:legacy:format", "", "", false},                               // CPE 2.2 format rejected
		{"not-a-cpe", "", "", false},
	}
	for _, tc := range cases {
		v, p, ok := cpeVendorProduct(tc.in)
		if ok != tc.ok || (ok && (v != tc.vendor || p != tc.product)) {
			t.Fatalf("cpeVendorProduct(%q) = %q,%q,%v want %q,%q,%v", tc.in, v, p, ok, tc.vendor, tc.product, tc.ok)
		}
	}
}

func TestSplitUnescapedColons(t *testing.T) {
	// A product may legitimately contain an escaped colon: it must survive.
	got := splitUnescapedColons(`cpe:2.3:a:weird\:vendor:prod\:uct:1.0:*`)
	if len(got) != 7 {
		t.Fatalf("parts=%v (len %d)", got, len(got))
	}
	if got[3] != `weird\:vendor` || got[4] != `prod\:uct` {
		t.Fatalf("escaped fields lost: %q %q", got[3], got[4])
	}
}

// ToCVE 必须解析 weaknesses 里的 CWE 编号：去重、只收标准编号、NVD 内部
// 占位（NVD-CWE-OTHER / NVD-CWE-NOINFO / CWE-OTHER）不进库。
// 这是「查到 CVE → 提示对应方法论 skill」联动（M1 闭环）的 NVD 侧入口。
func TestToCVEParsesWeaknesses(t *testing.T) {
	wrap := nvdWrapper{CVE: nvdCVE{
		ID:        "CVE-2026-9999",
		Published: "2026-10-01T00:00:00.000",
		Descriptions: []nvdDescription{{Lang: "en", Value: "SQL injection in the admin console."}},
		Weaknesses: []nvdWeakness{
			{Description: []nvdDescription{{Lang: "en", Value: "CWE-89"}}},
			{Description: []nvdDescription{{Lang: "en", Value: "CWE-89"}}},   // 重复（不同 source 常见）
			{Description: []nvdDescription{{Lang: "en", Value: "cwe-79"}}},  // 小写 → 归一
			{Description: []nvdDescription{{Lang: "en", Value: "NVD-CWE-OTHER"}}}, // 占位 → 丢弃
			{Description: []nvdDescription{{Lang: "en", Value: "NVD-CWE-noinfo"}}},// 占位 → 丢弃
			{Description: []nvdDescription{{Lang: "zh", Value: "CWE-200"}}}, // 非 en → 丢弃
		},
	}}
	c, ok := ToCVE(wrap)
	if !ok {
		t.Fatal("ToCVE 应成功")
	}
	want := []string{"CWE-89", "CWE-79"}
	if len(c.Cwes) != len(want) {
		t.Fatalf("cwes=%v，应为 %v", c.Cwes, want)
	}
	for i := range want {
		if c.Cwes[i] != want[i] {
			t.Fatalf("cwes=%v，应为 %v", c.Cwes, want)
		}
	}
}

// 没有任何 weaknesses 时 cwes 应为空数组而不是 nil（marshal 后省略即可，
// 但不能 panic）。
func TestToCVEWithoutWeaknesses(t *testing.T) {
	c, ok := ToCVE(nvdWrapper{CVE: nvdCVE{
		ID:           "CVE-2026-9998",
		Descriptions: []nvdDescription{{Lang: "en", Value: "Some issue."}},
	}})
	if !ok || len(c.Cwes) != 0 {
		t.Fatalf("无 weaknesses 时 cwes 应为空，got=%v ok=%v", c.Cwes, ok)
	}
}

// NVD 的 WAF 对超限 IP 直接甩 503（无 Retry-After）——必须映射为 RateLimited，
// 让同步循环退避重试同一页，而不是废掉整轮（2026-11-15 实测频发）。
func TestFetchPageMapsWaf503ToRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("<html><body><h1>503 Service Unavailable</h1>"))
	}))
	defer srv.Close()
	c := &NvdClient{Base: srv.URL, APIKey: "k-503", HTTP: srv.Client()}
	_, err := c.FetchPage(context.Background(), 0, 1, time.Time{}, time.Time{})
	var rl *RateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("503 应映射为 *RateLimited，got %v", err)
	}
	if rl.RetryAfter <= 0 {
		t.Fatalf("503 的退避应为正数，got %v", rl.RetryAfter)
	}
}

// 429 保持原有行为：带 Retry-After 时照用，同步循环按它退避。
func TestFetchPageHonors429RetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := &NvdClient{Base: srv.URL, APIKey: "k-429", HTTP: srv.Client()}
	_, err := c.FetchPage(context.Background(), 0, 1, time.Time{}, time.Time{})
	var rl *RateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("429 应映射为 *RateLimited，got %v", err)
	}
	if rl.RetryAfter != 7*time.Second {
		t.Fatalf("RetryAfter=%v，应为 7s", rl.RetryAfter)
	}
}
