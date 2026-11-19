// /wifi の表示の認可と Wi-Fi トークンの自動発行 (provision.go) に使う
// Authentik API クライアント。
package main

import (
	"bytes"
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
		// Authentik の Pagination は next が「次のページ番号」で、最終ページでは 0 になる
		// (キーは常に存在する)。キーの存在だけで判断すると、存在しないページ 2 を
		// 取りに行って 404 になり、これまでに取れた結果が全部捨てられてしまう
		pagination, _ := data["pagination"].(map[string]any)
		switch next := pagination["next"].(type) {
		case float64:
			if next == 0 {
				return results, nil
			}
		case nil:
			// next が無い (または null) のも最終ページ扱い
			return results, nil
		}
	}
}

// post は JSON の body を送って、2xx なら応答 (あれば) を JSON で返す
func (c *authentikClient) post(path string, body map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.send(http.MethodPost, path, encoded)
}

// post204 は JSON の body を送って、2xx で応答がない (204) ことを期待する
func (c *authentikClient) post204(path string, body map[string]any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = c.send(http.MethodPost, path, encoded)
	return err
}

// delete は削除する。すでに消えている (404) は成功扱い (roles/radius の play と同じ)
func (c *authentikClient) delete(path string) error {
	_, err := c.send(http.MethodDelete, path, nil)
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
		return nil
	}
	return err
}

func (c *authentikClient) send(method, path string, body []byte) (map[string]any, error) {
	request, err := http.NewRequest(method, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
		request.Body = io.NopCloser(bytes.NewReader(body))
		request.ContentLength = int64(len(body))
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, &apiError{
			status: response.StatusCode,
			body:   strings.TrimSpace(string(raw)),
		}
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func (c *authentikClient) userInfo(usernames []string) (map[string]userInfoEntry, error) {
	// Authentik username → 連携していれば Discord ID。
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
		info[username] = userInfoEntry{}
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

// viewKey は部員トークン (wifi-<username>) の key を読む。
// トークンがない・失効している (view_key が 404) のときは空文字を返す
func (c *authentikClient) viewKey(identifier string) (string, error) {
	data, err := c.get("/api/v3/core/tokens/"+identifier+"/view_key/", nil)
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

func (c *authentikClient) wifiPassword(username string) (string, error) {
	// 部員トークン (wifi-<username>) の key = Wi-Fi (PEAP-MSCHAPv2) のパスワード。
	// roles/radius と wifi-manage-bot の provisioner が users ファイルに書き込む値と同じ。
	// トークンがなければ空文字 (未部員扱い)
	return c.viewKey(c.tokenPrefix + username)
}

// errReason は *url.Error から根本の理由を取り出す (Python の URLError.reason に相当)
func errReason(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err.Error()
	}
	return err.Error()
}
