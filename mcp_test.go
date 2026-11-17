package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	sdk "github.com/tuitui-open/bot-sdk-golang"
)

func TestToolDefinitions(t *testing.T) {
	definitions := Definitions()
	if len(definitions) == 0 {
		t.Fatal("工具定义不能为空")
	}
	for i, def := range definitions {
		if def.Name == "" || def.Description == "" {
			t.Fatal("工具名称和描述不能为空")
		}
		if i > 0 && definitions[i-1].Name >= def.Name {
			t.Fatal("工具必须按字母序排列且名称唯一")
		}
		for _, name := range def.InputSchema.Required {
			if _, ok := def.InputSchema.Properties[name]; !ok {
				t.Errorf("%s 缺少必填参数 %s 的定义", def.Name, name)
			}
		}
	}
}

func TestRequestBodyReadTimeout(t *testing.T) {
	s := httptest.NewServer(NewServer(Options{RequestTimeout: 30 * time.Millisecond}))
	defer s.Close()
	conn, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, err = io.WriteString(conn, "POST /mcp HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("应在请求体读取超时后返回 HTTP 408: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408", response.StatusCode)
	}
}

func TestDefaultLoggerWritesToolCallsToFile(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "log")
	now := time.Now()
	writer, err := newDailyLogWriter(directory, 7, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(writer, nil)))
	defer slog.SetDefault(previous)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]any{"trans_id": "default-log-trans", "users": []any{}})
	}))
	defer upstream.Close()
	// RunE2E 同样不传 Logger，应使用 main 初始化的日志目录。
	s := httptest.NewServer(NewServer(Options{Defaults: Credentials{"1", "fixture-secret"}, SDKOptions: &sdk.ClientOptions{APIBaseURL: upstream.URL}}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := OpenMCP(ctx, s.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := CallMCP(ctx, c, "tuitui_search_account", map[string]any{"keyword": "test-user"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	data, err := os.ReadFile(filepath.Join(directory, now.Format("2006-01-02")+".log"))
	if err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["msg"] != "tool call" || entry["tool"] != "tuitui_search_account" || entry["trans_id"] != "default-log-trans" || entry["success"] != true {
		t.Fatal("未将 tool call 及 trans_id 写入日志目录")
	}
}

func TestErrorReason(t *testing.T) {
	response := sdk.APIResponse{"errcode": 9, "errmsg": "simulated server error"}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	apiError := &sdk.APIError{Message: "SDK wrapper", Response: response}
	networkError := &sdk.APIError{Message: "request failed", Cause: errors.New("dial tcp: connection refused")}
	nonJSONError := &sdk.APIError{Message: "invalid JSON", Response: "<html>gateway error</html>"}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"server JSON", apiError, string(encoded)},
		{"wrapped server JSON", fmt.Errorf("wrapped: %w", apiError), string(encoded)},
		{"raw JSON", &sdk.APIError{Response: `{"errcode":9,"errmsg":"raw error"}`}, `{"errcode":9,"errmsg":"raw error"}`},
		{"network", networkError, networkError.Error()},
		{"non JSON response", nonJSONError, nonJSONError.Error()},
		{"local error", errors.New("invalid argument"), "invalid argument"},
		{"no error", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorReason(tc.err); got != tc.want {
				t.Errorf("错误原因不符：got %q, want %q", got, tc.want)
			}
		})
	}
}
func TestArguments(t *testing.T) {
	for _, def := range Definitions() {
		args := map[string]any{}
		for _, name := range def.InputSchema.Required {
			switch text(object(def.InputSchema.Properties[name])["type"]) {
			case "string":
				args[name] = "valid"
			case "array":
				args[name] = []any{"valid"}
			}
		}
		if _, err := parse(def, args); err != nil {
			t.Errorf("默认参数失败 %s: %v", def.Name, err)
		}
		for _, name := range def.InputSchema.Required {
			delete(args, name)
			if _, err := parse(def, args); err == nil {
				t.Errorf("未校验必填 %s.%s", def.Name, name)
			}
			break
		}
	}
}

func TestInvalidToolArguments(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		jsonResponse(w, map[string]any{"errcode": 0})
	}))
	defer upstream.Close()
	var logs bytes.Buffer
	s := httptest.NewServer(NewServer(Options{
		Defaults:   Credentials{"1", "fixture-secret"},
		SDKOptions: &sdk.ClientOptions{APIBaseURL: upstream.URL},
		Logger:     slog.New(slog.NewJSONHandler(&logs, nil)),
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := OpenMCP(ctx, s.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, def := range Definitions() {
		t.Run(def.Name, func(t *testing.T) {
			check := func(args any) {
				t.Helper()
				var request mcp.CallToolRequest
				request.Params.Name = def.Name
				request.Params.Arguments = args
				result, err := c.CallTool(ctx, request)
				if err != nil || result == nil || !result.IsError {
					t.Fatalf("invalid arguments %#v: result=%+v, err=%v", args, result, err)
				}
			}
			// Invalid arguments containers must also return a tool error.
			for _, args := range []any{nil, true, 42, "invalid", []any{}, map[string]any{}} {
				check(args)
			}
			for name, raw := range def.InputSchema.Properties {
				invalid := []any{nil, map[string]any{}}
				switch text(object(raw)["type"]) {
				case "string":
					invalid = append(invalid, true, 42, []any{})
				case "array":
					invalid = append(invalid, true, 42, "invalid", []any{nil}, []any{42}, []any{map[string]any{}})
				case "integer":
					invalid = append(invalid, true, "20", []any{}, 1.5, 1e100)
				case "boolean":
					invalid = append(invalid, "true", 42, []any{})
				}
				for _, value := range invalid {
					args := map[string]any{}
					for _, required := range def.InputSchema.Required {
						args[required] = "fixture"
						if text(object(def.InputSchema.Properties[required])["type"]) == "array" {
							args[required] = []any{"fixture"}
						}
					}
					args[name] = value
					check(args)
				}
			}
		})
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("invalid arguments reached upstream %d times", upstreamCalls.Load())
	}
	if strings.Contains(logs.String(), "mcp handler panic") {
		t.Fatal("invalid arguments triggered panic recovery")
	}
}

func TestMalformedMCPRequestsDoNotPanic(t *testing.T) {
	var logs bytes.Buffer
	s := httptest.NewServer(NewServer(Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}))
	defer s.Close()
	for _, body := range []string{
		``, `{`, `null`, `true`, `42`, `"invalid"`, `[]`, `[{}]`, `{}`,
		`{"jsonrpc":"2.0","id":{},"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":[],"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1e100,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"method":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":[]}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":null}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":{}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"unknown"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":[]}}`,
	} {
		t.Run(body, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, s.URL+"/mcp", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			client := &http.Client{Timeout: 2 * time.Second}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if _, err := io.Copy(io.Discard, response.Body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode >= 500 {
				t.Fatalf("malformed request returned HTTP %d", response.StatusCode)
			}
		})
	}
	if strings.Contains(logs.String(), "mcp handler panic") {
		t.Fatal("malformed request triggered panic recovery")
	}
}

func TestSchemaDefaultsAndTypes(t *testing.T) {
	if bytes.Contains(catalog, []byte(`"parameters"`)) {
		t.Fatal("工具定义不应包含重复的 parameters")
	}
	for _, def := range Definitions() {
		switch def.Name {
		case "tuitui_get_group_messages":
			args, err := parse(def, map[string]any{"group_id": "2"})
			if err != nil || args["last"] != 20 {
				t.Fatal("last 应使用 schema 的整数默认值")
			}
			args, err = parse(def, map[string]any{"group_id": "2", "last": float64(100)})
			if err != nil || args["last"] != 100 {
				t.Fatal("整数参数未正确转换")
			}
			for _, invalid := range []any{"20", 1.5, true} {
				if _, err := parse(def, map[string]any{"group_id": "2", "last": invalid}); err == nil {
					t.Fatal("last 接受了非整数")
				}
			}
		case "tuitui_get_private_messages":
			args, err := parse(def, map[string]any{"account": "test-user"})
			if err != nil || args["relative_time"] != "today" || args["cursor"] != "0" {
				t.Fatal("字符串默认值未保留")
			}
		case "tuitui_send_private_strong_call":
			args, err := parse(def, map[string]any{"account": "test-user", "content": "test"})
			if err != nil || args["sms_notice"] != false || args["call_notice"] != false {
				t.Fatal("布尔默认值未保留")
			}
		case "tuitui_send_private_file":
			args, err := parse(def, map[string]any{"account": "test-user", "url": "test-url"})
			if err != nil {
				t.Fatal("可选文件名不应成为必填参数")
			}
			if _, ok := args["filename"]; ok {
				t.Fatal("不应为无默认值的字段生成默认值")
			}
		}
	}
}

type apiFixture struct {
	mu         sync.Mutex
	paths      []string
	payloads   map[string]map[string]any
	identities []string
	authInURL  bool
}

func TestClientIP(t *testing.T) {
	for _, tt := range []struct {
		name, forwarded, real, remote, want string
	}{
		{"forwarded", " 192.0.2.1, 192.0.2.2", "192.0.2.3", "127.0.0.1:1234", "192.0.2.1"},
		{"real", "", " 192.0.2.3 ", "127.0.0.1:1234", "192.0.2.3"},
		{"empty forwarded", " ", "192.0.2.3", "127.0.0.1:1234", "192.0.2.3"},
		{"connection", "", "", "127.0.0.1:1234", "127.0.0.1"},
		{"ipv6 connection", "", "", "[2001:db8::1]:1234", "2001:db8::1"},
		{"no port", "", "", "192.0.2.4", "192.0.2.4"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			r.Header.Set("X-Forwarded-For", tt.forwarded)
			r.Header.Set("X-Real-IP", tt.real)
			r.RemoteAddr = tt.remote
			if got := clientIP(r); got != tt.want {
				t.Fatalf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func (f *apiFixture) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.URL.Path)
	f.identities = append(f.identities, r.Header.Get("Authorization"))
	f.authInURL = f.authInURL || r.URL.Query().Has("appid") || r.URL.Query().Has("secret")
	var p map[string]any
	json.NewDecoder(r.Body).Decode(&p)
	if f.payloads == nil {
		f.payloads = map[string]map[string]any{}
	}
	f.payloads[r.URL.Path] = p
	out := map[string]any{"errcode": 0, "errmsg": "ok", "trans_id": "fixture-" + r.URL.Path}
	out["appid"] = "test-app"
	out["robot_account"] = "test-bot"
	out["robot_desc"] = "test description"
	out["robot_name"] = "test name"
	out["robot_uid"] = "99"
	out["time"] = "test-time"
	out["nested"] = map[string]any{"appid": "nested-app", "errcode": 0, "time": "nested-time"}
	switch r.URL.Path {
	case "/prop/get":
		out["robot_account"] = "test-bot"
		out["robot_uid"] = "99"
	case "/message/custom/send":
		out["msgids"] = []any{map[string]any{"user": "test-user", "group": "20", "msgid": "10001"}}
		out["post_ids"] = []any{map[string]any{"team_id": "30", "channel_id": "40", "post_id": "50"}}
	case "/message/custom/modify":
		out["success"] = []any{map[string]any{"user": "test-user", "msgid": "10001"}}
	case "/message/single/readstatus":
		out["read"] = false
	case "/message/single/sync":
		out["msgs"] = []any{map[string]any{"event": "single_chat", "data": map[string]any{"msg_type": "text", "text": "hello"}}}
		out["has_more"] = false
	case "/message/group/sync":
		out["msgs"] = []any{map[string]any{"event": "group_chat", "user_name": "Test User", "data": map[string]any{"msg_type": "text", "text": "hello"}}}
	case "/user/search":
		out["users"] = []any{map[string]any{"name": "测试用户", "account": "test-user", "user_id": "10001", "department": "测试"}}
	case "/user/ids":
		out["users"] = []any{map[string]any{"user": "test-user", "id": 123}}
	case "/group/create":
		out["group_id"] = "20"
	case "/group/member/add", "/group/member/remove", "/strongNotice/single/send":
	case "/teams/team/create":
		out["datas"] = map[string]any{"team_id": "30", "default_channel_id": "31"}
	case "/teams/channel/create":
		out["datas"] = map[string]any{"channel_id": "40"}
	case "/teams/member/add":
		out["datas"] = map[string]any{"members": []string{"123"}}
	case "/teams/team/info/rich":
		out["datas"] = map[string]any{"info": map[string]any{"channels": []any{map[string]any{"channel_id": "40", "name": "test"}}}}
	case "/teams/channel/info":
		out["datas"] = map[string]any{"info": map[string]any{"team_id": "30"}}
	case "/teams/post/topic/list":
		out["datas"] = map[string]any{"post_list": []any{}}
	default:
		out["errcode"] = 404
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
func TestAllToolsThroughHTTP(t *testing.T) {
	fixture := &apiFixture{}
	upstream := httptest.NewServer(http.HandlerFunc(fixture.handler))
	defer upstream.Close()
	var logs bytes.Buffer
	s := httptest.NewServer(NewServer(Options{Defaults: Credentials{"1", "fixture-secret"}, SDKOptions: &sdk.ClientOptions{APIBaseURL: upstream.URL}, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := OpenMCP(ctx, s.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	list, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil || len(list.Tools) != len(Definitions()) {
		t.Fatal("工具发现失败")
	}
	args := map[string]any{"account": "test-user", "accounts": []any{"test-user"}, "message": "hello", "markdown": "hello", "content": "hello", "url": "https://example.com/a.png", "filename": "test.png", "msgid": "10001", "group_id": "20", "team_id": "30", "channel_id": "40", "name": "test", "members": []any{"test-user"}, "keyword": "test-user"}
	for _, def := range Definitions() {
		t.Run(def.Name, func(t *testing.T) {
			result, err := CallMCP(ctx, c, def.Name, args)
			if err != nil {
				t.Fatal(err)
			}
			if result == nil && def.Name != "tuitui_get_private_messages" {
				t.Fatal("缺少结果")
			}
			response := object(result)
			for _, key := range []string{"appid", "errcode", "errmsg", "robot_account", "robot_desc", "robot_name", "robot_uid", "time"} {
				if _, exists := response[key]; exists {
					t.Errorf("成功响应未过滤 %s", key)
				}
			}
			if text(response["trans_id"]) == "" || object(response["nested"])["appid"] != "nested-app" || object(response["nested"])["time"] != "nested-time" {
				t.Fatal("应保留 trans_id 及嵌套业务字段")
			}
		})
	}
	s.Close()
	decoder := json.NewDecoder(&logs)
	logged := 0
	for {
		var entry map[string]any
		if err := decoder.Decode(&entry); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if entry["msg"] != "tool call" {
			continue
		}
		logged++
		transID := text(entry["trans_id"])
		if !strings.HasPrefix(transID, "fixture-") || transID == "fixture-/prop/get" {
			t.Errorf("成功工具 %s 未记录对应服务端 trans_id", entry["tool"])
		}
	}
	if logged != len(Definitions()) {
		t.Fatal("未验证全部工具的成功日志")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.authInURL {
		t.Error("SDK 请求 URL 不应包含凭证")
	}
	for _, header := range fixture.identities {
		if header != "Bearer 1-fixture-secret" {
			t.Error("所有 SDK 请求都应使用 Bearer Header")
		}
	}
	if fixture.payloads["/teams/member/add"]["members[]"] == nil {
		t.Error("团队成员字段不正确")
	}
	if fixture.payloads["/teams/channel/create"]["team_id"] != "30" {
		t.Error("频道团队字段不正确")
	}
	if fixture.payloads["/message/single/readstatus"]["msgid"] != "10001" {
		t.Error("消息 ID 参数发生变化")
	}
}
func TestEndpointCredentials(t *testing.T) {
	var mu sync.Mutex
	identities := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appid, secret, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")
		if r.URL.Query().Has("appid") || r.URL.Query().Has("secret") {
			t.Error("SDK 请求 URL 不应包含凭证")
		}
		mu.Lock()
		identities = append(identities, appid)
		mu.Unlock()
		if secret == "abc" {
			json.NewEncoder(w).Encode(map[string]any{"errcode": 401})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"errcode": 0})
	}))
	defer upstream.Close()
	s := httptest.NewServer(NewServer(Options{Defaults: Credentials{"1", "default"}, SDKOptions: &sdk.ClientOptions{APIBaseURL: upstream.URL}}))
	defer s.Close()
	removed, err := http.Get(s.URL + "/mcp/streamable")
	if err != nil {
		t.Fatal(err)
	}
	removed.Body.Close()
	if removed.StatusCode != http.StatusNotFound {
		t.Fatal("旧兼容路径应返回 404")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, mode := range []struct{ path, header string }{{"/mcp?appid=2&secret=url", ""}, {"/mcp", "Bearer 3-header"}, {"/mcp", ""}} {
		wg.Add(1)
		go func(path, header string) {
			defer wg.Done()
			headers := map[string]string{}
			if header != "" {
				headers["Authorization"] = header
			}
			c, err := OpenMCP(ctx, s.URL+path, headers)
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			list, err := c.ListTools(ctx, mcp.ListToolsRequest{})
			if err != nil || len(list.Tools) != 21 {
				t.Error("端点发现失败")
			}
			if _, err := CallMCP(ctx, c, "tuitui_search_account", map[string]any{"keyword": "test-user"}); err != nil {
				t.Error(err)
			}
		}(mode.path, mode.header)
	}
	wg.Wait()
	c, err := OpenMCP(ctx, s.URL+"/mcp?appid=2&secret=abc", nil)
	if err != nil {
		t.Fatal("初始化不应调用上游验证凭证", err)
	}
	defer c.Close()
	if _, err := CallMCP(ctx, c, "tuitui_send_private_message", map[string]any{"accounts": []any{"zhangsan"}, "message": "test"}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Error("无效凭证应由目标接口拒绝并返回原始错误")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"1", "2", "3"} {
		found := false
		for _, id := range identities {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Error("请求身份未隔离")
		}
	}
}
func TestInitializationIgnoresCredentials(t *testing.T) {
	for _, tc := range []struct{ name, query, header string }{
		{"missing", "", ""},
		{"partial URL", "?appid=1", ""},
		{"empty secret", "?appid=1&secret=", ""},
		{"duplicate URL", "?appid=1&appid=2&secret=test", ""},
		{"invalid bearer", "", "Bearer invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				jsonResponse(w, map[string]any{})
			}))
			defer upstream.Close()
			s := httptest.NewServer(NewServer(Options{SDKOptions: &sdk.ClientOptions{APIBaseURL: upstream.URL}}))
			defer s.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			headers := map[string]string{}
			if tc.header != "" {
				headers["Authorization"] = tc.header
			}
			c, err := OpenMCP(ctx, s.URL+"/mcp"+tc.query, headers)
			if err != nil {
				t.Fatal("凭证不应影响 MCP 初始化", err)
			}
			defer c.Close()
			list, err := c.ListTools(ctx, mcp.ListToolsRequest{})
			if err != nil || len(list.Tools) != len(Definitions()) {
				t.Fatal("凭证不应影响工具发现", err)
			}
			var request mcp.CallToolRequest
			request.Params.Name = "tuitui_search_account"
			request.Params.Arguments = map[string]any{"keyword": "test-user"}
			result, err := c.CallTool(ctx, request)
			if err != nil || result == nil || !result.IsError {
				t.Fatal("凭证错误仅应在工具调用结果中返回", err)
			}
			if upstreamCalls.Load() != 0 {
				t.Fatal("凭证缺失或格式不完整时不应请求上游")
			}
		})
	}
}

func TestToolCallPreservesUpstreamError(t *testing.T) {
	for _, tc := range []struct{ name, want string }{{"server JSON", "auth-test-trans"}, {"connection closed", "EOF"}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/user/search" {
					t.Errorf("不应调用额外接口: %s", r.URL.Path)
				}
				if tc.name == "connection closed" {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					connection.Close()
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"errcode": 9, "errmsg": "test authentication failure", "trans_id": "auth-test-trans"})
			}))
			defer upstream.Close()
			s := httptest.NewServer(NewServer(Options{Defaults: Credentials{"1", "fixture-secret"}, SDKOptions: &sdk.ClientOptions{APIBaseURL: upstream.URL}}))
			defer s.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := OpenMCP(ctx, s.URL+"/mcp?appid=1&secret=fixture-secret", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.ListTools(ctx, mcp.ListToolsRequest{}); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 {
				t.Fatal("初始化及工具发现不应请求上游")
			}
			_, err = CallMCP(ctx, c, "tuitui_search_account", map[string]any{"keyword": "test-user"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("工具错误丢失上游原因: %v", err)
			}
			if strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), "secret=") {
				t.Error("上游失败错误不应包含 URL 凭证")
			}
			if calls.Load() != 1 {
				t.Fatalf("一次工具调用应仅请求目标接口一次，实际: %d", calls.Load())
			}
		})
	}
}

