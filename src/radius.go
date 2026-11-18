// FreeRADIUS が SQLite に書き込むアカウントリング (Accounting) の読み取り。
package main

import (
	"database/sql"
	"time"
)

func openDB(path string) (*sql.DB, error) {
	// freerad (書き込み側) と並行で動くので読み取り専用で開く
	return sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
}

// hostapd は既定で 300 秒間隔の Accounting-Interim-Update を送る
// (radacct の acctupdatetime が進み続ける)。端末が電源オフ・圏外等で
// Stop を送れずに消えると、acctstoptime が NULL のまま固まるので、
// 更新が 3 間隔 (900 秒) ないセッションは切断されたものとみなす
const acctInterimInterval = 300
const staleAfter = 3 * acctInterimInterval

func accessLogData(db *sql.DB) ([]string, error) {
	// AP (hostapd) は端末が切断するときに Stop を送り、AP の再起動時は
	// Accounting-Off でその NAS のセッションをすべて閉じるため、acctstoptime が
	// NULL のままのセッションが「接続中」を表す。ただし Stop を取りこぼした
	// セッションは NULL のまま残るので、Interim-Update が続いている
	// (生きている) 部員だけを表示する
	liveCutoff := time.Now().Unix() - staleAfter
	rows, err := db.Query(
		"SELECT DISTINCT username FROM radacct"+
			" WHERE acctstoptime IS NULL"+
			" AND COALESCE(acctupdatetime, acctstarttime) > ?"+
			" ORDER BY username",
		liveCutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var connected []string
	for rows.Next() {
		var username string
		if err := rows.Scan(&username); err != nil {
			return nil, err
		}
		connected = append(connected, username)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return connected, nil
}
