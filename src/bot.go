// Discord (discordgo) の実装。スラッシュコマンドの定義とイベントの Dispatch、
// 共通の認可ゲート。コマンドごとの処理は commands.go。
package main

import (
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

var defaultChannels = deviceChannels[defaultDevice]

// チャネル → 表示名 (/show と再発行ボタンのメッセージで使う)
var channelLabels = map[string]string{
	"mobileconfig": "iOS / iPadOS / macOS",
	"p12":          "Android / Windows など",
}

func deviceLabel(channels []string) string {
	switch {
	case len(channels) == 1 && channels[0] == "mobileconfig":
		return "Apple 端末 (iPhone / iPad / Mac)"
	case len(channels) == 1 && channels[0] == "p12":
		return "Android / Windows など"
	default:
		return "両方 (全端末で入れ直す)"
	}
}

func deviceOption(description string) *discordgo.ApplicationCommandOption {
	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(deviceChannels))
	for name := range deviceChannels {
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: name, Value: name})
	}
	return &discordgo.ApplicationCommandOption{
		Type:        discordgo.ApplicationCommandOptionString,
		Name:        "device",
		Description: description,
		Required:    false,
		Choices:     choices,
	}
}

// 再発行ボタンの CustomID (クリックされたとき channels を取り出す)
func reissueCustomID(channels []string) string {
	return "reissue:" + strings.Join(channels, ",")
}

var commands = []*discordgo.ApplicationCommand{
	{
		Name:        "connect",
		Description: "Authentik との Discord 連携状況を確認し、未連携なら連携する",
	},
	{
		Name:        "issue",
		Description: "Wi-Fi クライアント証明書を新しく発行して送る (本人にしか見えない)",
		Options: []*discordgo.ApplicationCommandOption{
			deviceOption("証明書を使う端末。有効期限が切れたときは /reissue でその端末だけ選べば、他の端末の証明書は無効になりません"),
		},
	},
	{
		Name:        "show",
		Description: "現在有効な自分の Wi-Fi クライアント証明書を表示する",
	},
	{
		Name:        "reissue",
		Description: "Wi-Fi クライアント証明書を再発行する (以前の証明書は破棄されます)",
		Options: []*discordgo.ApplicationCommandOption{
			deviceOption("再発行する証明書の端末。選んだ端末の証明書だけが新しくなり、他の端末の証明書は変わりません"),
		},
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

// deleteStaleCommands は現行のコマンド一覧 (commands) にない Discord 上の
// コマンド登録を消す。ApplicationCommandCreate は一覧にないコマンドを残すため、
// 古いバージョンの Bot (Python 版) が登録した /wifi-cert 等がギルドに残り続ける。
// ギルドスコープは現行一覧にない名前だけを削除し、グローバルスコープは
// ギルド限定運用なので全部削除する
func (b *bot) deleteStaleCommands(s *discordgo.Session) {
	current := make(map[string]bool, len(commands))
	for _, cmd := range commands {
		current[cmd.Name] = true
	}
	// ギルドスコープ
	guild, err := s.ApplicationCommands(s.State.User.ID, b.cfg.guildID)
	if err != nil {
		log.Printf("failed to list guild commands: %v", err)
	} else {
		for _, cmd := range guild {
			if current[cmd.Name] {
				continue
			}
			if err := s.ApplicationCommandDelete(s.State.User.ID, b.cfg.guildID, cmd.ID); err != nil {
				log.Printf("failed to delete stale /%s: %v", cmd.Name, err)
			} else {
				log.Printf("deleted stale guild command /%s", cmd.Name)
			}
		}
	}
	// グローバルスコープ (この Bot はギルド限定で登録するので全消し)
	global, err := s.ApplicationCommands(s.State.User.ID, "")
	if err != nil {
		log.Printf("failed to list global commands: %v", err)
		return
	}
	for _, cmd := range global {
		if err := s.ApplicationCommandDelete(s.State.User.ID, "", cmd.ID); err != nil {
			log.Printf("failed to delete global /%s: %v", cmd.Name, err)
		} else {
			log.Printf("deleted global command /%s", cmd.Name)
		}
	}
}

func (b *bot) onReady(s *discordgo.Session, _ *discordgo.Ready) {
	// ギルド限定で登録する (グローバル登録は反映に最大1時間かかるため)
	if b.registered {
		return
	}
	// 古いバージョンが登録したまま残っているコマンド (/wifi-cert 等) を消してから登録する。
	// ApplicationCommandCreate は追加 (上書き) だけで、一覧から外れたコマンドは残る
	b.deleteStaleCommands(s)
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
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		switch i.ApplicationCommandData().Name {
		case "connect":
			b.handleConnect(s, i)
		case "issue":
			b.handleIssue(s, i)
		case "show":
			b.handleShow(s, i)
		case "reissue":
			b.handleReissue(s, i)
		case "access-log":
			b.handleAccessLog(s, i)
		}
	case discordgo.InteractionMessageComponent:
		// 証明書メッセージの「再発行」ボタン
		b.onReissueButton(s, i)
	}
}

// linked は Authentik との連携だけを確認するゲート (/show・/connect 用)。
// 未連携なら誘導メッセージを送って false を返す
func (b *bot) linked(s *discordgo.Session, i *discordgo.InteractionCreate) (string, bool) {
	if b.cfg.authentikURL == "" || b.cfg.authentikToken == "" {
		b.followError(s, i, "この Bot は Authentik と連携されていません。管理者に連絡してください。")
		return "", false
	}
	username, err := b.authentik.usernameForDiscord(interactionUser(i).ID)
	if err != nil {
		b.followError(s, i, "Authentik に接続できませんでした: "+errReason(err))
		return "", false
	}
	if username == "" {
		sendConnectPrompt(b.cfg, s, i)
		return "", false
	}
	return username, true
}

// authorize は発行の認可ゲート (Discord 連携済み + 部員トークン wifi-<username>)。
// 通らなければ理由のメッセージを送って false を返す
func (b *bot) authorize(s *discordgo.Session, i *discordgo.InteractionCreate) (string, bool) {
	username, ok := b.linked(s, i)
	if !ok {
		return "", false
	}
	password, err := b.authentik.wifiPassword(username)
	if err != nil {
		b.followError(s, i, "Authentik に接続できませんでした: "+errReason(err))
		return "", false
	}
	if password == "" {
		b.followError(s, i,
			"まだ部員登録が済んでいません (Wi-Fi 用トークンがありません)。"+
				"管理者が roles/radius の play を実行するまでお待ちください。")
		return "", false
	}
	return username, true
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
