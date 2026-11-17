// FreeRADIUS が SQLite に書き込むアカウントリング (Accounting) の読み取り。
package main

import (
	"database/sql"
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
}

// hostapd は既定で 300 秒間隔の Accounting-Interim-Update を送る
// (radacct の acctupdatetime が進み続ける)。端末が電源オフ・圏外等で
// Stop を送れずに消えると、acctstoptime が NULL のまま固まるので、
// 更新が 3 間隔 (900 秒) ないセッションは切断されたものとみなす
const acctInterimInterval = 300
const staleAfter = 3 * acctInterimInterval

func accessLogData(db *sql.DB) ([]sessionRow, error) {
	// AP (hostapd) は端末が切断するときに Stop を送り、AP の再起動時は
	// Accounting-Off でその NAS のセッションをすべて閉じるため、
	// acctstoptime が NULL ままのセッションが「接続中」を表す。
	// ただし Stop を取りこぼしたセッションは NULL のまま残るので、
	// 表示対象は Interim-Update が続いている (生きている) 部員に限定し、
	// 起点の時刻は同じ部員の固まったセッション (当日分) も含めた最も早い
	// 開始時刻にする (最初に接続した端末が消えても起点を維持するため)。
	// 時刻は FreeRADIUS が書き込む Unix エポック (整数)。
	liveCutoff := time.Now().Unix() - staleAfter
	// 当日の 0 時 (JST)。日をまたいだ固まった古いセッションを起点にしないための界限
	now := time.Now().In(jst)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, jst).Unix()
	rows, err := db.Query(
		"SELECT username, MIN(acctstarttime) FROM radacct"+
			" WHERE acctstoptime IS NULL"+
			" AND (acctstarttime >= ? OR COALESCE(acctupdatetime, acctstarttime) > ?)"+
			" AND username IN (SELECT username FROM radacct"+
			" WHERE acctstoptime IS NULL"+
			" AND COALESCE(acctupdatetime, acctstarttime) > ?)"+
			" GROUP BY username ORDER BY username",
		dayStart, liveCutoff, liveCutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 同じ部員のセッションが複数開いていても、最初に接続した時刻を出す
	var connected []sessionRow
	for rows.Next() {
		var row sessionRow
		if err := rows.Scan(&row.username, &row.started); err != nil {
			return nil, err
		}
		connected = append(connected, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return connected, nil
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
