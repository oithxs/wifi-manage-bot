// FreeRADIUS が SQLite に書き込むアカウントリング (Accounting) の読み取り。
package main

import (
	"database/sql"
	"errors"
	"strconv"
	"time"
)

var jst = time.FixedZone("JST", 9*60*60)

func openDB(path string) (*sql.DB, error) {
	// freerad (書き込み側) と並行で動くので読み取り専用で開く
	return sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
}

type sessionRow struct {
	username string
	started  any
	stopped  any
	updated  any
}

// hostapd は既定で 300 秒間隔の Accounting-Interim-Update を送る
// (radacct の acctupdatetime が進み続ける)。端末が電源オフ・圏外等で
// Stop を送れずに消えると、acctstoptime が NULL のまま固まるので、
// 更新が 3 間隔 (900 秒) ないセッションは切断されたものとみなす
const acctInterimInterval = 300
const staleAfter = 3 * acctInterimInterval

func accessLogData(db *sql.DB) ([]sessionRow, *sessionRow, error) {
	// AP (hostapd) は端末が切断するときに Stop を送り、AP の再起動時は
	// Accounting-Off でその NAS のセッションをすべて閉じるため、
	// acctstoptime が NULL ままのセッションが「接続中」を表す。
	// ただし Stop を取りこぼしたセッションは NULL のまま残るので、
	// Interim-Update の更新がないものは除外する。
	// 時刻は FreeRADIUS が書き込む Unix エポック (整数)。
	rows, err := db.Query(
		"SELECT username, MIN(acctstarttime) FROM radacct"+
			" WHERE acctstoptime IS NULL"+
			" AND COALESCE(acctupdatetime, acctstarttime) > ?"+
			" GROUP BY username ORDER BY username",
		time.Now().Unix()-staleAfter)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	// 同じ部員のセッションが複数開いていても、最初に接続した時刻を出す
	var connected []sessionRow
	for rows.Next() {
		var row sessionRow
		if err := rows.Scan(&row.username, &row.started); err != nil {
			return nil, nil, err
		}
		connected = append(connected, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var last *sessionRow
	row := db.QueryRow(
		"SELECT username, acctstarttime, acctstoptime, acctupdatetime FROM radacct" +
			" ORDER BY acctstarttime DESC LIMIT 1")
	var rowLast sessionRow
	if err := row.Scan(&rowLast.username, &rowLast.started, &rowLast.stopped, &rowLast.updated); err == nil {
		last = &rowLast
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	return connected, last, nil
}

func toEpoch(value any) (time.Time, bool) {
	switch v := value.(type) {
	case int64:
		return time.Unix(v, 0), true
	case float64:
		return time.Unix(int64(v), 0), true
	case string:
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(sec, 0), true
		}
		// FreeRADIUS の schema によっては ISO 形式で書かれることもある
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", v, jst); err == nil {
			return t, true
		}
	case []byte:
		return toEpoch(string(v))
	}
	return time.Time{}, false
}

func fmtTime(epoch any) string {
	// radacct の Unix エポックを JST の「月-日 時:分」にする
	t, ok := toEpoch(epoch)
	if !ok {
		return "?"
	}
	return t.In(jst).Format("01-02 15:04")
}
