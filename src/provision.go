// Wi-Fi 部員のプロビジョニング。Authentik の部員グループの有効なユーザーごとに
// App パスワード トークン (wifi-<username>) を発行し、その key (= Wi-Fi のパスワード) を
// FreeRADIUS の users ファイルに書き込む。
//
// iac の roles/radius が play でやっていた操作と同じものを、起動時・一定間隔・
// Authentik の Webhook で行う。play は初回構築と手動の一括操作・再発行
// (radius_wifi_rotate_users) 用にそのまま残る。どちらで書いても users ファイルの
// 内容は同じになる。
//
// トークンの key (パスワード) はログに出力しない。
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// usersHeader は iac の roles/radius の users.j2 と同じヘッダー。Ansible の play も
// 同じ内容を書くので、片方だけがファイルを書き換えて HUP が往復しないように必ず同じにする
const usersHeader = `# Wi-Fi (PEAP-MSCHAPv2) 用のパスワード。Authentik の部員ごとの App パスワード
# (token wifi-<username>) の key から生成される。roles/radius と wifi-manage-bot の
# どちらも同じ内容を書く (Bot は起動時・定期同期・Webhook で更新する)
# 左側は端末の ID (identity) と同じ文字列 = メールの @ より前の部分 (Authentik の username)
# パスワードに " や \ を含めると壊れるので、生成側で文字種を絞っている
`

// users ファイルのエントリに使える username。roles/radius の assert と同じ文字だけ
var usersFileUsername = regexp.MustCompile(`^[a-z0-9._-]+$`)

// パスワードの文字数と文字種。roles/radius の lookup('password', ...) と同じ
const passwordLength = 24

const passwordChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

type provisioner struct {
	cfg       *config
	authentik *authentikClient
	// 同期を 1 回分だけ予約する。Webhook が何回来ても、実行中でなければ 1 回だけ行う
	pending chan struct{}
}

func newProvisioner(cfg *config, authentik *authentikClient) *provisioner {
	return &provisioner{
		cfg:       cfg,
		authentik: authentik,
		pending:   make(chan struct{}, 1),
	}
}

// request は同期を予約する (実行中に何回来ても、終わったあとに 1 回だけ行う)
func (p *provisioner) request() {
	select {
	case p.pending <- struct{}{}:
	default:
	}
}

// run は起動時に 1 回、あとは一定間隔 (と Webhook からの予約) で同期する。
// Authentik に届かないあいだはエラーを出して次の周期を待つ (users ファイルは書き換えないので
// すでに接続している部員には影響しない)
func (p *provisioner) run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.provisionInterval)
	defer ticker.Stop()
	p.request()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-p.pending:
		}
		start := time.Now()
		if err := p.reconcile(); err != nil {
			log.Printf("Wi-Fi provision failed: %v", err)
			continue
		}
		log.Printf("Wi-Fi provision finished (took %s)", time.Since(start).Round(time.Second))
	}
}

