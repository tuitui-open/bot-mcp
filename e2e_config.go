package main

import (
	"bufio"
	"errors"
	"os"
	"strings"
)

// loadTestValues reads secrets internally; errors never contain input values.
func loadTestValues(path string) (map[string]string, error) {
	values := map[string]string{}
	f, err := os.Open(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("无法加载本地测试配置")
	}
	if err == nil {
		defer f.Close()
		s := bufio.NewScanner(f)
		for s.Scan() {
			line := strings.TrimSpace(strings.TrimPrefix(s.Text(), "\ufeff"))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			key, value, ok := strings.Cut(line, "=")
			key = strings.TrimSpace(key)
			if !ok {
				return nil, errors.New("本地测试配置格式错误")
			}
			switch key {
			case "TUITUI_APP_ID", "TUITUI_APP_SECRET", "TUITUI_E2E_ACCOUNT", "TUITUI_E2E_IMAGE_URL", "TUITUI_E2E_FILE_URL", "TUITUI_E2E_INVALID_IMAGE_URL", "TUITUI_E2E_INVALID_FILE_URL":
			default:
				continue
			}
			value = strings.TrimSpace(value)
			if len(value) >= 2 && (value[0] == '\'' && value[len(value)-1] == '\'' || value[0] == '"' && value[len(value)-1] == '"') {
				value = value[1 : len(value)-1]
			}
			values[key] = value
		}
		if s.Err() != nil {
			return nil, errors.New("无法加载本地测试配置")
		}
	}
	return values, nil
}

func LoadE2EConfig(path string) (Credentials, E2EOptions, error) {
	values, err := loadTestValues(path)
	if err != nil {
		return Credentials{}, E2EOptions{}, err
	}
	options := E2EOptions{
		InvalidFileURL:  values["TUITUI_E2E_INVALID_FILE_URL"],
		InvalidImageURL: values["TUITUI_E2E_INVALID_IMAGE_URL"],
		Account:         values["TUITUI_E2E_ACCOUNT"],
		ImageURL:        values["TUITUI_E2E_IMAGE_URL"],
		FileURL:         values["TUITUI_E2E_FILE_URL"],
	}
	c := Credentials{values["TUITUI_APP_ID"], values["TUITUI_APP_SECRET"]}
	if c.AppID == "" && c.Secret == "" {
		return c, options, nil
	}
	if !c.valid() {
		return Credentials{}, E2EOptions{}, errors.New("默认机器人凭证不完整或无效")
	}
	return c, options, nil
}