func TestCredentialErrorsNeverLeak(t *testing.T) {
	var logs bytes.Buffer
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(map[string]any{"errcode": 0}) }))
	defer up.Close()
	s := httptest.NewServer(NewServer(Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil)), Defaults: Credentials{"1", "super-secret"}, SDKOptions: &sdk.ClientOptions{APIBaseURL: up.URL}}))
	defer s.Close()
	r, err := http.Post(s.URL+"/mcp?appid=1&secret=super-secret", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if strings.Contains(logs.String(), "super-secret") {
		t.Fatal("密钥进入日志")
	}
}

func TestRequestCancellationIsolation(t *testing.T) {
	reached := make(chan struct{})
	canceled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer 1-one" {
			io.Copy(io.Discard, r.Body)
			close(reached)
			<-r.Context().Done()
			close(canceled)
			return
		}
		jsonResponse(w, map[string]any{})
	}))
	defer up.Close()
	s := httptest.NewServer(NewServer(Options{SDKOptions: &sdk.ClientOptions{APIBaseURL: up.URL}}))
	defer s.Close()
	first, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r, _ := http.NewRequestWithContext(first, "POST", s.URL+"/mcp?appid=1&secret=one", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tuitui_search_account","arguments":{"keyword":"test-user"}}}`))
		r.Header.Set("Content-Type", "application/json")
		rsp, err := http.DefaultClient.Do(r)
		if err == nil {
			rsp.Body.Close()
		}
	}()
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("首个请求未到达上游")
	}
	secondCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	second, err := OpenMCP(secondCtx, s.URL+"/mcp?appid=2&secret=two", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	cancel()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("取消未传播到上游")
	}
	if _, err := CallMCP(secondCtx, second, "tuitui_search_account", map[string]any{"keyword": "test-user"}); err != nil {
		t.Fatal("其他请求被错误取消")
	}
	<-done
}

