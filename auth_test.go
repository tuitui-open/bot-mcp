package main

import (
	"net/http/httptest"
	"testing"
)

func TestResolve(t *testing.T) {
	defaults := Credentials{"1", "default"}
	for _, tc := range []struct {
		name, query, header string
		want                Credentials
		fail                bool
	}{
		{name: "defaults", want: defaults}, {name: "url", query: "?appid=2&secret=url", header: "Bearer 3-header", want: Credentials{"2", "url"}},
		{name: "bearer", header: "bearer 3-abc-def", want: Credentials{"3", "abc-def"}},
		{name: "partial", query: "?appid=2", header: "Bearer 3-header", fail: true},
		{name: "empty", query: "?appid=2&secret=", fail: true}, {name: "duplicate", query: "?appid=2&appid=3&secret=a", fail: true},
		{name: "invalid header", header: "Bearer invalid", fail: true}, {name: "empty app", header: "Bearer -secret", fail: true},
		{name: "empty secret", header: "Bearer 2-", fail: true},
		{name: "malformed query", query: "?appid=%zz", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://localhost/mcp"+tc.query, nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			got, err := defaults.Resolve(r)
			if (err != nil) != tc.fail || !tc.fail && got != tc.want {
				t.Fatal("凭证来源选择错误")
			}
		})
	}
	if _, err := (Credentials{}).Resolve(httptest.NewRequest("POST", "http://localhost/mcp?access_token=token", nil)); err == nil {
		t.Fatal("不应接受 access_token")
	}
	if defaults != (Credentials{"1", "default"}) {
		t.Fatal("默认值被修改")
	}
}
