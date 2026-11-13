package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadE2EConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "local.env")
	for _, key := range []string{"TUITUI_APP_ID", "TUITUI_APP_SECRET", "TUITUI_E2E_ACCOUNT", "TUITUI_E2E_IMAGE_URL", "TUITUI_E2E_FILE_URL", "TUITUI_E2E_INVALID_IMAGE_URL", "TUITUI_E2E_INVALID_FILE_URL"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	if c, options, err := LoadE2EConfig(p); err != nil || c != (Credentials{}) || options != (E2EOptions{}) {
		t.Fatal("缺失配置时不应提供默认值")
	}
	if err := os.WriteFile(p, []byte("# fixture\nTUITUI_APP_ID=12\nTUITUI_APP_SECRET='fixture'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, options, err := LoadE2EConfig(p)
	if err != nil || c != (Credentials{"12", "fixture"}) || options != (E2EOptions{}) {
		t.Fatal("配置未正确加载")
	}
	if err := os.WriteFile(p, []byte("TUITUI_APP_ID=12\nTUITUI_APP_SECRET=fixture\nTUITUI_E2E_ACCOUNT='test-user'\nTUITUI_E2E_IMAGE_URL=https://example.com/image.png\nTUITUI_E2E_FILE_URL=https://example.com/file.txt\nTUITUI_E2E_INVALID_IMAGE_URL=https://example.com/blocked.png\nTUITUI_E2E_INVALID_FILE_URL=http://192.168.1.1/test.txt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, options, err = LoadE2EConfig(p)
	if err != nil || options != (E2EOptions{Account: "test-user", ImageURL: "https://example.com/image.png", FileURL: "https://example.com/file.txt", InvalidImageURL: "https://example.com/blocked.png", InvalidFileURL: "http://192.168.1.1/test.txt"}) {
		t.Fatal("E2E 测试参数未从配置文件加载")
	}
	t.Setenv("TUITUI_APP_SECRET", "override")
	t.Setenv("TUITUI_E2E_ACCOUNT", "override-user")
	t.Setenv("TUITUI_E2E_IMAGE_URL", "https://example.com/override.png")
	t.Setenv("TUITUI_E2E_FILE_URL", "https://example.com/override.txt")
	c, options, err = LoadE2EConfig(p)
	if err != nil || c.Secret != "fixture" || options.Account != "test-user" || options.ImageURL != "https://example.com/image.png" || options.FileURL != "https://example.com/file.txt" {
		t.Fatal("E2E 配置应仅从文件读取")
	}
}
