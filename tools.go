package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	sdk "github.com/tuitui-open/bot-sdk-golang"
)

type Tools struct {
	SDK         *sdk.Client
	credentials Credentials
}

func NewTools(c Credentials, options *sdk.ClientOptions) *Tools {
	return &Tools{SDK: sdk.NewClient(c.AppID, c.Secret, options), credentials: c}
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func array(v any) []any           { a, _ := v.([]any); return a }
func text(v any) string           { s, _ := v.(string); return s }
func texts(v any) []string {
	var out []string
	for _, v := range array(v) {
		out = append(out, text(v))
	}
	return out
}

func (t *Tools) Execute(ctx context.Context, name string, a map[string]any) (any, error) {
	s := func(k string) string { return text(a[k]) }
	ss := func(k string) []string { return texts(a[k]) }
	switch name {
	case "tuitui_send_private_message":
		return t.SDK.IM.SendText(ctx, sdk.SendIMTextOptions{To: t.SDK.To.Accounts(ss("accounts")), Text: s("message")})
	case "tuitui_send_group_message":
		return t.SDK.IM.SendText(ctx, sdk.SendIMTextOptions{To: t.SDK.To.Group(s("group_id")), Text: s("message"), At: []string{}})
	case "tuitui_send_private_image":
		return t.SDK.Request(ctx, "/message/custom/send", map[string]any{
			"tousers": []string{s("account")}, "msgtype": "image", "image": map[string]any{"media_id": s("url")},
		}, nil)
	case "tuitui_send_group_image":
		return t.SDK.Request(ctx, "/message/custom/send", map[string]any{
			"togroups": []string{s("group_id")}, "msgtype": "image", "image": map[string]any{"media_id": s("url")},
		}, nil)
	case "tuitui_send_private_file":
		return t.SDK.IM.SendFileByURL(ctx, sdk.SendIMFileByURLOptions{
			To: t.SDK.To.Account(s("account")), URL: s("url"), Filename: s("filename"),
		})
	case "tuitui_send_private_strong_call":
		return t.SDK.IM.SendStrongNotice(ctx, sdk.SendStrongNoticeOptions{Account: s("account"), Content: s("content"), SMSNotice: a["sms_notice"] == true, CallNotice: a["call_notice"] == true})
	case "tuitui_recall_private_message":
		return t.SDK.IM.Recall(ctx, sdk.RecallIMMessageOptions{To: t.SDK.To.Account(s("account")), MessageID: s("msgid")})
	case "tuitui_get_private_message_readstatus":
		return t.SDK.Request(ctx, "/message/single/readstatus", map[string]any{"user": s("account"), "msgid": s("msgid")}, nil)
	case "tuitui_get_private_messages":
		asc := true
		return t.SDK.IM.GetHistory(ctx, t.SDK.To.Account(s("account")), &sdk.GetChatRecordOptions{RelativeTime: s("relative_time"), Cursor: s("cursor"), OrderAsc: &asc})
	case "tuitui_get_group_messages":
		last, _ := a["last"].(int)
		return t.SDK.IM.GetHistory(ctx, t.SDK.To.Group(s("group_id")), &sdk.GetChatRecordOptions{Limit: last})
	case "tuitui_search_account":
		// User.Search discards response metadata; Request preserves trans_id.
		return t.SDK.Request(ctx, "/user/search", map[string]any{"keyword": s("keyword")}, nil)
	case "tuitui_get_user_ids":
		return t.SDK.Request(ctx, "/user/ids", map[string]any{"users": ss("accounts")}, nil)
	case "tuitui_create_group":
		return t.SDK.Request(ctx, "/group/create", map[string]any{"name": s("name"), "owner": s("owner"), "members": ss("members")}, nil)
	case "tuitui_add_group_members":
		return t.SDK.Group.MemberAdd(ctx, s("group_id"), ss("members"))
	case "tuitui_remove_group_members":
		return t.SDK.Group.MemberRemove(ctx, s("group_id"), ss("members"))
	case "tuitui_create_team":
		return t.SDK.Request(ctx, "/teams/team/create", map[string]any{"name": s("name"), "desc": s("desc")}, nil)
	case "tuitui_add_team_members":
		return t.SDK.Request(ctx, "/teams/member/add", map[string]any{"team_id": s("team_id"), "members[]": ss("members")}, nil)
	case "tuitui_create_channel":
		return t.SDK.Request(ctx, "/teams/channel/create", map[string]any{"team_id": s("team_id"), "name": s("name"), "desc": s("desc")}, nil)
	case "tuitui_get_team_channel_list":
		return t.SDK.Request(ctx, "/teams/team/info/rich", map[string]any{"team_id": s("team_id")}, nil)
	case "tuitui_send_channel_markdown_message":
		info, err := t.SDK.Teams.GetChannelInfo(ctx, s("channel_id"))
		if err != nil {
			return nil, err
		}
		return t.SDK.Request(ctx, "/message/custom/send", map[string]any{
			"toteams":  []any{map[string]any{"team_id": info["team_id"], "channel_id": s("channel_id")}},
			"msgtype":  "richtext/markdown",
			"richtext": map[string]any{"markdown": s("markdown"), "delims_left": "{{", "delims_right": "}}"},
		}, nil)
	case "tuitui_get_team_post_list_by_time_interval":
		channel, start, end := s("channel_id"), s("start_time"), s("end_time")
		stop := time.Now().Unix()
		var err error
		if end != "" {
			stop, err = strconv.ParseInt(end, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("end_time: %w", err)
			}
		}
		from := stop - 86400
		if start != "" {
			from, err = strconv.ParseInt(start, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("start_time: %w", err)
			}
		}
		// The tool accepts Unix seconds; Bot API uses milliseconds.
		if from > 9223372036854775 || from < -9223372036854775 || stop > 9223372036854775 || stop < -9223372036854775 {
			return nil, fmt.Errorf("时间戳超出毫秒转换范围")
		}
		info, err := t.SDK.Teams.GetChannelInfo(ctx, channel)
		if err != nil {
			return nil, err
		}
		return t.SDK.Teams.GetPostTopics(ctx, sdk.GetPostTopicsOptions{
			TeamID: text(info["team_id"]), ChannelID: channel, Size: 20,
			SortType: "time", Order: "asc", FromTimestamp: from * 1000, EndTimestamp: stop * 1000,
		})
	}
	return nil, errors.New("未知工具")
}
