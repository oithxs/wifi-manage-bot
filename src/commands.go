// スラッシュコマンド (/connect /wifi /access-log) の実装。
package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// --- /connect: Authentik との連携 ---

func (b *bot) handleConnect(s *discordgo.Session, i *discordgo.InteractionCreate) {
	deferEphemeral(s, i)
	if b.cfg.authentikURL == "" || b.cfg.authentikToken == "" {
		b.followError(s, i, "この Bot は Authentik と連携されていません。管理者に連絡してください。")
		return
	}
	username, err := b.authentik.usernameForDiscord(interactionUser(i).ID)
	if err != nil {
		b.followError(s, i, "Authentik に接続できませんでした: "+errReason(err))
		return
	}
	if username == "" {
		sendConnectPrompt(b.cfg, s, i)
		return
	}
	password, err := b.authentik.wifiPassword(username)
	if err != nil {
		b.followError(s, i, "Authentik に接続できませんでした: "+errReason(err))
		return
	}
	content := fmt.Sprintf("Authentik と連携済みです: `%s`\n", username)
	if password == "" {
		content += "Wi-Fi 用の部員トークンがありません (部員登録が済んでいません)。" +
			"管理者が roles/radius の play を実行するまでお待ちください。"
	} else {
		content += "部員登録も済んでいます。`/wifi` で Wi-Fi の接続情報を表示できます。"
	}
	followEphemeral(s, i, &discordgo.WebhookParams{Content: content})
}

// --- /wifi: Wi-Fi (PEAP-MSCHAPv2) の接続情報 ---

// handleWifi は認可ゲートを通った部員に Wi-Fi の ID とパスワードを ephemeral
// で送る。パスワードは Authentik の部員トークン (wifi-<username>) の key。
// ログにはパスワードを残さない
func (b *bot) handleWifi(s *discordgo.Session, i *discordgo.InteractionCreate) {
	deferEphemeral(s, i)
	username, password, ok := b.authorize(s, i)
	if !ok {
		return
	}
	log.Printf("sent wifi credentials to %s (id %s)", username, interactionUser(i).ID)
	content := fmt.Sprintf(
		"Wi-Fi (`%s`) の接続情報です。**このメッセージはあなたにしか見えません。**\n"+
			"- ID (identity): `%s`\n"+
			"- パスワード: ||`%s`||\n\n"+
			"端末の Wi-Fi 設定では WPA/WPA2-Enterprise、EAP 方式 = **PEAP**、"+
			"内側の認証 = **MSCHAPv2**、CA 証明書 = 「システム標準 / 指定しない」を選び、"+
			"上の ID とパスワードを入力してください。\n\n",
		b.cfg.ssid, username, password)
	followEphemeral(s, i, &discordgo.WebhookParams{
		Content:         content,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
}

// --- /access-log: 接続中の部員一覧 ---

func (b *bot) handleAccessLog(s *discordgo.Session, i *discordgo.InteractionCreate) {
	deferEphemeral(s, i)
	connected, last, err := accessLogData(b.db)
	if err != nil {
		b.followError(s, i, "アカウントリング DB を読めませんでした: "+err.Error())
		return
	}
	// 連携状況 (username → Discord ID) を取る。Authentik が落ちても username だけは出す
	usernames := make([]string, 0, len(connected)+1)
	for _, row := range connected {
		usernames = append(usernames, row.username)
	}
	if last != nil && !contains(usernames, last.username) {
		usernames = append(usernames, last.username)
	}
	info := map[string]userInfoEntry{}
	var infoErr error
	if b.cfg.authentikURL != "" && b.cfg.authentikToken != "" {
		if entries, err := b.authentik.userInfo(usernames); err == nil {
			info = entries
		} else {
			infoErr = err
		}
	}
	who := func(username string) string {
		// 連携済みなら Discord ID のメンション、未連携なら username をそのまま出す
		if entry, ok := info[username]; ok && entry.discordID != "" {
			return "<@" + entry.discordID + ">"
		}
		return "`" + username + "`"
	}
	lines := make([]string, 0, len(connected))
	for _, row := range connected {
		lines = append(lines, fmt.Sprintf("- %s — %s から", who(row.username), fmtTime(row.started)))
	}
	text := "Wi-Fi に接続している部員はいません。"
	if len(lines) > 0 {
		text = fmt.Sprintf("Wi-Fi に接続中 (%d 名):\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if last != nil {
		if last.stopped == nil {
			// 取りこぼした Stop のセッションは接続中と表示しない
			if updated, ok := toEpoch(last.updated); ok && time.Since(updated) > staleAfter {
				text += fmt.Sprintf("\n\n最後の利用: %s — %s 〜 %s (以後応答なし)",
					who(last.username), fmtTime(last.started), fmtTime(last.updated))
			} else {
				text += fmt.Sprintf("\n\n最後の利用: %s — %s から接続中", who(last.username), fmtTime(last.started))
			}
		} else {
			text += fmt.Sprintf("\n\n最後の利用: %s — %s 〜 %s", who(last.username), fmtTime(last.started), fmtTime(last.stopped))
		}
	}
	// メンションは表示にだけ使い、通知は飛ばさない
	// Authentik から連携状況を取れなかったときはその旨を出す (メンションが省略される)
	if infoErr != nil {
		text += "\n⚠️ Authentik から連携状況を取得できませんでした: " + errReason(infoErr)
	}
	followEphemeral(s, i, &discordgo.WebhookParams{
		Content:         text,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
