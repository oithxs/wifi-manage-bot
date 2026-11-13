# Wi-Fi Discord Bot (wifi-bot)。FreeRADIUS 機上で Docker Compose で動かす。
# - アカウントリング (SQLite) を読み取り専用で見る
# - 証明書の発行は sudo で wifi-cert-issue (root) を呼ぶ (CA 秘密鍵は root のみが読める)
# ユーザー/グループはホスト側の wifi-bot (uid) / freerad (gid) に合わせるため
# build 時に ARG で固定する (compose.yml の .env から渡す)
FROM python:3.13-slim

ARG WIFI_BOT_UID=999
ARG WIFI_BOT_GID=999

RUN apt-get update \
    && apt-get install -y --no-install-recommends sudo openssl \
    && rm -rf /var/lib/apt/lists/*

# ホストの wifi-bot と同じ uid/gid でユーザーを作る (bind mount の権限を揃えるため)
RUN groupadd -g "${WIFI_BOT_GID}" wifi-bot \
    && useradd -m -u "${WIFI_BOT_UID}" -g "${WIFI_BOT_GID}" wifi-bot

COPY requirements.txt /opt/wifi-bot/requirements.txt
RUN pip install --no-cache-dir -r /opt/wifi-bot/requirements.txt

# 証明書発行 helper (root 専用)。コンテナ内の sudoers で wifi-bot ユーザーだけが
# sudo -n で実行できる。ホスト側の sudoers は不要
# sudo は既定で環境変数をリセットするため、設定 (WIFI_CA_DIR 等) を env_keep で
# 通す (compose.yml の environment がコンテナ環境に置いた値を使う)
COPY wifi-cert-issue /usr/local/sbin/wifi-cert-issue
RUN chmod 0755 /usr/local/sbin/wifi-cert-issue \
    && echo "wifi-bot ALL=(root) NOPASSWD: /usr/local/sbin/wifi-cert-issue" \
        > /etc/sudoers.d/wifi-bot-certs \
    && echo 'Defaults env_keep += "WIFI_*"' \
        >> /etc/sudoers.d/wifi-bot-certs \
    && chmod 0440 /etc/sudoers.d/wifi-bot-certs

COPY bot.py /opt/wifi-bot/bot.py

USER wifi-bot
WORKDIR /opt/wifi-bot
CMD ["python", "bot.py"]
