package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type Credentials struct{ AppID, Secret string }

func (c Credentials) valid() bool {
	id, err := strconv.ParseUint(c.AppID, 10, 64)
	return err == nil && id > 0 && strings.TrimSpace(c.Secret) != ""
}

func (c Credentials) Resolve(r *http.Request) (Credentials, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return Credentials{}, errors.New("URL 查询参数格式错误")
	}
	var resolved Credentials
	if q.Has("appid") || q.Has("secret") {
		if len(q["appid"]) != 1 || len(q["secret"]) != 1 {
			return Credentials{}, errors.New("URL 必须提供完整且唯一的 appid/secret")
		}
		resolved = Credentials{q.Get("appid"), q.Get("secret")}
	} else if _, ok := r.Header["Authorization"]; ok {
		h := r.Header.Values("Authorization")
		if len(h) != 1 {
			return Credentials{}, errors.New("Authorization 格式错误")
		}
		fields := strings.Fields(h[0])
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
			return Credentials{}, errors.New("Authorization 格式错误")
		}
		var found bool
		resolved.AppID, resolved.Secret, found = strings.Cut(fields[1], "-")
		if !found {
			err = errors.New("Bearer 格式错误")
		}
	} else {
		resolved = c
	}
	if err != nil || !resolved.valid() {
		return Credentials{}, fmt.Errorf("请提供有效的 appid/secret 或 Bearer 凭证")
	}
	return resolved, nil
}