// listenAndServe は Authentik の Webhook を待ち受ける。Webhook の中身は見ず、同期を予約するだけ。
// 取りこぼしても定期同期で直る。Header だけ確認して残りは読まない
func (p *provisioner) listenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /webhooks/authentik", func(w http.ResponseWriter, r *http.Request) {
		want := "Bearer " + p.cfg.webhookToken
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		log.Println("received authentik webhook")
		p.request()
		w.WriteHeader(http.StatusAccepted)
	})
	srv := &http.Server{
		Addr:              p.cfg.webhookListen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		log.Printf("listening for authentik webhooks on %s", p.cfg.webhookListen)
		serverErr <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
	case err := <-serverErr:
		return err
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// reconcile は Authentik の部員とトークンを突き合わせて users ファイルを収束させる
func (p *provisioner) reconcile() error {
	members, err := p.authentik.list("/api/v3/core/users/", map[string]string{
		"is_active":      "true",
		"groups_by_name": p.cfg.memberGroup,
		"include_groups": "false",
	})
	if err != nil {
		return fmt.Errorf("部員の一覧を取れません: %w", err)
	}
	// 部員が 0 人は、グループの指定の間違いの可能性があるので users ファイルを空にしない
	// (roles/radius の assert と同じ。全員退部のときは play で対応する)
	if len(members) == 0 {
		return errors.New("部員グループの有効なユーザーがいません")
	}

	// username → ユーザーの pk。トークンを作るときに user に指定する
	memberPK := make(map[string]any, len(members))
	usernames := make([]string, 0, len(members))
	for _, member := range members {
		username, _ := member["username"].(string)
		if !usersFileUsername.MatchString(username) {
			log.Printf("skip member with an unrenderable username %q (users ファイルに a-z0-9._- 以外の文字)", username)
			continue
		}
		memberPK[username] = member["pk"]
		usernames = append(usernames, username)
	}
	// 部員がいても username が 1 人も書けないときは、users ファイルを空にして
	// しまわない (roles/radius の assert と同じ)
	if len(usernames) == 0 {
		return errors.New("users ファイルに書ける username の部員がいません")
	}

	tokens, err := p.authentik.list("/api/v3/core/tokens/", map[string]string{
		"intent":   "app_password",
		"expiring": "false",
	})
	if err != nil {
		return fmt.Errorf("トークンの一覧を取れません: %w", err)
	}
	// この Bot (と roles/radius の play) が管理するトークンだけに絞る。
	// 部員が自分で作った app_password は intent と expiring が同じなので、
	// 名前の prefix と description で区別する (play と同じ条件)
	type tokenRef struct {
		identifier string
		username   string
	}
	managed := make([]tokenRef, 0, len(tokens))
	for _, token := range tokens {
		identifier, _ := token["identifier"].(string)
		description, _ := token["description"].(string)
		if !strings.HasPrefix(identifier, p.cfg.tokenPrefix) || description != p.cfg.tokenDescription {
			continue
		}
		userObj, _ := token["user_obj"].(map[string]any)
		username, _ := userObj["username"].(string)
		managed = append(managed, tokenRef{identifier: identifier, username: username})
	}
	managedByUsername := make(map[string]tokenRef, len(managed))
	for _, token := range managed {
		managedByUsername[token.username] = token
	}

	// トークンがない部員には作って、新しいパスワードを key に設定する
	passwords := make(map[string]string, len(usernames))
	var errs []error
	for _, username := range usernames {
		if _, have := managedByUsername[username]; have {
			continue
		}
		password, err := p.createToken(username, memberPK[username])
		if err != nil {
			errs = append(errs, fmt.Errorf("トークンの作成 %s: %w", username, err))
			continue
		}
		passwords[username] = password
		log.Printf("issued Wi-Fi token for %s", username)
	}

	// すでにあるトークンの key を読む。読めなければ (失効・set_key されていない) 作り直す。
	// 部員でなくなった部員のトークンは消す (roles/radius の radius_wifi_prune と同じ)
	for _, token := range managed {
		if !slices.Contains(usernames, token.username) {
			if err := p.authentik.delete("/api/v3/core/tokens/" + token.identifier + "/"); err != nil {
				errs = append(errs, fmt.Errorf("トークンの削除 %s: %w", token.identifier, err))
				continue
			}
			log.Printf("deleted Wi-Fi token of %s (部員ではなくなったため)", token.username)
			continue
		}
		if _, ok := passwords[token.username]; ok {
			continue // このあいだに新しく発行した部員
		}
		key, err := p.authentik.viewKey(token.identifier)
		if err != nil {
			errs = append(errs, fmt.Errorf("トークンの key の取得 %s: %w", token.identifier, err))
			continue
		}
		if key != "" {
			passwords[token.username] = key
			continue
		}
		// view_key が 404 = 失効している。削除して作り直す
		if err := p.authentik.delete("/api/v3/core/tokens/" + token.identifier + "/"); err != nil {
			errs = append(errs, fmt.Errorf("失効したトークンの削除 %s: %w", token.identifier, err))
			continue
		}
		password, err := p.createToken(token.username, memberPK[token.username])
		if err != nil {
			errs = append(errs, fmt.Errorf("トークンの再発行 %s: %w", token.username, err))
			continue
		}
		passwords[token.username] = password
		log.Printf("re-issued expired Wi-Fi token for %s", token.username)
	}

	// 1 部員でもパスワードの揃っていない状態で users ファイルを書き換えると、その部員の
	// パスワードを失くしてしまうので、失敗が残っているときは今の内容のままにする
	missing := make([]string, 0)
	for _, username := range usernames {
		if _, ok := passwords[username]; !ok {
			missing = append(missing, username)
		}
	}
	if len(missing) > 0 {
		errs = append(errs, fmt.Errorf("パスワードを読めない部員: %s", strings.Join(missing, ", ")))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	content := renderUsersFile(passwords)
	if err := p.writeUsersFile(content); err != nil {
		return fmt.Errorf("users ファイルの更新: %w", err)
	}
	return nil
}

// createToken は部員トークン wifi-<username> を作り、key に新しいパスワードを設定して
// そのパスワードを返す (roles/radius の play と同じ)
func (p *provisioner) createToken(username string, pk any) (string, error) {
	password, err := generatePassword()
	if err != nil {
		return "", err
	}
	identifier := p.cfg.tokenPrefix + username
	if _, err := p.authentik.post("/api/v3/core/tokens/", map[string]any{
		"identifier":  identifier,
		"intent":      "app_password",
		"user":        pk,
		"description": p.cfg.tokenDescription,
		"expiring":    false,
	}); err != nil {
		return "", err
	}
	if err := p.authentik.post204("/api/v3/core/tokens/"+identifier+"/set_key/", map[string]any{
		"key": password,
	}); err != nil {
		return "", err
	}
	return password, nil
}

// generatePassword は Wi-Fi パスワード (users ファイルに書いても壊れない文字だけ) を作る
func generatePassword() (string, error) {
	buf := make([]byte, passwordLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = passwordChars[int(b)%len(passwordChars)]
	}
	return string(buf), nil
}

// renderUsersFile は users ファイルの内容を作る。部員は username でソートする
// (roles/radius の dictsort と同じ並び)
func renderUsersFile(passwords map[string]string) string {
	var b strings.Builder
	b.WriteString(usersHeader)
	for _, username := range slices.Sorted(maps.Keys(passwords)) {
		fmt.Fprintf(&b, "%s Cleartext-Password := \"%s\"\n", username, passwords[username])
	}
	return b.String()
}

// writeUsersFile は内容が変わっているときだけ、一時ファイルに書いてから置き換える。
// FreeRADIUS は起動時 (と HUP) にしか読まないので、置き換えの途中が見えることはない。
// 置き換えたファイルは freerad グループで読めないと FreeRADIUS が起動できないため、
// 所有グループを freerad (FREERAD_GID) に揃える (この Bot は freerad の補助グループに入っている)。
// ボリュームはディレクトリごとマウントしているので、rename で置き換えても同じパスで見える
func (p *provisioner) writeUsersFile(content string) error {
	existing, err := os.ReadFile(p.cfg.usersFile)
	if err == nil && string(existing) == content {
		return nil
	}
	dir := filepath.Dir(p.cfg.usersFile)
	tmp, err := os.CreateTemp(dir, ".authorize-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // rename が成功したあとは何もしない
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(fs.FileMode(0o660)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chown(-1, p.cfg.freeradGID); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), p.cfg.usersFile); err != nil {
		return err
	}
	log.Printf("updated the users file")
	return nil
}