func TestToolCallLogsInputAndOutput(t *testing.T) {
	var logs bytes.Buffer
	failureResponse := map[string]any{
		"errcode": 9, "errmsg": "simulated API failure", "detail": "original server detail", "trace_id": "test-trace", "trans_id": "test-transaction",
		"appid": "test-app", "robot_account": "test-bot", "robot_desc": "test description", "robot_name": "test name", "robot_uid": "99", "time": "test-time",
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prop/get" {
			jsonResponse(w, map[string]any{})
			return
		}
		var args map[string]any
		json.NewDecoder(r.Body).Decode(&args)
		if args["keyword"] == "fail" {
			json.NewEncoder(w).Encode(failureResponse)
			return
		}
		jsonResponse(w, map[string]any{"users": []any{map[string]any{"account": "test-user", "user_id": "10001"}}, "trans_id": "success-transaction"})
	}))
	defer up.Close()
	s := httptest.NewServer(NewServer(Options{
		Defaults:   Credentials{"1", "fixture-secret"},
		SDKOptions: &sdk.ClientOptions{APIBaseURL: up.URL},
		Logger:     slog.New(slog.NewJSONHandler(&logs, nil)),
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := OpenMCP(ctx, s.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	inputs := []map[string]any{
		{"keyword": "test-user", "note": "https://example.com/raw-input"},
		{},
		{"keyword": "fail"},
	}
	var outputs []string
	for _, input := range inputs {
		var request mcp.CallToolRequest
		request.Params.Name = "tuitui_search_account"
		request.Params.Arguments = input
		result, err := c.CallTool(ctx, request)
		if err != nil {
			c.Close()
			t.Fatal(err)
		}
		if result == nil || len(result.Content) != 1 {
			c.Close()
			t.Fatal("应返回单条文本内容")
		}
		content, ok := result.Content[0].(mcp.TextContent)
		if !ok {
			c.Close()
			t.Fatal("应返回文本内容")
		}
		outputs = append(outputs, content.Text)
	}
	c.Close()
	s.Close()
	decoder := json.NewDecoder(&logs)
	count := 0
	for {
		var entry map[string]any
		if err := decoder.Decode(&entry); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if entry["msg"] != "tool call" {
			continue
		}
		if entry["appid"] != "1" {
			t.Error("工具调用日志缺少正确的 appid")
		}
		if entry["ip"] != "127.0.0.1" {
			t.Error("工具调用日志缺少正确的来源 IP")
		}
		if count >= len(inputs) {
			t.Fatal("重复记录工具调用")
		}
		if !reflect.DeepEqual(entry["input"], inputs[count]) {
			t.Error("未完整记录原始输入")
		}
		if entry["output"] != outputs[count] {
			t.Error("日志输出应仅为 MCP 返回的文本内容")
		}
		if entry["success"] != (count == 0) {
			t.Error("成功状态错误")
		}
		switch count {
		case 0:
			if entry["trans_id"] != "success-transaction" {
				t.Error("成功响应应记录服务端 trans_id")
			}
		case 1:
			if _, ok := entry["trans_id"]; ok {
				t.Error("没有服务端 trans_id 时不应生成该字段")
			}
			if count == 1 && text(entry["output"]) == "" {
				t.Error("参数错误应在 output 中保留原始错误信息")
			}
		case 2:
			expected, _ := json.Marshal(failureResponse)
			if entry["output"] != string(expected) {
				t.Error("失败响应应完整保留所有服务端字段")
			}
			var response map[string]any
			if err := json.Unmarshal([]byte(text(entry["output"])), &response); err != nil {
				t.Fatal("错误原因应为服务端 JSON 字符串")
			}
			if response["errmsg"] != "simulated API failure" || response["detail"] != "original server detail" || response["trace_id"] != "test-trace" || response["trans_id"] != "test-transaction" {
				t.Error("未完整保留服务端原始错误响应")
			}
			if entry["trans_id"] != "test-transaction" {
				t.Error("未从服务端 JSON 提取 trans_id")
			}
		}
		for _, field := range []string{"error", "endpoint", "http_status", "errcode", "upstream_response", "cause"} {
			if _, exists := entry[field]; exists {
				t.Errorf("错误原因无需额外的 %s 字段", field)
			}
		}
		count++
	}
	if count != len(inputs) {
		t.Fatal("成功、参数错误或上游失败调用缺少日志")
	}
}

func TestMediaPreservesServerError(t *testing.T) {
	for _, tool := range []string{"tuitui_send_private_image", "tuitui_send_private_file"} {
		t.Run(tool, func(t *testing.T) {
			body := `{"detail":{"reason":"test-policy"},"errmsg":"download denied","error":"forbidden","trans_id":"test-transaction"}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/prop/get" {
					jsonResponse(w, map[string]any{})
					return
				}
				if r.URL.Path != "/message/custom/send" {
					t.Error("媒体应直接调用服务端发送接口")
				}
				var payload map[string]any
				json.NewDecoder(r.Body).Decode(&payload)
				if object(payload[text(payload["msgtype"])])["media_id"] != "https://example.com/blocked.png" {
					t.Error("媒体 URL 未原样交给服务端")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, body)
			}))
			defer upstream.Close()
			var logs bytes.Buffer
			server := httptest.NewServer(NewServer(Options{
				Defaults: Credentials{"1", "fixture-secret"}, SDKOptions: &sdk.ClientOptions{APIBaseURL: upstream.URL},
				Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client, err := OpenMCP(ctx, server.URL+"/mcp", nil)
			if err != nil {
				t.Fatal(err)
			}
			var request mcp.CallToolRequest
			request.Params.Name = tool
			request.Params.Arguments = map[string]any{"account": "test-user", "url": "https://example.com/blocked.png"}
			result, err := client.CallTool(ctx, request)
			client.Close()
			server.Close()
			if err != nil || result == nil || !result.IsError || len(result.Content) != 1 {
				t.Fatal("应返回服务端错误")
			}
			content, ok := result.Content[0].(mcp.TextContent)
			if !ok || content.Text != body {
				t.Fatal("工具应返回完整服务端 JSON，而不是只返回 errmsg")
			}
			decoder := json.NewDecoder(&logs)
			found := false
			for {
				var entry map[string]any
				if err := decoder.Decode(&entry); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if entry["msg"] == "tool call" {
					found = true
					if entry["output"] != body || entry["trans_id"] != "test-transaction" {
						t.Fatal("日志未完整保留 JSON 中的 trans_id 和 detail")
					}
					if _, exists := entry["error"]; exists {
						t.Fatal("不应重复记录 error 字段")
					}
				}
			}
			if !found {
				t.Fatal("缺少工具调用日志")
			}

		})
	}
}
