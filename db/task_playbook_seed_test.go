package db

import (
	"strings"
	"testing"
)

// Fresh installs receive every batch and repeated seeding stays idempotent.
func TestSeedBuiltinTaskPlaybooks(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	// 干净批次：清掉本测试的历史种子标记与同名模板（共享测试库）。
	_, _ = d.Exec(`DELETE FROM task_templates WHERE name LIKE 'Playbook：%'`)
	_, _ = d.Exec(`DELETE FROM settings WHERE key IN ('task_playbooks_v1', 'task_playbooks_v2')`)

	if err := d.seedBuiltinTaskPlaybooks(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	list, err := d.ListTaskTemplates()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, tpl := range list {
		if strings.HasPrefix(tpl.Name, "Playbook：") {
			count++
			if !strings.Contains(tpl.Goal, "【内部工作流") && !strings.Contains(tpl.Goal, "【工作流") && !strings.Contains(tpl.Goal, "【产出】") {
				t.Errorf("playbook %q 缺少结构化工作流段", tpl.Name)
			}
		}
	}
	want := len(builtinTaskPlaybooks) + len(builtinVulnHuntingPlaybooks)
	if count != want {
		t.Fatalf("playbook 数=%d want %d", count, want)
	}
	// 幂等：再跑一次不新增、不报错（flag 已 done）。
	before := len(list)
	if err := d.seedBuiltinTaskPlaybooks(); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	after, _ := d.ListTaskTemplates()
	if len(after) != before {
		t.Fatalf("重复种子不应新增：before=%d after=%d", before, len(after))
	}
}

func prepareVulnHuntingPlaybookUpgrade(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	for _, pb := range builtinVulnHuntingPlaybooks {
		if _, err := d.Exec(`DELETE FROM task_templates WHERE name=$1`, pb.name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Exec(`DELETE FROM settings WHERE key='task_playbooks_v2'`); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestVulnHuntingPlaybooksUpgradePreservesExistingTemplates(t *testing.T) {
	d := prepareVulnHuntingPlaybookUpgrade(t)
	// A deployed v1 batch may include both edited and deleted templates.
	legacyName := builtinTaskPlaybooks[0].name
	deletedLegacyName := builtinTaskPlaybooks[1].name
	if _, err := d.Exec(`UPDATE task_templates SET goal='用户的测试目标' WHERE name=$1`, legacyName); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DELETE FROM task_templates WHERE name=$1`, deletedLegacyName); err != nil {
		t.Fatal(err)
	}
	// A same-named custom template must also survive an incomplete batch/retry.
	existing, err := d.CreateTaskTemplate(TaskTemplateInput{
		Name: builtinVulnHuntingPlaybooks[0].name, Description: "自定义描述", Goal: "自定义目标",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Open exercises the startup path with the v1 marker already present.
	upgraded, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	list, err := upgraded.ListTaskTemplates()
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]*TaskTemplate)
	for _, tpl := range list {
		byName[tpl.Name] = tpl
	}
	for _, name := range []string{
		"Playbook：SRC / 众测漏洞挖掘",
		"Playbook：API 越权与业务逻辑漏洞挖掘",
		"Playbook：源码审计与漏洞验证",
	} {
		if byName[name] == nil {
			t.Errorf("upgrade did not add %q", name)
		}
	}
	if tpl := byName[existing.Name]; tpl == nil || tpl.ID != existing.ID || tpl.Description != "自定义描述" || tpl.Goal != "自定义目标" {
		t.Errorf("same-named custom template changed: %+v", tpl)
	}
	if tpl := byName[legacyName]; tpl == nil || tpl.Goal != "用户的测试目标" {
		t.Errorf("edited legacy template changed: %+v", tpl)
	}
	if byName[deletedLegacyName] != nil {
		t.Error("deleted legacy template was recreated")
	}
	if v, _, err := upgraded.GetSetting("task_playbooks_v2"); err != nil || v != "done" {
		t.Fatalf("v2 marker=%q err=%v", v, err)
	}
}

func TestVulnHuntingPlaybooksRestartPreservesEditsAndDeletions(t *testing.T) {
	d := prepareVulnHuntingPlaybookUpgrade(t)
	if err := d.seedBuiltinTaskPlaybooks(); err != nil {
		t.Fatal(err)
	}
	editedName := "Playbook：API 越权与业务逻辑漏洞挖掘"
	deletedName := "Playbook：源码审计与漏洞验证"
	if _, err := d.Exec(`UPDATE task_templates SET goal='调整后的 API 目标' WHERE name=$1`, editedName); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DELETE FROM task_templates WHERE name=$1`, deletedName); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var goal string
	if err := reopened.QueryRow(`SELECT goal FROM task_templates WHERE name=$1`, editedName).Scan(&goal); err != nil || goal != "调整后的 API 目标" {
		t.Fatalf("edited template goal=%q err=%v", goal, err)
	}
	var count int
	if err := reopened.QueryRow(`SELECT count(*) FROM task_templates WHERE name=$1`, deletedName).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted template count=%d err=%v", count, err)
	}
}
