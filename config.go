package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type config struct {
	dbPath         string
	guildID        string
	authentikURL   string
	authentikToken string
	sourceSlug     string
	tokenPrefix    string
	certHelper     string
	issuedDir      string
	serialDir      string
	certDays       int
	ssid           string
	verifyName     string
	botToken       string
}

func loadConfig() (*config, error) {
	required := func(key string) (string, error) {
		value := os.Getenv(key)
		if value == "" {
			return "", fmt.Errorf("環境変数 %s がありません", key)
		}
		return value, nil
	}
	dbPath, err := required("WIFI_RADIUS_DB")
	if err != nil {
		return nil, err
	}
	guildID, err := required("WIFI_DISCORD_GUILD_ID")
	if err != nil {
		return nil, err
	}
	botToken, err := required("WIFI_DISCORD_BOT_TOKEN")
	if err != nil {
		return nil, err
	}
	// /wifi-cert でユーザーを Discord ID から特定するための Authentik API (wifi-provision のトークン)
	authentikURL := strings.TrimRight(os.Getenv("WIFI_AUTHENTIK_URL"), "/")
	return &config{
		dbPath:         dbPath,
		guildID:        guildID,
		authentikURL:   authentikURL,
		authentikToken: os.Getenv("WIFI_AUTHENTIK_API_TOKEN"),
		sourceSlug:     envOr("WIFI_AUTHENTIK_SOURCE_SLUG", "discord"),
		// 部員トークン (wifi-<username>) の名前の prefix (FreeRADIUS 側の部員管理と一致させる)
		tokenPrefix: envOr("WIFI_TOKEN_PREFIX", "wifi-"),
		// クライアント証明書の発行 helper (/wifi-cert)。root のみが読める CA 秘密鍵で署名する
		certHelper: envOr("WIFI_CERT_HELPER", "/usr/local/sbin/wifi-cert-issue"),
		// helper が発行した p12 を置くディレクトリ (Bot が読み取って消す)
		issuedDir: envOr("WIFI_CERT_ISSUED_DIR", "/opt/wifi-bot/issued"),
		// 現在有効な証明書の serial 記録 (/show で読む。helper と FreeRADIUS と共有)
		serialDir: envOr("WIFI_SERIAL_DIR", "/var/lib/wifi-certs"),
		// 証明書の有効日数 (/show の有効期限の表示。helper と同じ値を .env で共有する)
		certDays: envInt("WIFI_CERT_DAYS", 365),
		// .mobileconfig (Apple 端末用プロファイル) の Wi-Fi 設定。SSID と RADIUS サーバー証明書の検証名
		ssid:       envOr("WIFI_SSID", "HxS-AirLAN"),
		verifyName: envOr("WIFI_RADIUS_VERIFY_NAME", "radius.h-x-s.dev"),
		botToken:   botToken,
	}, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}
