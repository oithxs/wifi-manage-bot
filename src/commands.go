// スラッシュコマンド (/connect /issue /reissue /access-log) と
// 証明書メッセージの「再発行」ボタンの実装。
package main

import (
	"bytes"
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
		content += "部員登録も済んでいます。`/issue` で Wi-Fi クライアント証明書を発行できます。"
	}
	followEphemeral(s, i, &discordgo.WebhookParams{Content: content})
}

// --- /issue: 新規発行、/reissue: 再発行 ---

func (b *bot) handleIssue(s *discordgo.Session, i *discordgo.InteractionCreate) {
	deferEphemeral(s, i)
	// device オプションは廃止。mobileconfig と p12 の両方を発行するので、
	// どの端末にも正しいファイルが含まれる
	b.issueCertificates(s, i, allChannels, false)
}

func (b *bot) handleReissue(s *discordgo.Session, i *discordgo.InteractionCreate) {
	deferEphemeral(s, i)
	channels := channelsFromOptions(i)
	b.issueCertificates(s, i, channels, true)
}

func channelsFromOptions(i *discordgo.InteractionCreate) []string {
	for _, option := range i.ApplicationCommandData().Options {
		if option.Name == "device" {
			if channels, ok := deviceChannels[option.StringValue()]; ok {
				return channels
			}
		}
	}
	// device は必須で choices に絞られているので、通常ここには来ない
	return allChannels
}

// issueCertificates は証明書を発行 (再発行) して、再発行ボタンつきの ephemeral
// メッセージで送る。reissue が true のときは「以前の証明書は破棄されました」の説明を添える。
// helper は再発行のたびに serial を上書きするので、そのチャネルの古い証明書は
// FreeRADIUS 側で失効する (破棄される)
func (b *bot) issueCertificates(s *discordgo.Session, i *discordgo.InteractionCreate, channels []string, reissue bool) {
	device := deviceLabel(channels)
	username, ok := b.authorize(s, i)
	if !ok {
		return
	}
	issued, err := issueCertificate(b.cfg, username, channels)
	if err != nil {
		b.followError(s, i, "証明書を発行できませんでした: "+err.Error())
		return
	}
	var files []*discordgo.File
	var notes strings.Builder
	if item, ok := issued["mobileconfig"]; ok {
		mcData, err := readFileOnce(item.File)
		if err != nil {
			b.followError(s, i, "証明書ファイルを読めませんでした: "+err.Error())
			return
		}
		// Apple 端末用は Wi-Fi 設定込みのプロファイル。パスワードもプロファイルに入るので
		// 端末側で入力する必要がない
		mc, err := buildMobileconfig(b.cfg, username, mcData, item.Password)
		if err != nil {
			b.followError(s, i, "プロファイルを作れませんでした: "+err.Error())
			return
		}
		files = append(files, &discordgo.File{
			Name:        fmt.Sprintf("wifi-%s.mobileconfig", username),
			ContentType: "application/x-apple-aspen-config",
			Reader:      bytes.NewReader(mc),
		})
		notes.WriteString("- **iOS / iPadOS / macOS**: `.mobileconfig` を Safari で開いてプロファイルをインストールしてください (パスワード入力不要、Wi-Fi 設定も済みます)。\n")
	}
	if item, ok := issued["p12"]; ok {
		p12Data, err := readFileOnce(item.File)
		if err != nil {
			b.followError(s, i, "証明書ファイルを読めませんでした: "+err.Error())
			return
		}
		files = append(files, &discordgo.File{
			Name:        fmt.Sprintf("wifi-%s.p12", username),
			ContentType: "application/x-pkcs12",
			Reader:      bytes.NewReader(p12Data),
		})
		if item.Password != "" {
			fmt.Fprintf(&notes, "- **Android / Windows など**: `.p12` をインポートしてください。パスワード: `%s`\n", item.Password)
		} else {
			notes.WriteString("- **Android / Windows など**: `.p12` をインポートしてください (そのまま使えます、パスワードは不要です)。\n")
		}
	}
	var head string
	if reissue {
		head = fmt.Sprintf(
			"Wi-Fi クライアント証明書 (CN: `%s`) を**再発行**しました (%s)。\n"+
				"以前に発行した証明書は**破棄されました**。このメッセージの証明書だけが有効です。"+
				"古い端末や古いファイルに入っていた証明書は使えなくなるので、必要なら端末から削除してください。\n"+
				"1 年で有効期限が切れます。切れたら下の「再発行」ボタンから出し直してください "+
				"(端末ごとに証明書が別なので、1 つの端末だけの再発行は他の端末に影響しません)。\n",
			issued[channels[0]].CN, device,
		)
	} else {
		head = fmt.Sprintf(
			"Wi-Fi クライアント証明書 (CN: `%s`) を発行しました (%s)。\n"+
				"1 年で有効期限が切れます。切れたら下の「再発行」ボタンから出し直してください "+
				"(端末ごとに証明書が別なので、1 つの端末だけの再発行は他の端末に影響しません)。\n",
			issued[channels[0]].CN, device,
		)
	}
	content := head + notes.String() +
		"\n⚠️ **「再発行」ボタンについて**: ボタンで再発行すると、このメッセージの証明書は**破棄されます**" +
		"(以前の証明書は失効し、接続できなくなります)。すべての端末にインストールし終えてから押してください。"
	followEphemeral(s, i, &discordgo.WebhookParams{
		Content: content,
		Files:   files,
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					&discordgo.Button{
						Label:    "証明書を再発行する",
						Style:    discordgo.PrimaryButton,
						CustomID: reissueCustomID(channels),
					},
				},
			},
		},
	})
}

// --- 「再発行」ボタン (証明書メッセージの Components) ---

func (b *bot) onReissueButton(s *discordgo.Session, i *discordgo.InteractionCreate) {
	customID := i.MessageComponentData().CustomID
	channels := strings.Split(strings.TrimPrefix(customID, "reissue:"), ",")
	var valid []string
	for _, channel := range channels {
		if channel == "mobileconfig" || channel == "p12" {
			valid = append(valid, channel)
		}
	}
	deferEphemeral(s, i)
	if len(valid) == 0 {
		b.followError(s, i, "ボタンが不正です。/reissue で再発行してください。")
		return
	}
	// ボタンは ephemeral メッセージのものなので実行者本人にしか押せないが、
	// 連携・部員状態はコマンドと同じゲートで改めて確認する
	b.issueCertificates(s, i, valid, true)
	b.markReissued(s, i, customID)
}

// markReissued は押された元のメッセージを書き換える (ボタンを無効化し、
// そのメッセージの証明書が破棄済みであることを追記する)
func (b *bot) markReissued(s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	if i.Message == nil || i.Message.Content == "" {
		return
	}
	disabled := []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				&discordgo.Button{
					Label:    "再発行しました (このメッセージの証明書は破棄済み)",
					Style:    discordgo.SecondaryButton,
					Disabled: true,
					CustomID: customID,
				},
			},
		},
	}
	edited := i.Message.Content + "\n→ **再発行しました。このメッセージの証明書は破棄されました。**"
	if _, err := s.FollowupMessageEdit(i.Interaction, i.Message.ID, &discordgo.WebhookEdit{
		Content:    &edited,
		Components: &disabled,
	}); err != nil {
		log.Printf("failed to edit reissued message: %v", err)
	}
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
