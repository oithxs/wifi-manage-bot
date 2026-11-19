# wifi-manage-bot

部室 Wi-Fi (802.1X / PEAP-MSCHAPv2) の Discord Bot。次のスラッシュコマンドを提供する:

- **`/connect`**: Authentik との Discord 連携状況を確認し、未連携なら連携ボタンを出す
- **`/wifi`**: Wi-Fi の接続情報 (ID = Authentik の username、パスワード = 部員トークン
  `wifi-<username>` の key) を表示する。メッセージは**実行者にしか見えない** (ephemeral)
- **`/access-log`**: 現在 Wi-Fi に接続している部員の一覧。FreeRADIUS が
  SQLite に書くアカウントリング (Accounting) を読む

表示の認可は Authentik で行う (Discord 連携済み + 部員トークン `wifi-<username>`
の存在)。証明書の発行は行わない (旧版の `/issue` `/reissue` は廃止。起動時に
自動で削除される)。

**Wi-Fi トークンの自動発行 (provisioning)** もこの Bot が行う。Authentik の部員
グループに追加されたユーザーに対して、起動時・一定間隔 (`WIFI_PROVISION_INTERVAL`)・
Authentik の Webhook で部員トークン `wifi-<username>` を発行し、その key (= Wi-Fi
パスワード) を FreeRADIUS の users ファイル (`mods-config/files/authorize`) に
書き込む。iac の `roles/radius` の play と同じ操作を継続的に行うもので、play は
初回構築と手動の一括操作・再発行 (`radius_wifi_rotate_users`) 用に残る。
`WIFI_MEMBER_GROUP` が空のときは発行しない (読み取り専用の Bot として動く)。

## 構成

```
src/main.go         エントリポイント (Bot 本体は Go + discordgo)
src/bot.go          スラッシュコマンドの定義と Discord イベントハンドラ
src/commands.go     スラッシュコマンド (/connect /wifi /access-log) の実装
src/radius.go       アカウントリング (FreeRADIUS の SQLite) の読み取り
src/authentik.go    Authentik API クライアント (連携確認・パスワードの取得・トークンの操作)
src/provision.go   Wi-Fi トークンの自動発行 (部員の同期と users ファイルの更新)
src/config.go       環境変数の読み込み
src/go.mod / go.sum Go 依存 (モジュール: github.com/oithxs/wifi-manage-bot)
Dockerfile          コンテナイメージ (golang でビルド + debian:bookworm-slim で実行)
compose.yml         実行環境 (FreeRADIUS 機上で動かす)
.env.example        環境変数のひな形 (.env にコピーして使う。秘密なのでコミットしない)
```

## 実行 (FreeRADIUS 機上)

1. `.env.example` を `.env` にコピーして埋める (Discord トークン、Authentik トークン、
   ホスト側パス、uid/gid)。マウントするホスト側のパス:

   | .env 変数 | 内容 | 既定 |
   |---|---|---|
   | `WIFI_RADIUS_DB_HOST` | アカウントリング DB (読み取り専用) | `/var/lib/freeradius/radius.sqlite` |
   | `WIFI_USERS_DIR_HOST` | Wi-Fi トークンの発行で書き換える users ファイルのディレクトリ | `/etc/freeradius/3.0/mods-config/files` |
   | `WIFI_BOT_UID` / `WIFI_BOT_GID` | コンテナの実行ユーザー (ホストの `wifi-manage-bot` に合わせる) | `999` / `999` |
   | `FREERAD_GID` | DB と users ファイルに触るための補助グループ | `104` |

   uid/gid は `id -u wifi-manage-bot`、`getent group freerad` で確認する。

   **トークンの自動発行を有効にするとき**は、Bot が users ファイルを書き換えられる
   ようにホスト側の準備が要る (iac の `roles/radius` がやること):

   - users ファイルのディレクトリ (`mods-config/files`) を `root:freerad 0770`、
     ファイルを `root:freerad 0660` にする (Bot は freerad の補助グループで書き込む)
   - users ファイルが変わったら FreeRADIUS に HUP を送る仕掛け (systemd の path unit +
     `docker kill --signal=HUP freeradius`)。FreeRADIUS は起動時にしか users ファイルを
     読まないため

   users ファイルの内容 (ヘッダーも) は iac の `roles/radius` の `users.j2` と同じ
   形式で書く。片方だけが書き換えて HUP が往復しないように、両方で必ず同じ内容になる。

2. 起動:

   ```sh
   docker compose up -d --build
   docker compose logs -f wifi-manage-bot
   ```

3. 停止・更新:

   ```sh
   docker compose down
   docker compose up -d --build   # .go ファイルを変えたら --build で再ビルド
   ```

## 開発

Go ツールチェーンがあれば直接ビルド・実行もできる:

```sh
cd src && go build -o ../wifi-manage-bot .   # 静的バイナリ (CGO 不要)
../wifi-manage-bot                           # WIFI_* 環境変数 (.env は読まないので手動で export する)
```

Bot の登録コマンド (`/connect`, `/wifi`, `/access-log`) はギルド限定で
起動直後 (Ready) に同期する。トークンを替えて再起動すれば反映される。
現行の一覧にないコマンド (証明書版の `/issue` `/reissue` や旧 Python 版の
`/wifi-cert` 等) は起動時に自動で削除する (bot.go の `deleteStaleCommands`)。

## 設計メモ

- **パスワードの供給元**: 部員トークン `wifi-<username>` の key は、users ファイルに
  書く Wi-Fi パスワードと同じ値。Bot は `view_key` API で取得して表示する。
  トークンの発行・key の設定 (`set_key`) も同じ API で行う (iac の `roles/radius`
  の play と同じ操作)
- **管理するトークンの範囲**: 名前が `wifi-` (WIFI_TOKEN_PREFIX) で始まり、
  description が `Wi-Fi password, managed by roles/radius`
  (WIFI_TOKEN_DESCRIPTION) のトークンだけを管理する。部員が自分で作った
  app_password と混同しないためで、play と同じ条件
- **パスワードをログに残さない**: `/wifi` の返信内容 (パスワード) は
  `docker logs` に出力しない。失敗時の `followError` ログにも本文が出るが、
  パスワードが入る経路はない。プロビジョニングのログにも部員の username しか出ない
- **失敗しても users ファイルを壊さない**: Authentik に届かないあいだや、一部の部員の
  パスワードが読めないときは users ファイルを書き換えない (接続中の部員への影響が
  ないようにする)。次の周期 (または Webhook) で直る
- **トークンの失効に備える**: 部員トークンの `view_key` が 404 になったら、トークンを
  作り直して新しいパスワードを発行する (旧パスワードは使えなくなっているため)
- **Bot が止まると発行も止まる**: Discord へのログインに失敗するとプロセスごと再起動する
  (プロビジョニングも一時的に止まる。トークンの再発行は iac の `roles/radius` の play
  でも行える)
