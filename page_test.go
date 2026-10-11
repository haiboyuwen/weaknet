package main

import (
	"os/exec"
	"strings"
	"testing"
)

// 页面不依赖构建工具；有 Node 时额外检查内嵌脚本语法。
func TestPageScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("未安装 Node，跳过页面脚本语法检查")
	}
	_, script, ok := strings.Cut(string(pageHTML), "<script>")
	if !ok {
		t.Fatal("页面缺少 script")
	}
	script, _, ok = strings.Cut(script, "</script>")
	if !ok {
		t.Fatal("页面缺少 script 结束标签")
	}
	cmd := exec.Command(node, "--check")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("页面脚本语法错误：%v\n%s", err, out)
	}
}

func TestStateKeepsRecentEvents(t *testing.T) {
	app := newTestApp(t, "unlimited")
	for i := 0; i < 310; i++ {
		app.event("[HTTP][App 日志] https://api.example/room")
	}
	st := app.state("")
	if len(st.Events) != 300 {
		t.Fatalf("事件数 = %d，期望 300", len(st.Events))
	}
	st.Events[0] = "不能修改内部存储"
	if strings.Contains(eventText(app), st.Events[0]) {
		t.Fatal("状态暴露了内部事件切片")
	}
}
