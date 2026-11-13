// クライアント証明書の発行。署名は root helper (wifi-cert-issue) がローカル CA で行う。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"howett.net/plist"
)

type issuedItem struct {
	Channel  string `json:"channel"`
	File     string `json:"file"`
	Password string `json:"password"`
	CN       string `json:"cn"`
}

func issueCertificate(cfg *config, username string, channels []string) (map[string]issuedItem, error) {
	// root helper (wifi-cert-issue) を呼び、チャネルごとの発行結果を返す
	//
	// helper は mobileconfig (Apple 端末用プロファイルに埋め込む証明書) と p12
	// (それ以外の OS 用) に**別々の証明書**を発行し、失効 (serial 記録) も
	// チャネルごとに独立している。channels は発行するチャネル
	// (["mobileconfig"], ["p12"], または両方 ["mobileconfig", "p12"])。
	// 失敗時 (部員でない・CA がない等) はエラー
	argv := []string{"-n", cfg.certHelper, username}
	if len(channels) == 1 {
		argv = append(argv, channels[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "sudo", argv...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
				return nil, errors.New(stderr)
			}
			return nil, fmt.Errorf("exit code %d", exitErr.ExitCode())
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errors.New("timed out (60 秒以内に helper が完了しませんでした)")
		}
		return nil, err
	}
	// helper はチャネルごとに 1 行の JSON を出す (NDJSON)
	issued := make(map[string]issuedItem)
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var item issuedItem
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			return nil, fmt.Errorf("unexpected helper output: %q", output)
		}
		issued[item.Channel] = item
	}
	if len(issued) != len(channels) {
		return nil, fmt.Errorf("unexpected helper output: %q", output)
	}
	for _, channel := range channels {
		if _, ok := issued[channel]; !ok {
			return nil, fmt.Errorf("unexpected helper output: %q", output)
		}
	}
	return issued, nil
}

func uuidHex() string {
	// profile identifier / UUID は端末内で一意ならよいので username を入れて分かりやすくする
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%032X", time.Now().UnixNano())
	}
	return strings.ToUpper(hex.EncodeToString(b))
}

func buildMobileconfig(cfg *config, username string, p12 []byte, password string) ([]byte, error) {
	// Apple 端末 (iOS / macOS / iPadOS) 用のプロファイル (.mobileconfig) を作る
	//
	// p12 (証明書 + 秘密鍵 + CA 証明書) を com.apple.security.pkcs12 payload にそのまま
	// 入れ、Wi-Fi 設定 (com.apple.wifi.managed) から参照する。これで「インストール」だけで
	// 証明書の取り込みと Wi-Fi (EAP-TLS) の設定が済む。p12 のパスワードはプロファイルに
	// 埋め込むため、端末側で入力する場面はない
	certUUID := uuidHex()
	certPayload := map[string]any{
		"PayloadType":         "com.apple.security.pkcs12",
		"PayloadIdentifier":   fmt.Sprintf("dev.h-x-s.wifi.cert.%s", username),
		"PayloadUUID":         certUUID,
		"PayloadVersion":      1,
		"PayloadDisplayName":  fmt.Sprintf("Wi-Fi 証明書 (wifi-%s)", username),
		"PayloadOrganization": "HxS",
		// []byte は plist の <data> (base64) として書き出される
		"PayloadContent":             p12,
		"PayloadCertificateFileName": fmt.Sprintf("wifi-%s.p12", username),
	}
	// p12 が暗号化されていないときは Password を付けない (付けると空文字扱いになり
	// パスワード入力を求められることがある)
	if password != "" {
		certPayload["Password"] = password
	}
	wifiPayload := map[string]any{
		"PayloadType":         "com.apple.wifi.managed",
		"PayloadIdentifier":   fmt.Sprintf("dev.h-x-s.wifi.network.%s", username),
		"PayloadUUID":         uuidHex(),
		"PayloadVersion":      1,
		"PayloadDisplayName":  fmt.Sprintf("Wi-Fi (%s)", cfg.ssid),
		"PayloadOrganization": "HxS",
		"SSID_STR":            cfg.ssid,
		"HIDDEN_NETWORK":      false,
		// WPA / WPA2 / WPA3-Enterprise はすべて "WPA" (Apple の仕様)
		"EncryptionType": "WPA",
		"AutoJoin":       true,
		"CaptiveBypass":  false,
		// クライアント証明書 (identity) は Wi-Fi payload の PayloadCertificateUUID で
		// pkcs12 payload を参照する (EAPClientConfiguration 内ではない)
		"PayloadCertificateUUID": certUUID,
		"EAPClientConfiguration": map[string]any{
			// 13 = EAP-TLS
			"AcceptEAPTypes": []any{13},
			// サーバー証明書は OS 標準の信頼ストア (Let's Encrypt) で検証し、
			// 受け付けるサーバー証明書の CN (SAN) をこの名前に限定する
			"TLSTrustedServerNames": []any{cfg.verifyName},
		},
	}
	profile := map[string]any{
		"PayloadType":        "Configuration",
		"PayloadIdentifier":  fmt.Sprintf("dev.h-x-s.wifi.%s", username),
		"PayloadUUID":        uuidHex(),
		"PayloadVersion":     1,
		"PayloadDisplayName": fmt.Sprintf("HxS Wi-Fi (wifi-%s)", username),
		"PayloadDescription": fmt.Sprintf(
			"HxS 部室 Wi-Fi (%s) のクライアント証明書と接続設定。"+
				"期限切れ・再発行のときはこのプロファイルを削除してから新しいものを入れてください", cfg.ssid),
		"PayloadOrganization": "HxS",
		"PayloadContent":      []any{certPayload, wifiPayload},
	}
	return plist.Marshal(profile, plist.XMLFormat)
}

