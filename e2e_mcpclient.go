package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

func OpenMCP(ctx context.Context, address string, headers map[string]string) (*client.Client, error) {
	c, err := client.NewStreamableHttpClient(address, transport.WithHTTPHeaders(headers))
	if err != nil {
		return nil, fmt.Errorf("无法创建 MCP 客户端: %w", err)
	}
	if err = c.Start(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("MCP 客户端启动失败: %w", err)
	}
	var request mcp.InitializeRequest
	request.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	request.Params.ClientInfo = mcp.Implementation{Name: "bot-mcp-e2e", Version: "1"}
	if _, err = c.Initialize(ctx, request); err != nil {
		c.Close()
		return nil, fmt.Errorf("MCP 初始化失败: %w", err)
	}
	return c, nil
}
func CallMCP(ctx context.Context, c *client.Client, name string, args map[string]any) (any, error) {
	var request mcp.CallToolRequest
	request.Params.Name = name
	request.Params.Arguments = args
	result, err := c.CallTool(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("MCP HTTP 调用失败: %w", err)
	}
	if result == nil || len(result.Content) != 1 {
		return nil, errors.New("MCP 返回内容无效")
	}
	content, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		return nil, errors.New("MCP 返回类型无效")
	}
	if result.IsError {
		return nil, errors.New(content.Text)
	}
	var value any
	if json.Unmarshal([]byte(content.Text), &value) != nil {
		return content.Text, nil
	}
	return value, nil
}
