// Discord (discordgo) の実装。スラッシュコマンドの定義とイベントの Dispatch、
// 共通の認可ゲート。コマンドごとの処理は commands.go。
package main

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/bwmarrin/discordgo"
)

var commands = []*discordgo.ApplicationCommand{
	{
		Name:        "connect",
		Description: "Authentik との Discord 連携状況を確認し、未連携なら連携する",
	},
	{
		Name:        "wifi",
		Description: "Wi-Fi (HxS-AirLAN) の ID とパスワードを表示する (本人にしか見えない)",
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
	// エラーは ephemeral にしか出ないので、docker logs でも原因を追えるように記録する
	u := interactionUser(i)
	log.Printf("error for %s (id %s): %s", u.Username, u.ID, content)
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
		case "wifi":
			b.handleWifi(s, i)
		case "access-log":
			b.handleAccessLog(s, i)
		}
	}
}

// linked は Authentik との連携だけを確認するゲート (/connect 用)。
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
		log.Printf("not linked: discord user %s (id %s) has no authentik connection",
			interactionUser(i).Username, interactionUser(i).ID)
		sendConnectPrompt(b.cfg, s, i)
		return "", false
	}
	return username, true
}

// authorize は Wi-Fi 利用の認可ゲート (Discord 連携済み + 部員トークン wifi-<username>)。
// 通らなければ理由のメッセージを送って false を返す。password は取得済みの
// Wi-Fi パスワード (部員トークンの key) をそのまま返す
func (b *bot) authorize(s *discordgo.Session, i *discordgo.InteractionCreate) (string, string, bool) {
	username, ok := b.linked(s, i)
	if !ok {
		return "", "", false
	}
	password, err := b.authentik.wifiPassword(username)
	if err != nil {
		b.followError(s, i, "Authentik に接続できませんでした: "+errReason(err))
		return "", "", false
	}
	if password == "" {
		log.Printf("no wifi token: %s (discord id %s) is not a registered member", username, interactionUser(i).ID)
		b.followError(s, i,
			"まだ Wi-Fi 用トークンがありません。部員登録の反映まで数分待ってから、"+
				"もう一度コマンドを実行してください。")
		return "", "", false
	}
	return username, password, true
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
