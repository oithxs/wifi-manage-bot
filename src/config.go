package main

import (
	"fmt"
	"os"
	"strings"
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
		// /wifi の返信の案内に出す SSID
		ssid:     envOr("WIFI_SSID", "HxS-AirLAN"),
		botToken: botToken,
	}, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
