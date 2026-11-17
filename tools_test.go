package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/tuitui-open/bot-sdk-golang"
)

func fixtureTools(t *testing.T, handler http.HandlerFunc) *Tools {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return NewTools(Credentials{"1", "fixture-secret"}, &sdk.ClientOptions{APIBaseURL: s.URL})
}
func jsonResponse(w http.ResponseWriter, v map[string]any) {
	v["errcode"] = 0
	json.NewEncoder(w).Encode(v)
}
func TestToolResponsesPassThrough(t *testing.T) {
	for _, response := range []string{
		`{"errcode":0,"errmsg":"ok","trans_id":"test-transaction","extra":{"value":"preserved"}}`,
		`{"errcode":0,"msgids":[],"post_ids":[],"success":[],"explains":[{"tousers":["test-user"],"reason":"denied"}]}`,
		`{"errcode":0,"msgids":[{"user":"different-user","msgid":"opaque-id"}],"post_ids":[{"post_id":"opaque-post"}]}`,
	} {
		c := fixtureTools(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/teams/channel/info":
				jsonResponse(w, map[string]any{"datas": map[string]any{"info": map[string]any{"team_id": "3"}}})
			default:
				w.Write([]byte(response))
			}
		})
		for _, def := range Definitions() {
			name := def.Name
			t.Run(name, func(t *testing.T) {
				args := map[string]any{
					"accounts": []any{"test-user"}, "account": "test-user", "message": "test",
					"channel_id": "2", "msgid": "4", "markdown": "test", "content": "test",
					"team_id": "3", "name": "test", "owner": "test-user", "members": []any{"test-user"}, "keyword": "test",
					"url": "https://example.com/file.txt", "filename": "test.txt",
				}
				if strings.Contains(name, "group") {
					args["group_id"] = "5"
				}
				v, err := c.Execute(context.Background(), name, args)
				if err != nil {
					t.Fatal(err)
				}
				got, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				var want any
				json.Unmarshal([]byte(response), &want)
				expected, _ := json.Marshal(want)
				if string(got) != string(expected) {
					t.Fatalf("服务端响应被改变: %s", got)
				}
			})
		}
	}
}
func TestUnexpectedUpstreamResponsesDoNotPanic(t *testing.T) {
	for _, response := range []string{
		``, `null`, `[]`, `"invalid"`, `<html>bad gateway</html>`, `{`,
		`{"errcode":null}`, `{"errcode":{}}`, `{"errcode":1,"errmsg":[]}`,
		`{"errcode":0,"datas":null}`,
		`{"errcode":0,"datas":{"info":[]}}`,
		`{"errcode":0,"datas":{"info":{"team_id":{}}},"msgs":[null],"msgids":true}`,
	} {
		t.Run(response, func(t *testing.T) {
			tools := fixtureTools(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, response)
			})
			for _, def := range Definitions() {
				t.Run(def.Name, func(t *testing.T) {
					args := map[string]any{
						"accounts": []any{"test-user"}, "account": "test-user", "message": "test",
						"channel_id": "2", "msgid": "4", "markdown": "test", "content": "test",
						"team_id": "3", "group_id": "5", "name": "test", "owner": "test-user",
						"members": []any{"test-user"}, "keyword": "test", "relative_time": "today",
						"url": "https://example.com/file.txt", "filename": "test.txt",
					}
					value, err := tools.Execute(context.Background(), def.Name, args)
					if err != nil {
						if errorReason(err) == "" {
							t.Fatal("missing error reason")
						}
					} else if _, err := json.Marshal(value); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func TestHistoryRequestParameters(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		want map[string]any
	}{
		{"tuitui_get_private_messages", map[string]any{"account": "test-user", "relative_time": "today", "cursor": "test-cursor"}, map[string]any{"user": "test-user", "relative_time": "today", "cursor": "test-cursor", "order_asc": true}},
		{"tuitui_get_group_messages", map[string]any{"group_id": "opaque-group", "last": -1}, map[string]any{"group_id": "opaque-group", "limit": float64(-1)}},
		{"tuitui_get_team_post_list_by_time_interval", map[string]any{"channel_id": "opaque-channel", "start_time": "200", "end_time": "100"}, map[string]any{"channel_id": "opaque-channel", "from_timestamp": float64(200000), "end_timestamp": float64(100000), "order": "asc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := fixtureTools(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/teams/channel/info" {
					jsonResponse(w, map[string]any{"datas": map[string]any{"info": map[string]any{"team_id": "opaque-team"}}})
					return
				}
				calls++
				var payload map[string]any
				json.NewDecoder(r.Body).Decode(&payload)
				for key, want := range tc.want {
					if payload[key] != want {
						t.Errorf("%s = %v, want %v", key, payload[key], want)
					}
				}
				jsonResponse(w, map[string]any{"has_more": true, "cursor": "unchanged", "msgs": []any{"raw"}, "trans_id": "test-history"})
			})
			ctx := context.Background()
			result, err := c.Execute(ctx, tc.name, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			response := result.(sdk.APIResponse)
			if calls != 1 || response["has_more"] != true || response["cursor"] != "unchanged" || response["trans_id"] != "test-history" {
				t.Fatal("应透传单次响应，保留分页及追踪信息")
			}
		})
	}
}
func formatInt(n int64) string { return strconv.FormatInt(n, 10) }
func TestCancellationAndNoWriteRetry(t *testing.T) {
	var calls atomic.Int32
	c := fixtureTools(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		w.Write([]byte(`{"errcode":503}`))
	})
	_, err := c.Execute(context.Background(), "tuitui_send_private_message", map[string]any{"accounts": []any{"test-user"}, "message": "hello"})
	if err == nil || calls.Load() != 1 {
		t.Fatal("写操作被重试")
	}
	c = fixtureTools(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = c.SDK.Property.Info(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("取消信号未传播")
	}
}

func TestGroupPartialFailuresAndMarkdown(t *testing.T) {
	calls := 0
	c := fixtureTools(t, func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		switch r.URL.Path {
		case "/group/create":
			if p["owner"] != "test-user" || len(array(p["members"])) != 1 {
				t.Error("单成员建群及 owner 默认规则错误")
			}
			jsonResponse(w, map[string]any{"group_id": "2", "fails": []any{"invalid"}, "explains": []any{map[string]any{"reason": "denied"}}})
		case "/group/member/add", "/group/member/remove":
			jsonResponse(w, map[string]any{"fails": []any{"invalid"}, "explains": []any{map[string]any{"reason": "denied"}}})
		case "/teams/channel/info":
			jsonResponse(w, map[string]any{"datas": map[string]any{"info": map[string]any{"team_id": "3"}}})
		case "/message/custom/send":
			calls++
			rich := object(p["richtext"])
			if rich["markdown"] != "@test-user {{literal}}" || rich["delims_left"] != "{{" || rich["delims_right"] != "}}" {
				t.Error("Markdown 原文及模板语义被改变")
			}
			jsonResponse(w, map[string]any{"post_ids": []any{map[string]any{"post_id": "4"}}})
		}
	})
	for _, name := range []string{"tuitui_create_group", "tuitui_add_group_members", "tuitui_remove_group_members"} {
		v, err := c.Execute(context.Background(), name, map[string]any{"name": "test", "owner": "test-user", "members": []any{"test-user"}, "group_id": "2"})
		if err != nil || len(array(v.(sdk.APIResponse)["fails"])) != 1 {
			t.Fatal("群操作部分失败信息丢失")
		}
	}
	if _, err := c.Execute(context.Background(), "tuitui_send_channel_markdown_message", map[string]any{"channel_id": "2", "markdown": "@test-user {{literal}}"}); err != nil || calls != 1 {
		t.Fatal("帖子未保持单次原始发送")
	}
}

func TestMediaURLsAreHandledByServer(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("MCP 不应下载媒体 URL")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer source.Close()
	for _, tc := range []struct{ tool, msgType, filename string }{
		{"tuitui_send_private_file", "attachment", "../original.png"},
		{"tuitui_send_private_file", "attachment", ""},
		{"tuitui_send_private_image", "image", ""},
		{"tuitui_send_group_image", "image", ""},
	} {
		t.Run(tc.tool+"/"+tc.filename, func(t *testing.T) {
			calls := 0
			c := fixtureTools(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/message/custom/send" {
					t.Errorf("不应自行上传媒体: %s", r.URL.Path)
				}
				var payload map[string]any
				json.NewDecoder(r.Body).Decode(&payload)
				if payload["msgtype"] != tc.msgType {
					t.Error("发送类型错误")
				}
				media := object(payload[tc.msgType])
				if media["media_id"] != source.URL+"/original.png" {
					t.Error("媒体 URL 应原样交给服务端")
				}
				if tc.filename != "" && media["filename"] != tc.filename {
					t.Error("文件名应原样交给服务端处理")
				}
				if tc.filename == "" {
					if _, ok := media["filename"]; ok {
						t.Error("未指定文件名时不应生成默认文件名")
					}
				}
				jsonResponse(w, map[string]any{"trans_id": "media-trans"})
			})
			args := map[string]any{"account": "test-user", "url": source.URL + "/original.png", "filename": tc.filename}
			if tc.tool == "tuitui_send_group_image" {
				args["group_id"] = "2"
			}
			ctx := context.Background()
			result, err := c.Execute(ctx, tc.tool, args)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || result.(sdk.APIResponse)["trans_id"] != "media-trans" {
				t.Fatal("应只调用一次服务端并透传 trans_id")
			}
		})
	}
}
