# wifi-bot

部室 Wi-Fi (802.1X / EAP-TLS) の Discord Bot。次の 2 つのスラッシュコマンドを提供する:

- **`/access-log`**: 現在 Wi-Fi に接続している部員の一覧と、最後の利用。FreeRADIUS が
  SQLite に書くアカウントリング (Accounting) を読む
- **`/wifi-cert`**: 実行者本人の Wi-Fi クライアント証明書を発行して送る。Apple 端末
  (iOS / iPadOS / macOS) には Wi-Fi 設定込みの `.mobileconfig`、それ以外の OS には
  `.p12` とパスワード。端末種別ごとに**別の証明書**を発行し、失効 (再発行) も端末種別ごとに独立

発行の認可は Authentik (Discord 連携済み + 部員トークン `wifi-<username>` の存在)、
署名は同梱の root helper (`wifi-cert-issue`) がローカル CA で行う。

## 構成

```
bot.py             Bot 本体 (discord.py)
wifi-cert-issue    証明書発行 helper (root 専用、設定は環境変数)
Dockerfile         コンテナイメージ (python:3.13-slim + sudo + openssl)
compose.yml        実行環境 (FreeRADIUS 機上で動かす)
requirements.txt   Python 依存
.env.example       環境変数のひな形 (.env にコピーして使う。秘密なのでコミットしない)
```

ディレクトリは自己完結している (上位の ansible role などへの参照がない)。
**別リポジトリに分離するときはこのディレクトリを丸ごと `mv` して `git init` するだけ。**
iac 側 (`ansible/roles/radius`) は `radius_bot_repo_dir` 変数でこのディレクトリの場所を
参照しているので、移動したらその値を変えるだけでよい。

## 実行 (FreeRADIUS 機上)

1. `.env.example` を `.env` にコピーして埋める (Discord トークン、Authentik トークン、
   ホスト側パス、uid/gid)。マウントするホスト側のパス:

   | .env 変数 | 内容 | 既定 |
   |---|---|---|
   | `WIFI_RADIUS_DB_HOST` | アカウントリング DB (読み取り専用) | `/var/lib/freeradius/radius.sqlite` |
   | `WIFI_CA_DIR_HOST` | 部員証明書に署名するローカル CA | `/etc/freeradius/3.0/certs` |
   | `WIFI_SERIAL_DIR_HOST` | 証明書の serial 記録 (FreeRADIUS 側の照合と共有) | `/var/lib/wifi-certs` |
   | `WIFI_ISSUED_DIR_HOST` | 発行した p12 の受け渡し場所 | `/opt/wifi-bot/issued` |
   | `WIFI_BOT_UID` / `WIFI_BOT_GID` | コンテナの実行ユーザー (ホストの `wifi-bot` に合わせる) | `999` / `999` |
   | `FREERAD_GID` | DB を読むための補助グループ | `104` |

   uid/gid は `id -u wifi-bot`、`getent group freerad` で確認する。

2. 起動:

   ```sh
   docker compose up -d --build
   docker compose logs -f wifi-bot
   ```

3. 停止・更新:

   ```sh
   docker compose down
   docker compose up -d --build   # bot.py を変えたら --build で再ビルド
   ```

serial ディレクトリと issued ディレクトリはホストと共有する。helper は serial を
root 所有・0644 で書くので、FreeRADIUS 側の `wifi-cert-check` (exec xlat) がそのまま読める。

## 開発

ローカルで動かす場合も Docker Compose を使える (マウント先はモックで用意するか、
DB と CA だけ実物を参照する)。トークンを `.env` に書いて:

```sh
docker compose up --build
```

Bot の登録コマンド (`/wifi-cert`, `/access-log`) はギルド限定で `setup_hook` が
同期する。トークンを替えて再起動すれば反映される。

## 設計メモ

- **発行チャネル**: `bot.py` の `DEVICE_CHANNELS` (Apple 端末 → mobileconfig /
  それ以外 → p12 / 両方)。helper はチャネルごとに別の鍵ペア・証明書を作り、
  `<username>/serial.mobileconfig` と `<username>/serial.p12` をそれぞれ上書きする。
  1 つの端末だけの再発行は他の端末の証明書に影響しない
- **helper の権限**: CA 秘密鍵は root のみが読めるので、Bot (非 root) は
  `sudo -n` で helper を呼ぶ。sudo は既定で環境変数をリセットするため、sudoers に
  `Defaults env_keep += "WIFI_*"` を入れて設定を通している。sudoers は
  Dockerfile でコンテナ内に置き (`/etc/sudoers.d/wifi-bot-certs`)、ホスト側の
  sudoers は不要
- **CA への書き込みなし**: CA ディレクトリは読み取り専用でマウントする
  (`ca.key` をコンテナから書き換えられないようにするため)。openssl の serial
  管理ファイル (`ca.srl`) は helper の作業ディレクトリに作る
  (`-CAcreateserial` はファイルが無いときランダムな serial を生成する)
- **旧形式との互換**: mobileconfig 配布開始以前の証明書は旧形式の
  `<username>/serial` (単一ファイル) で管理されていた。helper は p12 チャネルの
  発行時にこのファイルを削除して移行する。FreeRADIUS 側の `wifi-cert-check`
  (iac の roles/radius) は両形式を照合する
