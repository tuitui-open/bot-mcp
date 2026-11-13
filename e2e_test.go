package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestE2EConsoleSummary(t *testing.T) {
	s := &e2eSuite{
		report:  E2EReport{Run: "test-run", Protocol: "passed", Resources: map[string]string{"group_id": "20"}, Retained: []string{"group:20"}},
		results: map[string]E2EResult{},
	}
	for _, def := range Definitions() {
		s.results[def.Name] = E2EResult{Tool: def.Name, Status: "passed"}
	}
	var output bytes.Buffer
	if err := s.finish(&output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"test-run", "group_id: 20", "保留资源: group:20", "21/21 passed"} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("命令行缺少结果：%s", expected)
		}
	}

	s.report.Results = nil
	s.results[Definitions()[0].Name] = E2EResult{Tool: Definitions()[0].Name, Status: "failed", Detail: "模拟失败原因"}
	output.Reset()
	if err := s.finish(&output); err == nil {
		t.Fatal("失败测试不得返回成功")
	}
	if !strings.Contains(output.String(), "模拟失败原因") {
		t.Fatal("命令行未输出失败原因")
	}
}
