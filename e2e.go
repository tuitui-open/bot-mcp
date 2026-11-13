package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

type E2EOptions struct{ Account, ImageURL, FileURL, InvalidImageURL, InvalidFileURL string }
type E2EResult struct {
	Tool   string `json:"tool"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}
type E2EReport struct {
	Run       string            `json:"run"`
	Protocol  string            `json:"protocol"`
	Results   []E2EResult       `json:"results"`
	Resources map[string]string `json:"resources"`
	Retained  []string          `json:"retained_resources"`
}
type e2eSuite struct {
	ctx     context.Context
	session *client.Client
	tools   *Tools
	report  E2EReport
	results map[string]E2EResult
}

func (s *e2eSuite) step(name string, fn func() (any, error)) (value any, success bool) {
	defer func() {
		if recover() != nil {
			s.results[name] = E2EResult{name, "failed", "测试或上游响应结构异常"}
			fmt.Println(name + ": failed")
		}
	}()
	value, err := fn()
	status := "passed"
	detail := ""
	if err != nil {
		status = "failed"
		detail = errorReason(err)
	}
	if strings.HasPrefix(detail, "前置条件：") {
		status = "blocked"
	}
	s.results[name] = E2EResult{name, status, detail}
	fmt.Println(name + ": " + status)
	return value, err == nil
}
func (s *e2eSuite) call(name string, a map[string]any) (any, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 90*time.Second)
	defer cancel()
	return CallMCP(ctx, s.session, name, a)
}
func requireID(v any, key string) (string, error) {
	m := object(v)
	if m == nil {
		return "", errors.New("返回结构错误")
	}
	return ID(m[key])
}

// ID validates test assertions only; tool responses are passed through unchanged.
func ID(v any) (string, error) {
	switch v := v.(type) {
	case string:
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			return v, nil
		}
	case float64:
		if v > 0 && v <= 9007199254740991 && math.Trunc(v) == v {
			return strconv.FormatFloat(v, 'f', 0, 64), nil
		}
	}
	return "", errors.New("测试响应中的 ID 无效或不安全")
}
func requireMessageID(v any, account string) (string, error) {
	for _, item := range array(object(v)["msgids"]) {
		m := object(item)
		if strings.EqualFold(text(m["user"]), account) {
			return ID(m["msgid"])
		}
	}
	return "", errors.New("测试响应未包含目标账号的消息 ID")
}
func contains(v any, marker string) bool {
	b, _ := json.Marshal(v)
	return strings.Contains(string(b), marker)
}

func expectServerError(err error) error {
	if err == nil {
		return errors.New("此用例应由服务端拒绝")
	}
	var response struct {
		Errcode *int   `json:"errcode"`
		Errmsg  string `json:"errmsg"`
		TransID string `json:"trans_id"`
	}
	if json.Unmarshal([]byte(err.Error()), &response) != nil || response.Errcode == nil || *response.Errcode == 0 || response.Errmsg == "" || response.TransID == "" {
		return errors.New("预期完整服务端错误 JSON，包含非零 errcode、errmsg 和 trans_id")
	}
	return nil
}

func testInvalidSecret(ctx context.Context, address, account, marker string) error {
	// 错误凭证测试使用固定假 appid 和 secret。
	target := address + "/mcp"
	c, err := OpenMCP(ctx, target, map[string]string{"Authorization": "Bearer 123456789-abc"})
	if err != nil {
		return fmt.Errorf("错误 secret 不应影响初始化: %w", err)
	}
	defer c.Close()
	list, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return fmt.Errorf("错误 secret 不应影响工具发现: %w", err)
	}
	if len(list.Tools) != len(Definitions()) {
		return errors.New("错误 secret 下工具发现不完整")
	}
	_, err = CallMCP(ctx, c, "tuitui_send_private_message", map[string]any{"accounts": []any{account}, "message": marker + " E2E invalid secret"})
	return expectServerError(err)
}
func (s *e2eSuite) finish(output io.Writer) error {
	passed := 0
	for _, def := range Definitions() {
		result, ok := s.results[def.Name]
		if !ok {
			result = E2EResult{def.Name, "blocked", "前置条件未满足，未执行成功路径"}
		}
		s.report.Results = append(s.report.Results, result)
		if result.Status == "passed" {
			passed++
		}
	}
	sort.Slice(s.report.Results, func(i, j int) bool { return s.report.Results[i].Tool < s.report.Results[j].Tool })
	fmt.Fprintf(output, "\nE2E %s\n", s.report.Run)
	for _, result := range s.report.Results {
		fmt.Fprintf(output, "%s: %s", result.Tool, result.Status)
		if result.Detail != "" {
			fmt.Fprintf(output, " — %s", result.Detail)
		}
		fmt.Fprintln(output)
	}
	keys := make([]string, 0, len(s.report.Resources))
	for key := range s.report.Resources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(output, "%s: %s\n", key, s.report.Resources[key])
	}
	for _, resource := range s.report.Retained {
		fmt.Fprintf(output, "保留资源: %s\n", resource)
	}
	total := len(Definitions())
	fmt.Fprintf(output, "E2E: %d/%d passed\n", passed, total)
	if passed != total || s.report.Protocol != "passed" {
		return errors.New("完整 E2E 尚未全部通过")
	}
	return nil
}

func RunE2E(defaults Credentials, options E2EOptions) error {
	if options.Account == "" {
		return errors.New("请在 .env.e2e 中配置 TUITUI_E2E_ACCOUNT")
	}
	if options.ImageURL == "" {
		return errors.New("请在 .env.e2e 中配置 TUITUI_E2E_IMAGE_URL")
	}
	if options.FileURL == "" {
		return errors.New("请在 .env.e2e 中配置 TUITUI_E2E_FILE_URL")
	}
	if options.InvalidImageURL == "" {
		return errors.New("请在 .env.e2e 中配置 TUITUI_E2E_INVALID_IMAGE_URL")
	}
	if options.InvalidFileURL == "" {
		return errors.New("请在 .env.e2e 中配置 TUITUI_E2E_INVALID_FILE_URL")
	}
	account := options.Account
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	s := &e2eSuite{ctx: ctx, tools: NewTools(defaults, nil), results: map[string]E2EResult{}, report: E2EReport{Run: "mcp" + time.Now().Format("0102150405"), Resources: map[string]string{}, Protocol: "failed"}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return errors.New("测试服务监听失败")
	}
	serverCtx, stop := context.WithCancel(ctx)
	defer stop()
	serverDone := make(chan error, 1)
	go func() { serverDone <- Serve(serverCtx, listener, NewServer(Options{Defaults: defaults})) }()
	defer func() { stop(); <-serverDone }()
	address := "http://" + listener.Addr().String()
	var session *client.Client
	for _, mode := range []string{"default", "bearer"} {
		target := address + "/mcp"
		headers := map[string]string{}
		if mode == "bearer" {
			headers["Authorization"] = "Bearer " + defaults.AppID + "-" + defaults.Secret
		}
		testCtx, finish := context.WithTimeout(ctx, 45*time.Second)
		c, err := OpenMCP(testCtx, target, headers)
		if err == nil {
			var list *mcp.ListToolsResult
			list, err = c.ListTools(testCtx, mcp.ListToolsRequest{})
			if err == nil && len(list.Tools) != 21 {
				err = errors.New("工具数量不是 21")
			}
			if err == nil {
				_, err = CallMCP(testCtx, c, "tuitui_search_account", map[string]any{"keyword": account})
			}
		}
		finish()
		if err != nil {
			if c != nil {
				c.Close()
			}
			fmt.Printf("protocol: failed (%s) — %s\n", mode, errorReason(err))
			return s.finish(os.Stdout)
		}
		c.Close()
	}
	s.report.Protocol = "passed"
	fmt.Println("protocol: passed (/mcp, default credentials and Bearer)")
	session, err = OpenMCP(ctx, address+"/mcp", map[string]string{"Authorization": "Bearer " + defaults.AppID + "-" + defaults.Secret})
	if err != nil {
		fmt.Printf("protocol: failed (session) — %s\n", errorReason(err))
		return s.finish(os.Stdout)
	}
	defer session.Close()
	s.session = session
	marker := s.report.Run
	uids := map[string]string{}
	s.step("tuitui_search_account", func() (any, error) {
		v, err := s.call("tuitui_search_account", map[string]any{"keyword": account})
		if err != nil {
			return nil, err
		}
		for _, user := range array(object(v)["users"]) {
			m := object(user)
			if text(m["account"]) == account {
				if _, err := ID(m["user_id"]); err != nil {
					return nil, err
				}
				if _, ok := m["name"].(string); !ok {
					return nil, errors.New("缺少用户姓名")
				}
				return v, nil
			}
		}
		return nil, fmt.Errorf("未找到测试账号 %s", account)
	})
	s.step("tuitui_get_user_ids", func() (any, error) {
		accounts := []any{account}
		v, err := s.call("tuitui_get_user_ids", map[string]any{"accounts": accounts})
		if err != nil {
			return nil, err
		}
		for _, item := range array(object(v)["users"]) {
			m := object(item)
			id, err := ID(m["id"])
			if err != nil {
				return nil, err
			}
			uids[text(m["user"])] = id
		}
		if uids[account] == "" {
			return nil, errors.New("缺少目标用户 ID")
		}
		return v, nil
	})
	messageID := ""
	s.step("tuitui_send_private_message", func() (any, error) {
		v, err := s.call("tuitui_send_private_message", map[string]any{"accounts": []any{account}, "message": marker + " E2E 文本"})
		if err != nil {
			return nil, err
		}
		messageID, err = requireMessageID(v, account)
		s.report.Resources["private_message_id"] = messageID
		return v, err
	})
	s.step("tuitui_send_private_strong_call", func() (any, error) {
		return s.call("tuitui_send_private_strong_call", map[string]any{"account": account, "content": marker + " E2E 强通知", "sms_notice": false, "call_notice": false})
	})
	s.step("tuitui_send_private_image", func() (any, error) {
		v, err := s.call("tuitui_send_private_image", map[string]any{"account": account, "url": options.ImageURL})
		if err != nil {
			return nil, err
		}
		id, err := requireMessageID(v, account)
		s.report.Resources["private_image_id"] = id
		if err != nil {
			return nil, err
		}
		_, err = s.call("tuitui_send_private_image", map[string]any{"account": account, "url": options.InvalidImageURL})
		if err := expectServerError(err); err != nil {
			return nil, err
		}
		fmt.Println("tuitui_send_private_image (expected failure): passed")
		return v, nil
	})
	s.step("tuitui_send_private_file", func() (any, error) {
		var last any
		for _, tc := range []struct{ url, name string }{{options.FileURL, marker + ".txt"}, {options.ImageURL, marker + ".png"}, {options.FileURL, ""}} {
			args := map[string]any{"account": account, "url": tc.url}
			if tc.name != "" {
				args["filename"] = tc.name
			}
			v, err := s.call("tuitui_send_private_file", args)
			if err != nil {
				return nil, err
			}
			if _, err = requireMessageID(v, account); err != nil {
				return nil, err
			}
			last = v
		}
		_, err := s.call("tuitui_send_private_file", map[string]any{"account": account, "url": options.InvalidFileURL})
		if err := expectServerError(err); err != nil {
			return nil, err
		}
		if !strings.Contains(err.Error(), "SSRF") {
			return nil, errors.New("内网文件 URL 应由服务端 SSRF 防护拒绝")
		}
		fmt.Println("tuitui_send_private_file (SSRF, expected failure): passed")
		return last, nil
	})
	s.step("tuitui_get_private_messages", func() (any, error) {
		if messageID == "" {
			return nil, errors.New("前置条件：文本发送未成功")
		}
		var v any
		var messages []any
		cursor := "0"
		seen := map[string]bool{cursor: true}
		for {
			var err error
			v, err = s.call("tuitui_get_private_messages", map[string]any{"account": account, "relative_time": "last_1_hours", "cursor": cursor})
			if err != nil {
				return nil, err
			}
			messages = append(messages, array(object(v)["msgs"])...)
			if object(v)["has_more"] != true {
				break
			}
			cursor = text(object(v)["cursor"])
			if cursor == "" || seen[cursor] || len(seen) >= 100 {
				return nil, errors.New("测试历史分页游标未前进或超出页数限制")
			}
			seen[cursor] = true
		}
		if !contains(messages, marker) {
			return nil, errors.New("历史未包含本次消息")
		}
		for resource, kind := range map[string]string{"private_image_id": "image"} {
			if id := s.report.Resources[resource]; id != "" {
				found := false
				for _, item := range messages {
					d := object(object(item)["data"])
					if text(d["msgid"]) == id && text(d["msg_type"]) == kind {
						found = true
					}
				}
				if !found {
					return nil, errors.New("历史未确认本次图片类型")
				}
			}
		}
		if s.results["tuitui_send_private_file"].Status == "passed" && (!contains(messages, marker+".txt") || !contains(messages, marker+".png")) {
			return nil, errors.New("历史未确认附件文件名")
		}
		return v, nil
	})
	s.step("tuitui_get_private_message_readstatus", func() (any, error) {
		if messageID == "" {
			return nil, errors.New("前置条件：缺少本次消息 ID")
		}
		v, err := s.call("tuitui_get_private_message_readstatus", map[string]any{"account": account, "msgid": messageID})
		if err != nil {
			return nil, err
		}
		if _, ok := object(v)["read"].(bool); !ok {
			return nil, errors.New("已读状态不是布尔值")
		}
		return v, nil
	})
	s.step("tuitui_recall_private_message", func() (any, error) {
		if messageID == "" {
			return nil, errors.New("前置条件：缺少本次消息 ID")
		}
		v, err := s.call("tuitui_recall_private_message", map[string]any{"account": account, "msgid": messageID})
		if err != nil {
			return nil, err
		}
		for _, item := range array(object(v)["success"]) {
			m := object(item)
			if strings.EqualFold(text(m["user"]), account) && text(m["msgid"]) == messageID {
				return v, nil
			}
		}
		return nil, errors.New("撤回结果不匹配")
	})
	group := ""
	owner := account
	member := ""
	info, err := s.tools.SDK.Property.Info(ctx)
	if err == nil {
		member = info.Account
	}
	s.step("tuitui_create_group", func() (any, error) {
		if member == "" {
			return nil, errors.New("前置条件：缺少机器人账号")
		}
		v, err := s.call("tuitui_create_group", map[string]any{"name": marker, "owner": owner, "members": []any{owner, member}})
		if err != nil {
			return nil, err
		}
		group, err = requireID(v, "group_id")
		if err != nil {
			return nil, err
		}
		s.report.Resources["group_id"] = group
		s.report.Retained = append(s.report.Retained, "group:"+group)
		_, err = s.tools.SDK.Group.Info(ctx, group)
		return v, err
	})
	groupCall := func(name string, a map[string]any) (any, error) {
		if group == "" {
			return nil, errors.New("前置条件：测试群创建失败")
		}
		a["group_id"] = group
		return s.call(name, a)
	}
	s.step("tuitui_send_group_message", func() (any, error) {
		return groupCall("tuitui_send_group_message", map[string]any{"message": marker + " E2E 群消息"})
	})
	s.step("tuitui_send_group_image", func() (any, error) {
		return groupCall("tuitui_send_group_image", map[string]any{"url": options.ImageURL})
	})
	s.step("tuitui_get_group_messages", func() (any, error) {
		v, err := groupCall("tuitui_get_group_messages", map[string]any{"last": 100})
		if err != nil {
			return nil, err
		}
		if !contains(v, marker) || !contains(v, "image") {
			return nil, errors.New("群历史未包含本次文本或图片")
		}
		return v, nil
	})
	s.step("tuitui_add_group_members", func() (any, error) {
		// 仅有群主和机器人两个账号，验证重复添加已有群主的幂等行为。
		v, err := groupCall("tuitui_add_group_members", map[string]any{"members": []any{owner}})
		if err != nil {
			return nil, err
		}
		members, err := s.tools.SDK.Group.Members(ctx, group)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			if strings.EqualFold(m.Account, owner) {
				fmt.Println("tuitui_add_group_members (existing member): passed")
				return v, nil
			}
		}
		return nil, errors.New("重复添加后未找到原有群主")
	})
	s.step("tuitui_remove_group_members", func() (any, error) {
		// 最后移除机器人；退群后不能再调用群信息或成员查询接口。
		v, err := groupCall("tuitui_remove_group_members", map[string]any{"members": []any{member}})
		if err != nil {
			return nil, err
		}
		groups, err := s.tools.SDK.Group.RobotIn(ctx)
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			if g.GroupID == group {
				return nil, errors.New("移除后机器人仍在测试群中")
			}
		}
		return v, nil
	})
	team := ""
	channel := ""
	post := ""
	s.step("tuitui_create_team", func() (any, error) {
		v, err := s.call("tuitui_create_team", map[string]any{"name": marker, "desc": "bot-mcp E2E"})
		if err != nil {
			return nil, err
		}
		team, err = requireID(object(v)["datas"], "team_id")
		if err != nil {
			return nil, err
		}
		if _, err = requireID(object(v)["datas"], "default_channel_id"); err != nil {
			return nil, err
		}
		s.report.Resources["team_id"] = team
		s.report.Retained = append(s.report.Retained, "team:"+team)
		return v, nil
	})
	s.step("tuitui_add_team_members", func() (any, error) {
		if team == "" || uids[account] == "" {
			return nil, errors.New("前置条件：缺少团队或用户 ID")
		}
		v, err := s.call("tuitui_add_team_members", map[string]any{"team_id": team, "members": []any{uids[account]}})
		if err != nil {
			return nil, err
		}
		members, err := s.tools.SDK.Teams.GetMembers(ctx, team)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			if contains(m, uids[account]) {
				return v, nil
			}
		}
		return nil, errors.New("团队成员未确认")
	})
	s.step("tuitui_create_channel", func() (any, error) {
		if team == "" {
			return nil, errors.New("前置条件：测试团队创建失败")
		}
		v, err := s.call("tuitui_create_channel", map[string]any{"team_id": team, "name": marker, "desc": "E2E"})
		if err != nil {
			return nil, err
		}
		channel, err = requireID(object(v)["datas"], "channel_id")
		if err != nil {
			return nil, err
		}
		s.report.Resources["channel_id"] = channel
		s.report.Retained = append(s.report.Retained, "channel:"+channel)
		info, err := s.tools.SDK.Teams.GetChannelInfo(ctx, channel)
		if err != nil {
			return nil, err
		}
		id, err := ID(info["team_id"])
		if err != nil || id != team {
			return nil, errors.New("频道所属团队不正确")
		}
		return v, nil
	})
	s.step("tuitui_get_team_channel_list", func() (any, error) {
		if channel == "" {
			return nil, errors.New("前置条件：测试频道创建失败")
		}
		v, err := s.call("tuitui_get_team_channel_list", map[string]any{"team_id": team})
		if err != nil {
			return nil, err
		}
		if !contains(v, channel) || !contains(v, marker) {
			return nil, errors.New("频道列表未包含本次频道")
		}
		return v, nil
	})
	s.step("tuitui_send_channel_markdown_message", func() (any, error) {
		if channel == "" {
			return nil, errors.New("前置条件：测试频道创建失败")
		}
		v, err := s.call("tuitui_send_channel_markdown_message", map[string]any{"channel_id": channel, "markdown": "**" + marker + " E2E**"})
		if err != nil {
			return nil, err
		}
		posts := array(object(v)["post_ids"])
		if len(posts) == 0 {
			return nil, errors.New("缺少帖子 ID 列表")
		}
		post, err = ID(object(posts[0])["post_id"])
		s.report.Resources["post_id"] = post
		return v, err
	})
	s.step("tuitui_get_team_post_list_by_time_interval", func() (any, error) {
		if post == "" {
			return nil, errors.New("前置条件：测试帖子发送失败")
		}
		v, err := s.call("tuitui_get_team_post_list_by_time_interval", map[string]any{"channel_id": channel, "start_time": strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10), "end_time": strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)})
		if err != nil {
			return nil, err
		}
		if !contains(v, post) || !contains(v, marker) {
			return nil, errors.New("帖子查询未包含本次帖子")
		}
		return v, nil
	})
	testCtx, finish := context.WithTimeout(ctx, 45*time.Second)
	err = testInvalidSecret(testCtx, address, account, s.report.Run)
	finish()
	if err != nil {
		s.report.Protocol = "failed"
		fmt.Printf("protocol: failed (invalid secret) — %s\n", errorReason(err))
	} else {
		fmt.Println("invalid credentials (appid=123456789, secret=abc): initialize/list passed; send_private_message rejected as expected")
	}
	return s.finish(os.Stdout)
}
