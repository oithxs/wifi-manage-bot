// Wi-Fi の接続状況の確認と Wi-Fi (PEAP-MSCHAPv2) の接続情報の表示を行う Discord Bot
// (wifi-manage-bot リポジトリ)。
//
// FreeRADIUS が SQLite に書き込むアカウントリング (radius.go) を読み、
//
//   - /access-log: acctstoptime が NULL (切断されていない) セッションの部員一覧。
//     ただし Interim-Update が止まった固まったセッションは切断扱いにする (radius.go)。
//     連携済みの部員は Discord のメンションで表示し、未連携の部員は username で表示する。
//     開始時刻は生きているセッションが 1 つでもあれば、当日の固まったセッションも
//     含めて最初に接続した時刻を起点にする
//   - /wifi: 実行者の Wi-Fi 接続情報 (identity = Authentik の username と
//     部員トークン wifi-<username> の key = パスワード) を ephemeral で表示する。
//     誰に表示してよいかの判定 (Discord 連携済み + 部員トークンの存在) は Authentik で行う
//     (authentik.go)。返信は ephemeral
//
// Authentik に追加された部員への Wi-Fi トークンの自動発行 (provision.go) もこの
// プロセスで行う。起動時・一定間隔・Authentik の Webhook で、部員のトークンと
// FreeRADIUS の users ファイルを Authentik に合わせて更新する (WIFI_MEMBER_GROUP が
// 空のときは行わない)。
//
// Discord 側の実装は bot.go、設定は config.go。
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	authentik := &authentikClient{
		baseURL:     cfg.authentikURL,
		token:       cfg.authentikToken,
		sourceSlug:  cfg.sourceSlug,
		tokenPrefix: cfg.tokenPrefix,
	}

	// Wi-Fi 部員のプロビジョニング。Discord とは独立に動かす
	// (Authentik に届かないあいだは失敗を繰り返して、次の周期で直る)
	if cfg.memberGroup != "" {
		provisioner := newProvisioner(cfg, authentik)
		go provisioner.run(ctx)
		if cfg.webhookToken != "" {
			go func() {
				if err := provisioner.listenAndServe(ctx); err != nil &&
					!errors.Is(err, http.ErrServerClosed) {
					log.Printf("webhook server error: %v", err)
				}
			}()
		}
	}

	dg, err := discordgo.New("Bot " + cfg.botToken)
	if err != nil {
		log.Fatal(err)
	}
	// スラッシュコマンドのみで intents は不要
	dg.Identify.Intents = discordgo.IntentsNone
	b := &bot{
		cfg:       cfg,
		db:        db,
		authentik: authentik,
	}
	dg.AddHandler(b.onReady)
	dg.AddHandler(b.onInteraction)
	if err := dg.Open(); err != nil {
		log.Fatal(err)
	}
	log.Println("started")
	<-ctx.Done()
	log.Println("shutting down")
	dg.Close()
}
