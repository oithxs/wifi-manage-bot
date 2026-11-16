# Wi-Fi Discord Bot (wifi-manage-bot)。FreeRADIUS 機上で Docker Compose で動かす。
# - 本体は Go (discordgo) で multi-stage build で静的バイナリにする
# - アカウントリング (SQLite) を読み取り専用で見る
# - 証明書の発行は sudo で wifi-cert-issue (root) を呼ぶ (CA 秘密鍵は root のみが読める)
# ユーザー/グループはホスト側の wifi-manage-bot (uid) / freerad (gid) に合わせるため
# build 時に ARG で固定する (compose.yml の .env から渡す)
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY src/go.mod src/go.sum ./
RUN go mod download
COPY src/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wifi-manage-bot .

FROM debian:bookworm-slim

ARG WIFI_BOT_UID=999
ARG WIFI_BOT_GID=999

RUN apt-get update \
    && apt-get install -y --no-install-recommends sudo openssl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# ホストの wifi-manage-bot と同じ uid/gid でユーザーを作る (bind mount の権限を揃えるため)
RUN groupadd -g "${WIFI_BOT_GID}" wifi-manage-bot \
    && useradd -m -u "${WIFI_BOT_UID}" -g "${WIFI_BOT_GID}" wifi-manage-bot

# 証明書発行 helper (root 専用)。コンテナ内の sudoers で wifi-manage-bot ユーザーだけが
# sudo -n で実行できる。ホスト側の sudoers は不要
# sudo は既定で環境変数をリセットするため、設定 (WIFI_CA_DIR 等) を env_keep で
# 通す (compose.yml の environment がコンテナ環境に置いた値を使う)
COPY wifi-cert-issue /usr/local/sbin/wifi-cert-issue
RUN chmod 0755 /usr/local/sbin/wifi-cert-issue \
    && echo "wifi-manage-bot ALL=(root) NOPASSWD: /usr/local/sbin/wifi-cert-issue" \
        > /etc/sudoers.d/wifi-manage-bot-certs \
    && echo 'Defaults env_keep += "WIFI_*"' \
        >> /etc/sudoers.d/wifi-manage-bot-certs \
    && chmod 0440 /etc/sudoers.d/wifi-manage-bot-certs

COPY --from=build /out/wifi-manage-bot /usr/local/bin/wifi-manage-bot

USER wifi-manage-bot
WORKDIR /opt/wifi-manage-bot
CMD ["/usr/local/bin/wifi-manage-bot"]
