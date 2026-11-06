package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道失败: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	runErr := fn()
	if err := w.Close(); err != nil {
		t.Fatalf("关闭管道失败: %v", err)
	}
	data, readErr := io.ReadAll(r)
	if err := r.Close(); err != nil {
		t.Fatalf("关闭读取端失败: %v", err)
	}
	if readErr != nil {
		t.Fatalf("读取 stdout 失败: %v", readErr)
	}
	return string(data), runErr
}

type apiKeyAddTestOutput struct {
	Name    string `json:"name"`
	APIKey  string `json:"api_key"`
	Masked  string `json:"masked"`
	Auth    string `json:"auth"`
	Warning string `json:"warning"`
}

type apiKeyListTestOutput struct {
	Auth string `json:"auth"`
	Keys []struct {
		Name   string `json:"name"`
		Masked string `json:"masked"`
	} `json:"keys"`
}

type apiKeyRemoveTestOutput struct {
	Name    string `json:"name"`
	Removed bool   `json:"removed"`
	Auth    string `json:"auth"`
	Warning string `json:"warning"`
}

func TestAPIKeyCLI(t *testing.T) {
	home := isolateHome(t)

	addOut, err := captureStdout(t, func() error {
		return runAPIKey([]string{"add", "laptop"})
	})
	if err != nil {
		t.Fatalf("apikey add 失败: %v", err)
	}
	var added apiKeyAddTestOutput
	if err := json.Unmarshal([]byte(addOut), &added); err != nil {
		t.Fatalf("add 输出不是合法 JSON: %v\n%s", err, addOut)
	}
	if added.Name != "laptop" || added.APIKey == "" || added.Masked == "" || added.Auth != "enforced" || added.Warning == "" {
		t.Fatalf("add 输出不符: %#v", added)
	}
	if !strings.HasPrefix(added.APIKey, "dsf_") {
		t.Fatalf("密钥前缀不符: %q", added.APIKey)
	}
	if _, err := os.Stat(filepath.Join(home, dataDirName, authDBFileName)); err != nil {
		t.Fatalf("auth.db 应落在数据目录: %v", err)
	}

	listOut, err := captureStdout(t, func() error {
		return runAPIKey([]string{"list"})
	})
	if err != nil {
		t.Fatalf("apikey list 失败: %v", err)
	}
	var listed apiKeyListTestOutput
	if err := json.Unmarshal([]byte(listOut), &listed); err != nil {
		t.Fatalf("list 输出不是合法 JSON: %v\n%s", err, listOut)
	}
	if listed.Auth != "enforced" || len(listed.Keys) != 1 || listed.Keys[0].Name != "laptop" || listed.Keys[0].Masked != added.Masked {
		t.Fatalf("list 输出不符: %#v", listed)
	}
	if strings.Contains(listOut, added.APIKey) {
		t.Fatal("list 不应输出完整密钥")
	}

	removeOut, err := captureStdout(t, func() error {
		return runAPIKey([]string{"remove", "laptop"})
	})
	if err != nil {
		t.Fatalf("apikey remove 失败: %v", err)
	}
	var removed apiKeyRemoveTestOutput
	if err := json.Unmarshal([]byte(removeOut), &removed); err != nil {
		t.Fatalf("remove 输出不是合法 JSON: %v\n%s", err, removeOut)
	}
	if !removed.Removed || removed.Auth != "disabled" || removed.Warning == "" {
		t.Fatalf("remove 输出不符: %#v", removed)
	}

	logData, err := os.ReadFile(filepath.Join(home, dataDirName, "logs", "server.log"))
	if err != nil {
		t.Fatalf("读取审计日志失败: %v", err)
	}
	foundRemoveLog := false
	for _, line := range strings.Split(string(logData), "\n") {
		if !strings.Contains(line, "删除访问密钥") {
			continue
		}
		foundRemoveLog = true
		if !strings.Contains(line, "masked=") {
			t.Fatalf("remove 审计日志缺少 masked: %s", line)
		}
	}
	if !foundRemoveLog {
		t.Fatalf("缺少 remove 审计日志: %s", logData)
	}

	listOut, err = captureStdout(t, func() error {
		return runAPIKey([]string{"list"})
	})
	if err != nil {
		t.Fatalf("apikey list 失败: %v", err)
	}
	if err := json.Unmarshal([]byte(listOut), &listed); err != nil {
		t.Fatalf("list 输出不是合法 JSON: %v\n%s", err, listOut)
	}
	if listed.Auth != "disabled" || len(listed.Keys) != 0 {
		t.Fatalf("删除最后一个密钥后 list 不符: %#v", listed)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"localhost", true},
		{"LOCALHOST", true},
		{"0.0.0.0", false},
		{"192.168.1.10", false},
		{"example.com", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isLoopbackHost(tc.host); got != tc.want {
			t.Fatalf("isLoopbackHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestAPIKeyAddWithDataDirFlag(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()

	out, err := captureStdout(t, func() error {
		return runAPIKey([]string{"add", "--data-dir", dir, "laptop"})
	})
	if err != nil {
		t.Fatalf("apikey add --data-dir 失败: %v", err)
	}
	var added apiKeyAddTestOutput
	if err := json.Unmarshal([]byte(out), &added); err != nil {
		t.Fatalf("add 输出不是合法 JSON: %v\n%s", err, out)
	}
	if added.Name != "laptop" || added.APIKey == "" {
		t.Fatalf("add 输出不符: %#v", added)
	}
	if _, err := os.Stat(filepath.Join(dir, authDBFileName)); err != nil {
		t.Fatalf("auth.db 应落在 --data-dir 下: %v", err)
	}
}
