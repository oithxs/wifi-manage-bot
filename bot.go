// Discord (discordgo) のイベントハンドラとスラッシュコマンド。
package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// 端末種別 → 発行するチャネル。チャネルごとに別の証明書・別の失効 (serial) なので、
// 1 つの端末だけの再発行は他の端末の証明書に影響しない
var deviceChannels = map[string][]string{
	"Apple 端末 (iPhone / iPad / Mac)": {"mobileconfig"},
	"Android / Windows など":           {"p12"},
	"両方 (全端末で入れ直す)":                  {"mobileconfig", "p12"},
}

var defaultDevice = "Apple 端末 (iPhone / iPad / Mac)"

var commands = []*discordgo.ApplicationCommand{
	{
		Name:        "wifi-cert",
		Description: "自分の Wi-Fi クライアント証明書を発行して送る (本人にしか見えない)",
		Options: []*discordgo.ApplicationCommandOption{{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "device",
			Description: "証明書を使う端末。有効期限が切れたときはその端末だけ選べば、他の端末の証明書は無効になりません",
			Required:    false,
			Choices: []*discordgo.ApplicationCommandOptionChoice{
				{Name: "Apple 端末 (iPhone / iPad / Mac)", Value: "Apple 端末 (iPhone / iPad / Mac)"},
				{Name: "Android / Windows など", Value: "Android / Windows など"},
				{Name: "両方 (全端末で入れ直す)", Value: "両方 (全端末で入れ直す)"},
			},
		}},
	},
	{
		Name:        "access-log",
		Description: "Wi-Fi に接続している部員を表示する",
	},
}

type bot struct {
	cfg        *config
	db         *sql.DB
	authentik  *authentikClient
	registered bool
}

func interactionUser(i *discordgo.InteractionCreate) *discordgo.User {
	if i.Member != nil {
		return i.Member.User
	}
	return i.User
}

func deferEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		log.Printf("failed to defer interaction: %v", err)
	}
}

func followEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, params *discordgo.WebhookParams) {
	if params.Flags == 0 {
		params.Flags = discordgo.MessageFlagsEphemeral
	}
	if _, err := s.FollowupMessageCreate(i.Interaction, true, params); err != nil {
		log.Printf("failed to send followup: %v", err)
	}
}

func (b *bot) followError(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	followEphemeral(s, i, &discordgo.WebhookParams{Content: content})
}

func (b *bot) onReady(s *discordgo.Session, _ *discordgo.Ready) {
	// ギルド限定で登録する (グローバル登録は反映に最大1時間かかるため)
	if b.registered {
		return
	}
	var firstErr error
	for _, cmd := range commands {
		if _, err := s.ApplicationCommandCreate(s.State.User.ID, b.cfg.guildID, cmd); err != nil {
			log.Printf("failed to register /%s: %v", cmd.Name, err)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			log.Printf("registered /%s (guild %s)", cmd.Name, b.cfg.guildID)
		}
	}
	if firstErr == nil {
		b.registered = true
	}
}

func (b *bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	switch i.ApplicationCommandData().Name {
	case "wifi-cert":
		b.handleWifiCert(s, i)
	case "access-log":
		b.handleAccessLog(s, i)
	}
}

func sendConnectPrompt(cfg *config, s *discordgo.Session, i *discordgo.InteractionCreate) {
	// 未連携のときに返す誘導 (設定画面の Connect ボタンと同じ URL)
	followEphemeral(s, i, &discordgo.WebhookParams{
		Content: "Authentik に Discord が連携されていません。" +
			"次のボタンから連携して、もう一度コマンドを実行してください。",
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					&discordgo.Button{
						Label: "Authentik で Discord を連携する",
						Style: discordgo.LinkButton,
						URL:   fmt.Sprintf("%s/source/oauth/login/%s/", cfg.authentikURL, cfg.sourceSlug),
					},
				},
			},
		},
	})
}

func (b *bot) handleWifiCert(s *discordgo.Session, i *discordgo.InteractionCreate) {
	deferEphemeral(s, i)
	if b.cfg.authentikURL == "" || b.cfg.authentikToken == "" {
		b.followError(s, i, "この Bot は Authentik と連携されていません。管理者に連絡してください。")
		return
	}
	device := defaultDevice
	for _, option := range i.ApplicationCommandData().Options {
		if option.Name == "device" {
			device = option.StringValue()
		}
	}
	channels, ok := deviceChannels[device]
	if !ok {
		channels = deviceChannels[defaultDevice]
	}
	discordID := interactionUser(i).ID
	username, err := b.authentik.usernameForDiscord(discordID)
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
	if password == "" {
		b.followError(s, i,
			"まだ部員登録が済んでいません (Wi-Fi 用トークンがありません)。"+
				"管理者が roles/radius の play を実行するまでお待ちください。")
		return
	}
	// 発行の認可はここまでの判定 (Discord 連携済み + 部員トークン) で済んでいる。
	// 選ばれた端末種別のチャネルだけを発行する。チャネルごとに別の証明書・別の失効
	// (serial 記録) なので、1 つの端末の再発行は他の端末の証明書に影響しない
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
	followEphemeral(s, i, &discordgo.WebhookParams{
		Content: fmt.Sprintf(
			"Wi-Fi クライアント証明書 (CN: `%s`) を発行しました (%s)。\n"+
				"1 年で有効期限が切れます。切れたら**同じ端末を選んで**もう一度 /wifi-cert を実行してください "+
				"(端末ごとに証明書が別なので、1 つの端末だけの再発行は他の端末に影響しません)。\n",
			issued[channels[0]].CN, device,
		) + notes.String(),
		Files: files,
	})
}

func (b *bot) handleAccessLog(s *discordgo.Session, i *discordgo.InteractionCreate) {
	deferEphemeral(s, i)
	connected, last, err := accessLogData(b.db)
	if err != nil {
		b.followError(s, i, "アカウントリング DB を読めませんでした: "+err.Error())
		return
	}
	// 氏名と連携状況 (username → 氏名/Discord ID) を取る。Authentik が落ちても username だけは出す
	usernames := make([]string, 0, len(connected)+1)
	for _, row := range connected {
		usernames = append(usernames, row.username)
	}
	if last != nil && !contains(usernames, last.username) {
		usernames = append(usernames, last.username)
	}
	info := map[string]userInfoEntry{}
	if b.cfg.authentikURL != "" && b.cfg.authentikToken != "" {
		if entries, err := b.authentik.userInfo(usernames); err == nil {
			info = entries
		}
	}
	who := func(username string) string {
		entry, ok := info[username]
		name := username
		if ok && entry.name != "" {
			name = entry.name
		}
		var parts []string
		if ok && entry.discordID != "" {
			parts = append(parts, "<@"+entry.discordID+">")
		}
		if name != username {
			parts = append(parts, name)
		}
		parts = append(parts, "`"+username+"`")
		if !(ok && entry.discordID != "") {
			parts = append(parts, "未連携")
		}
		return strings.Join(parts, " ")
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
			text += fmt.Sprintf("\n\n最後の利用: %s — %s から接続中", who(last.username), fmtTime(last.started))
		} else {
			text += fmt.Sprintf("\n\n最後の利用: %s — %s 〜 %s", who(last.username), fmtTime(last.started), fmtTime(last.stopped))
		}
	}
	// メンションは表示にだけ使い、通知は飛ばさない
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
