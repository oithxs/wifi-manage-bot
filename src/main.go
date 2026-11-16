// Wi-Fi の接続状況の確認と証明書の発行を行う Discord Bot (wifi-manage-bot リポジトリ)。
//
// FreeRADIUS が SQLite に書き込むアカウントリング (radius.go) を読み、
//
//   - /access-log: acctstoptime が NULL (切断されていない) セッションの部員一覧。
//     ただし Interim-Update が止まった固まったセッションは切断扱いにする (radius.go)。
//     連携済みの部員は Discord のメンションで表示し、未連携の部員は username で表示する。
//     開始時刻は生きているセッションが 1 つでもあれば、当日の固まったセッションも
//     含めて最初に接続した時刻を起点にする。最後の利用 (誰が・いつから〜いつ) も出す
//   - /wifi-cert: 実行者の Wi-Fi クライアント証明書を発行して送る。
//     Apple 端末 (iOS / macOS / iPadOS) 向けに .mobileconfig (Wi-Fi 設定込みのプロファイル)、
//     それ以外の OS 向けに従来どおり .p12 とパスワードを添える。
//     誰に発行してよいかの判定 (Discord 連携済み + 部員トークン) は Authentik で行い
//     (authentik.go)、署名は root helper (wifi-cert-issue) がローカル CA で行う
//     (cert.go)。返信は ephemeral
//
// Discord 側の実装は bot.go、設定は config.go。
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	// タイムゾーンデータをバイナリに埋め込む (tzdata がない環境でも動くように)
	_ "time/tzdata"

	"github.com/bwmarrin/discordgo"

	// CGO 不要の SQLite ドライバ (アカウントリング DB の読み取り専用接続)
	_ "modernc.org/sqlite"
)

func main() {
	log.SetPrefix("wifi-manage-bot: ")
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	db, err := openDB(cfg.dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	dg, err := discordgo.New("Bot " + cfg.botToken)
	if err != nil {
		log.Fatal(err)
	}
	// スラッシュコマンドのみで intents は不要
	dg.Identify.Intents = discordgo.IntentsNone
	b := &bot{
		cfg: cfg,
		db:  db,
		authentik: &authentikClient{
			baseURL:     cfg.authentikURL,
			token:       cfg.authentikToken,
			sourceSlug:  cfg.sourceSlug,
			tokenPrefix: cfg.tokenPrefix,
		},
	}
	dg.AddHandler(b.onReady)
	dg.AddHandler(b.onInteraction)
	if err := dg.Open(); err != nil {
		log.Fatal(err)
	}
	log.Println("started")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")
	dg.Close()
}
