// provision.go の単体テスト。users ファイルの内容は iac の roles/radius の users.j2
// (Ansible の template は trim_blocks で展開する) とバイト単位で一致させる必要が
// あるため、ここで形を固定する
package main

import (
	"strings"
	"testing"
)

func TestRenderUsersFile(t *testing.T) {
	got := renderUsersFile(map[string]string{"carol": "pw3", "alice": "pw1", "bob": "pw2"})
	want := usersHeader +
		"alice Cleartext-Password := \"pw1\"\n" +
		"bob Cleartext-Password := \"pw2\"\n" +
		"carol Cleartext-Password := \"pw3\"\n"
	if got != want {
		t.Errorf("renderUsersFile() =\n%q\nwant =\n%q", got, want)
	}
	// iac の users.j2 のヘッダーと同じ文字列で始まる (roles/radius の play が同じ内容を書く)
	if !strings.HasPrefix(got, "# Wi-Fi (PEAP-MSCHAPv2) 用のパスワード。") {
		t.Errorf("renderUsersFile() が users.j2 のヘッダーで始まっていません: %q", got)
	}
}

// パスワードは users ファイルに書いても壊れない文字だけで作る
// (roles/radius の play の ascii_letters,digits と同じ)
func TestGeneratePassword(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		password, err := generatePassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(password) != passwordLength {
			t.Fatalf("password length = %d, want %d", len(password), passwordLength)
		}
		for _, c := range password {
			if !strings.ContainsRune(passwordChars, c) {
				t.Fatalf("password %q has a character outside the allowed set", password)
			}
		}
		seen[password] = true
	}
	if len(seen) < 2 {
		t.Fatal("generated passwords are not random")
	}
}
