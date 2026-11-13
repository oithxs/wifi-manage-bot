// 誰に証明書を発行してよいか (/wifi-cert の認可ゲート) を判別するための
// Authentik API クライアント。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

type apiError struct {
	status int
	body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("Authentik API error: %s: %s", http.StatusText(e.status), e.body)
}

type userInfoEntry struct {
	name      string
	discordID string
}

type authentikClient struct {
	baseURL     string
	token       string
	sourceSlug  string
	tokenPrefix string
}

func (c *authentikClient) get(path string, params map[string]string) (map[string]any, error) {
	if len(params) > 0 {
		query := url.Values{}
		for key, value := range params {
			query.Set(key, value)
		}
		path += "?" + query.Encode()
	}
	request, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, &apiError{
			status: response.StatusCode,
			body:   strings.TrimSpace(string(body)),
		}
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func (c *authentikClient) list(path string, params map[string]string) ([]map[string]any, error) {
	// Authentik API のページングされた一覧を全部取る
	var results []map[string]any
	for page := 1; ; page++ {
		p := map[string]string{"page": strconv.Itoa(page), "page_size": "100"}
		for key, value := range params {
			p[key] = value
		}
		data, err := c.get(path, p)
		if err != nil {
			return nil, err
		}
		raw, _ := data["results"].([]any)
		for _, item := range raw {
			if entry, ok := item.(map[string]any); ok {
				results = append(results, entry)
			}
		}
		pagination, _ := data["pagination"].(map[string]any)
		if _, ok := pagination["next"]; !ok {
			return results, nil
		}
	}
}

func (c *authentikClient) userInfo(usernames []string) (map[string]userInfoEntry, error) {
	// Authentik username → {"name": 氏名, "discord_id": 連携していれば Discord ID}
	//
	// 連携していない部員は discord_id が空文字になる。Authentik が落ちたときはエラーを返す
	// (呼び出し側が username だけの表示にフォールバックする)
	want := make(map[string]bool, len(usernames))
	for _, username := range usernames {
		want[username] = true
	}
	pkToUsername := make(map[string]string)
	info := make(map[string]userInfoEntry)
	users, err := c.list("/api/v3/core/users/", map[string]string{"include_groups": "false"})
	if err != nil {
		return nil, err
	}
	for _, user := range users {
		pk := fmt.Sprint(user["pk"])
		username, _ := user["username"].(string)
		pkToUsername[pk] = username
		entry := userInfoEntry{name: username}
		if name, ok := user["name"].(string); ok && name != "" {
			entry.name = name
		}
		info[username] = entry
	}
	connections, err := c.list(
		"/api/v3/sources/user_connections/oauth/",
		map[string]string{"source__slug": c.sourceSlug},
	)
	if err != nil {
		return nil, err
	}
	for _, connection := range connections {
		username, ok := pkToUsername[fmt.Sprint(connection["user"])]
		if !ok || !want[username] {
			continue
		}
		entry := info[username]
		if identifier, ok := connection["identifier"].(string); ok {
			entry.discordID = identifier
		}
		info[username] = entry
	}
	filtered := make(map[string]userInfoEntry, len(usernames))
	for username, entry := range info {
		if want[username] {
			filtered[username] = entry
		}
	}
	return filtered, nil
}

func (c *authentikClient) usernameForDiscord(discordID string) (string, error) {
	// Discord ID (連携済み) から Authentik の username を返す。連携がなければ空文字
	// search は部分一致のため、identifier との完全一致で絞る
	connections, err := c.get(
		"/api/v3/sources/user_connections/oauth/",
		map[string]string{"source__slug": c.sourceSlug, "search": discordID},
	)
	if err != nil {
		return "", err
	}
	results, _ := connections["results"].([]any)
	for _, item := range results {
		connection, ok := item.(map[string]any)
		if !ok || fmt.Sprint(connection["identifier"]) != discordID {
			continue
		}
		user, err := c.get(fmt.Sprintf("/api/v3/core/users/%s/", fmt.Sprint(connection["user"])), nil)
		if err != nil {
			return "", err
		}
		username, _ := user["username"].(string)
		return username, nil
	}
	return "", nil
}

func (c *authentikClient) wifiPassword(username string) (string, error) {
	// 部員トークン (wifi-<username>) の存在確認。トークンがあれば部員、なければ空文字
	//
	// トークンの key は /wifi の削除後は使わないが、トークンの存在自体が
	// /wifi-cert の部員判定 (認可ゲート) として必要なので残す
	data, err := c.get("/api/v3/core/tokens/"+c.tokenPrefix+username+"/view_key/", nil)
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
			return "", nil
		}
		return "", err
	}
	key, _ := data["key"].(string)
	return key, nil
}

// errReason は *url.Error から根本の理由を取り出す (Python の URLError.reason に相当)
func errReason(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err.Error()
	}
	return err.Error()
}
