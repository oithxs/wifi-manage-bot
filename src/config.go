package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	dbPath         string
	guildID        string
	authentikURL   string
	authentikToken string
	sourceSlug     string
	tokenPrefix    string
	ssid           string
	botToken       string
	// --- Wi-Fi 部員のプロビジョニング (provision.go) ---
	// 部員 (この Authentik グループの有効なユーザー) のトークンを自動で発行・管理する。
	// 空のときは発行しない (読み取り専用の Bot として動く)
	memberGroup string
	// Bot が書き換える FreeRADIUS の users ファイル (mods-config/files/authorize)
	usersFile string
	// users ファイルに付ける所有グループ (freerad) の gid。FreeRADIUS が読めるようにする
	freeradGID int
	// 同期の間隔。Authentik の Webhook を受けたときは即座に行う
	provisionInterval time.Duration
	// Authentik の Webhook を待ち受けるアドレスとトークン。トークンが空のときは待ち受けない
	webhookListen string
	webhookToken  string
	// この Bot が管理するトークンの description。roles/radius の play と同じ値を使う
	// (一致しているトークンだけをこの Bot が管理する)
	tokenDescription string
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
	// /wifi でユーザーを Discord ID から特定するための Authentik API (wifi-provision のトークン)
	authentikURL := strings.TrimRight(os.Getenv("WIFI_AUTHENTIK_URL"), "/")
	cfg := &config{
		dbPath:         dbPath,
		guildID:        guildID,
		authentikURL:   authentikURL,
		authentikToken: os.Getenv("WIFI_AUTHENTIK_API_TOKEN"),
		sourceSlug:     envOr("WIFI_AUTHENTIK_SOURCE_SLUG", "discord"),
		// 部員トークン (wifi-<username>) の名前の prefix (FreeRADIUS 側の部員管理と一致させる)
		tokenPrefix: envOr("WIFI_TOKEN_PREFIX", "wifi-"),
		// /wifi の返信の案内に出す SSID
		ssid:     envOr("WIFI_SSID", "HxS-AirLAN"),
		botToken: botToken,
	}
	// --- Wi-Fi 部員のプロビジョニング ---
	// 部員グループが空のときは発行しない (読み取り専用)
	cfg.memberGroup = os.Getenv("WIFI_MEMBER_GROUP")
	if cfg.memberGroup != "" {
		usersFile, err := required("WIFI_USERS_FILE")
		if err != nil {
			return nil, err
		}
		cfg.usersFile = usersFile
		// users ファイルの所有グループ (freerad)。FreeRADIUS が読めるように新しいファイルにも付ける
		freeradGID, err := strconv.Atoi(os.Getenv("FREERAD_GID"))
		if err != nil {
			return nil, fmt.Errorf("環境変数 FREERAD_GID がありません: %w", err)
		}
		cfg.freeradGID = freeradGID
		// 同期の間隔 (取ってこられなくても次の周期で直る)
		cfg.provisionInterval = 10 * time.Minute
		if value := os.Getenv("WIFI_PROVISION_INTERVAL"); value != "" {
			interval, err := time.ParseDuration(value)
			if err != nil {
				return nil, fmt.Errorf("環境変数 WIFI_PROVISION_INTERVAL: %w", err)
			}
			cfg.provisionInterval = interval
		}
		// Authentik の Webhook (roles/authentik が送る)。トークンが空のときは定期同期だけ
		cfg.webhookToken = os.Getenv("WIFI_WEBHOOK_TOKEN")
		cfg.webhookListen = envOr("WIFI_WEBHOOK_LISTEN", ":8080")
		// この Bot が管理するトークンの description。roles/radius の play と同じ値を使う
		// (一致しているトークンだけを管理する。値を変えると play と別々のトークンを作ってしまう)
		cfg.tokenDescription = envOr("WIFI_TOKEN_DESCRIPTION", "Wi-Fi password, managed by roles/radius")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
