//go:build e2e

package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestE2EPrivateImageServerError(t *testing.T) {
	credentials, options, err := LoadE2EConfig(".env.e2e")
	if err != nil || !credentials.valid() {
		t.Fatal("E2E 测试凭证不可用")
	}
	if options.InvalidImageURL == "" {
		t.Fatal("请配置 TUITUI_E2E_INVALID_IMAGE_URL")
	}
	logs, err := newDailyLogWriter("log", 7, time.Now)
	if err != nil {
		t.Fatal("无法创建 E2E 日志")
	}
	defer logs.Close()
	server := httptest.NewServer(NewServer(Options{Defaults: credentials, Logger: slog.New(slog.NewJSONHandler(logs, nil))}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := OpenMCP(ctx, server.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var request mcp.CallToolRequest
	request.Params.Name = "tuitui_send_private_image"
	request.Params.Arguments = map[string]any{"account": options.Account, "url": options.InvalidImageURL}
	result, err := client.CallTool(ctx, request)
	if err != nil {
		t.Fatal("MCP 调用失败")
	}
	if result == nil || !result.IsError || len(result.Content) != 1 {
		t.Fatal("应返回图片下载错误")
	}
	content, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatal("错误原因应为文本")
	}
	if err := expectServerError(errors.New(content.Text)); err != nil {
		t.Fatal(err)
	}
}
