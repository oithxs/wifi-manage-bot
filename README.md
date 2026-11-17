# wifi-manage-bot

部室 Wi-Fi (802.1X / PEAP-MSCHAPv2) の Discord Bot。次のスラッシュコマンドを提供する:

- **`/connect`**: Authentik との Discord 連携状況を確認し、未連携なら連携ボタンを出す
- **`/wifi`**: Wi-Fi の接続情報 (ID = Authentik の username、パスワード = 部員トークン
  `wifi-<username>` の key) を表示する。メッセージは**実行者にしか見えない** (ephemeral)
- **`/access-log`**: 現在 Wi-Fi に接続している部員の一覧と、最後の利用。FreeRADIUS が
  SQLite に書くアカウントリング (Accounting) を読む

表示の認可は Authentik で行う (Discord 連携済み + 部員トークン `wifi-<username>`
の存在)。証明書の発行は行わない (旧版の `/issue` `/reissue` は廃止。起動時に
自動で削除される)。

## 構成

```
src/main.go         エントリポイント (Bot 本体は Go + discordgo)
src/bot.go          スラッシュコマンドの定義と Discord イベントハンドラ
src/radius.go       アカウントリング (FreeRADIUS の SQLite) の読み取り
src/authentik.go    Authentik API クライアント (連携確認とパスワードの取得)
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
   | `WIFI_BOT_UID` / `WIFI_BOT_GID` | コンテナの実行ユーザー (ホストの `wifi-manage-bot` に合わせる) | `999` / `999` |
   | `FREERAD_GID` | DB を読むための補助グループ | `104` |

   uid/gid は `id -u wifi-manage-bot`、`getent group freerad` で確認する。

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

- **パスワードの供給元**: 部員トークン `wifi-<username>` の key は、iac の
  `roles/radius` が FreeRADIUS の users ファイル (`mods-config/files/authorize`)
  に書き込む Wi-Fi パスワードと同じ値。Bot は `view_key` API で取得して表示する
- **パスワードをログに残さない**: `/wifi` の返信内容 (パスワード) は
  `docker logs` に出力しない。失敗時の `followError` ログにも本文が出るが、
  パスワードが入る経路はない
