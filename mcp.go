package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	sdk "github.com/tuitui-open/bot-sdk-golang"
)

type Options struct {
	Defaults       Credentials
	SDKOptions     *sdk.ClientOptions
	Logger         *slog.Logger
	RequestTimeout time.Duration
}

type requestIPKey struct{}

func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(strings.SplitN(r.Header.Get("X-Forwarded-For"), ",", 2)[0]); ip != "" {
		return ip
	}
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func NewServer(options Options) http.Handler {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	timeout := options.RequestTimeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	s := server.NewMCPServer("tuitui", "1.0.0")
	RegisterTools(s, logger)
	stream := server.NewStreamableHTTPServer(s, server.WithStateLess(true))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				logger.Error("mcp handler panic")
				http.Error(w, "内部错误", http.StatusInternalServerError)
			}
		}()
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		ctx = context.WithValue(ctx, requestIPKey{}, clientIP(r))
		controller := http.NewResponseController(w)
		deadline, _ := ctx.Deadline()
		if err := controller.SetReadDeadline(deadline); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Read the body before the tool call so net/http can
		// detect a client disconnect and cancel the upstream request promptly.
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			status := http.StatusBadRequest
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				status = http.StatusRequestTimeout
			}
			w.Header().Set("Connection", "close")
			http.Error(w, err.Error(), status)
			return
		}
		if err := controller.SetReadDeadline(time.Time{}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		// 仅在 tools/call 时解析凭证并创建 SDK 客户端，不影响初始化和工具发现。
		ctx = WithTools(ctx, func() (*Tools, error) {
			credentials, err := options.Defaults.Resolve(r)
			if err != nil {
				return nil, err
			}
			return NewTools(credentials, options.SDKOptions), nil
		})
		stream.ServeHTTP(w, r.WithContext(ctx))
	})
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	return mux
}

type toolsContextKey struct{}

func WithTools(ctx context.Context, resolve func() (*Tools, error)) context.Context {
	return context.WithValue(ctx, toolsContextKey{}, resolve)
}
func ToolsFromContext(ctx context.Context) (*Tools, error) {
	resolve, ok := ctx.Value(toolsContextKey{}).(func() (*Tools, error))
	if !ok {
		return nil, errors.New("缺少请求机器人身份")
	}
	return resolve()
}

//go:embed tools.json
var catalog []byte

func Definitions() []mcp.Tool {
	var definitions []mcp.Tool
	if err := json.Unmarshal(catalog, &definitions); err != nil {
		panic("invalid embedded tool catalog")
	}
	return definitions
}

func RegisterTools(s *server.MCPServer, logger *slog.Logger) {
	for _, definition := range Definitions() {
		def := definition
		s.AddTool(def, func(ctx context.Context, r mcp.CallToolRequest) (result *mcp.CallToolResult, callErr error) {
			start := time.Now()
			ip, _ := ctx.Value(requestIPKey{}).(string)
			tools, err := ToolsFromContext(ctx)
			appid := ""
			if tools != nil {
				appid = tools.credentials.AppID
			}
			defer func() {
				output := ""
				if result != nil && len(result.Content) > 0 {
					if content, ok := result.Content[0].(mcp.TextContent); ok {
						output = content.Text
					}
				}
				attributes := []any{"appid", appid, "ip", ip,
					"duration_ms", time.Since(start).Milliseconds(),
					"success", callErr == nil && result != nil && !result.IsError}
				var response struct {
					TransID string `json:"trans_id"`
				}
				if json.Unmarshal([]byte(output), &response) == nil && response.TransID != "" {
					attributes = append(attributes, "trans_id", response.TransID)
				}
				attributes = append(attributes, "tool", def.Name, "input", r.GetArguments(), "output", output)
				logger.InfoContext(ctx, "tool call", attributes...)
			}()
			if err != nil {
				return mcp.NewToolResultError(errorReason(err)), nil
			}
			args, err := parse(def, r.GetArguments())
			if err != nil {
				return mcp.NewToolResultError(errorReason(err)), nil
			}
			value, err := tools.Execute(ctx, def.Name, args)
			if err != nil {
				return mcp.NewToolResultError(errorReason(err)), nil
			}
			// 成功时过滤这些字段，避免干扰大模型解读；失败时保留完整服务端响应。
			if response, ok := value.(sdk.APIResponse); ok {
				for _, key := range []string{"appid", "errcode", "errmsg", "robot_account", "robot_desc", "robot_name", "robot_uid", "time"} {
					delete(response, key)
				}
			}
			if str, ok := value.(string); ok {
				return mcp.NewToolResultText(str), nil
			}
			b, err := json.Marshal(value)
			if err != nil {
				return mcp.NewToolResultError(errorReason(err)), nil
			}
			return mcp.NewToolResultText(string(b)), nil
		})
	}
}

func errorReason(err error) string {
	if err == nil {
		return ""
	}
	var apiError *sdk.APIError
	if errors.As(err, &apiError) && apiError.Response != nil {
		var response []byte
		switch value := apiError.Response.(type) {
		case string:
			response = []byte(value)
		case []byte:
			response = value
		case json.RawMessage:
			response = value
		default:
			response, _ = json.Marshal(value)
		}
		if json.Valid(response) {
			return string(response)
		}
	}
	return err.Error()
}

func parse(def mcp.Tool, args map[string]any) (map[string]any, error) {
	out := map[string]any{}
	required := map[string]bool{}
	for _, name := range def.InputSchema.Required {
		required[name] = true
		if _, ok := args[name]; !ok {
			return nil, fmt.Errorf("缺少参数 %s", name)
		}
	}
	for name, raw := range def.InputSchema.Properties {
		property := object(raw)
		v, ok := args[name]
		if !ok {
			v, ok = property["default"]
			if !ok {
				continue
			}
		}
		switch text(property["type"]) {
		case "string":
			str, ok := v.(string)
			if !ok || required[name] && strings.TrimSpace(str) == "" {
				return nil, fmt.Errorf("参数 %s 必须为非空字符串", name)
			}
		case "boolean":
			if _, ok := v.(bool); !ok {
				return nil, fmt.Errorf("参数 %s 必须为布尔值", name)
			}
		case "integer":
			switch n := v.(type) {
			case float64:
				if n != float64(int(n)) {
					return nil, fmt.Errorf("参数 %s 必须为整数", name)
				}
				v = int(n)
			case int:
			default:
				return nil, fmt.Errorf("参数 %s 必须为整数", name)
			}
		case "array":
			arr, ok := v.([]any)
			if !ok {
				return nil, fmt.Errorf("参数 %s 必须为数组", name)
			}
			if text(object(property["items"])["type"]) == "string" {
				for _, item := range arr {
					if _, ok := item.(string); !ok {
						return nil, fmt.Errorf("参数 %s 必须为字符串数组", name)
					}
				}
			}
		}
		out[name] = v
	}
	return out, nil
}