// readFileOnce はファイルを読み、成功・失敗に関わらず削除する (helper が置いた使い捨ての p12)
func readFileOnce(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	os.Remove(path)
	return data, err
}

// issuedCert は現在有効な証明書 1 枚分 (/show 用)。実体 (p12) は Bot が送信時に
// 削除するため残っておらず、残っているのは helper が書いた serial の記録だけ。
// 発行日時は serial ファイルの更新時刻、有効期限はそこに WIFI_CERT_DAYS を足した目安。
type issuedCert struct {
	Channel   string
	Serial    string
	IssuedAt  time.Time
	ExpiresAt time.Time
	Legacy    bool // 旧形式 (単一の serial ファイル。p12 チャネルの旧証明書)
}

func issuedCertificates(cfg *config, username string) ([]issuedCert, error) {
	dir := filepath.Join(cfg.serialDir, username)
	var certs []issuedCert
	for _, channel := range []string{"mobileconfig", "p12"} {
		path := filepath.Join(dir, "serial."+channel)
		data, mtime, err := readSerialFile(path)
		if err != nil {
			return nil, err
		}
		if data == "" {
			continue
		}
		certs = append(certs, issuedCert{
			Channel:   channel,
			Serial:    data,
			IssuedAt:  mtime,
			ExpiresAt: mtime.AddDate(0, 0, cfg.certDays),
		})
	}
	// 旧形式 (mobileconfig 配布開始以前) の serial ファイル。helper は p12 発行時に
	// 削除するので、残っている場合は「旧い p12 の証明書」が有効なままということ
	data, mtime, err := readSerialFile(filepath.Join(dir, "serial"))
	if err != nil {
		return nil, err
	}
	if data != "" {
		certs = append(certs, issuedCert{
			Channel:   "p12",
			Serial:    data,
			IssuedAt:  mtime,
			ExpiresAt: mtime.AddDate(0, 0, cfg.certDays),
			Legacy:    true,
		})
	}
	return certs, nil
}

func readSerialFile(path string) (string, time.Time, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}, err
	}
	return strings.TrimSpace(string(data)), info.ModTime(), nil
}
