package colstore

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// colRef 是结构版本中的一列：colID 全局唯一标识一列的“身份”，
// 同名再加列会分配新的 colID，与旧列互不相关。
type colRef struct {
	colID int64
	name  string
	typ   Kind
}

// segment 记录一列数据文件中的一段连续行：
// 该列文件中的这一段依次对应表的全局行号 [StartRow, StartRow+Count)。
// 一列的各段在文件中按追加顺序首尾相接。
type segment struct {
	StartRow int64
	Count    int64
}

// meta 封装单张表的 SQLite 元数据访问。
type meta struct {
	db *sql.DB
}

func openMeta(path string) (*meta, error) {
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)", path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	m := &meta{db: db}
	err = m.exec(`
CREATE TABLE IF NOT EXISTS meta_kv (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS columns (
  col_id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  type INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS versions (version INTEGER PRIMARY KEY);
CREATE TABLE IF NOT EXISTS version_cols (
  version INTEGER NOT NULL,
  col_id INTEGER NOT NULL,
  pos INTEGER NOT NULL,
  PRIMARY KEY (version, col_id)
);
CREATE TABLE IF NOT EXISTS segments (
  col_id INTEGER NOT NULL,
  seq INTEGER NOT NULL,
  start_row INTEGER NOT NULL,
  count INTEGER NOT NULL,
  PRIMARY KEY (col_id, seq)
);
`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return m, nil
}

func (m *meta) exec(query string, args ...any) error {
	_, err := m.db.Exec(query, args...)
	return err
}

func (m *meta) close() error { return m.db.Close() }

func (m *meta) getKV(key string) (int64, error) {
	var v int64
	err := m.db.QueryRow(`SELECT value FROM meta_kv WHERE key=?`, key).Scan(&v)
	return v, err
}

func (m *meta) setKV(key string, v int64) error {
	_, err := m.db.Exec(`INSERT INTO meta_kv(key,value) VALUES(?,?)
  ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, v)
	return err
}

// initialized 报告该表是否已建过（meta_kv 非空）。
func (m *meta) initialized() (bool, error) {
	var n int
	err := m.db.QueryRow(`SELECT COUNT(*) FROM meta_kv`).Scan(&n)
	return n > 0, err
}

// initSchema 建立版本 1 的初始结构。
func (m *meta) initSchema(schema []Column) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO versions(version) VALUES(1)`); err != nil {
		return err
	}
	for i, c := range schema {
		if _, err := tx.Exec(`INSERT INTO columns(col_id,name,type) VALUES(?,?,?)`, i, c.Name, int(c.Type)); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO version_cols(version,col_id,pos) VALUES(1,?,?)`, i, i); err != nil {
			return err
		}
	}
	for _, kv := range [][2]any{
		{"row_count", 0}, {"current_version", 1}, {"next_col_id", len(schema)},
	} {
		if _, err := tx.Exec(`INSERT INTO meta_kv(key,value) VALUES(?,?)`, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// schemaAt 返回指定版本的列（按 pos 升序）。
func (m *meta) schemaAt(version int64) ([]colRef, error) {
	rows, err := m.db.Query(`
SELECT c.col_id, c.name, c.type
FROM version_cols vc JOIN columns c ON c.col_id = vc.col_id
WHERE vc.version=? ORDER BY vc.pos`, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []colRef
	for rows.Next() {
		var r colRef
		var typ int
		if err := rows.Scan(&r.colID, &r.name, &typ); err != nil {
			return nil, err
		}
		r.typ = Kind(typ)
		out = append(out, r)
	}
	return out, rows.Err()
}

// retainedVersions 返回保留的所有版本号（升序）。
func (m *meta) retainedVersions() ([]int64, error) {
	rows, err := m.db.Query(`SELECT version FROM versions ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// commitVersion 在事务内写入一个新版本（版本号=当前+1，列为 cols），
// 并把 current_version 指向它，然后裁剪到最近 KeepVersions 个版本，
// 返回不再被任何保留版本引用的列 ID（其数据文件应删除）。
func (m *meta) commitVersion(cols []colRef) (newVersion int64, garbage []int64, err error) {
	cur, err := m.getKV("current_version")
	if err != nil {
		return 0, nil, err
	}
	newVersion = cur + 1
	tx, err := m.db.Begin()
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO versions(version) VALUES(?)`, newVersion); err != nil {
		return 0, nil, err
	}
	for i, c := range cols {
		if _, err := tx.Exec(`INSERT INTO version_cols(version,col_id,pos) VALUES(?,?,?)`, newVersion, c.colID, i); err != nil {
			return 0, nil, err
		}
	}
	if _, err := tx.Exec(`UPDATE meta_kv SET value=? WHERE key='current_version'`, newVersion); err != nil {
		return 0, nil, err
	}
	// 裁剪：只保留最近 KeepVersions 个版本
	if _, err := tx.Exec(`DELETE FROM versions WHERE version <= ?`, newVersion-KeepVersions); err != nil {
		return 0, nil, err
	}
	if _, err := tx.Exec(`DELETE FROM version_cols WHERE version <= ?`, newVersion-KeepVersions); err != nil {
		return 0, nil, err
	}
	// 找出不再被任何保留版本引用的列
	rows, err := tx.Query(`
SELECT col_id FROM columns
WHERE col_id NOT IN (SELECT DISTINCT col_id FROM version_cols)`)
	if err != nil {
		return 0, nil, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, nil, err
		}
		garbage = append(garbage, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, nil, err
	}
	rows.Close()
	for _, id := range garbage {
		if _, err := tx.Exec(`DELETE FROM columns WHERE col_id=?`, id); err != nil {
			return 0, nil, err
		}
		if _, err := tx.Exec(`DELETE FROM segments WHERE col_id=?`, id); err != nil {
			return 0, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}
	return newVersion, garbage, nil
}

// registerColumn 在 columns 表中登记一个新列，返回分配的 colID。
func (m *meta) registerColumn(name string, typ Kind) (int64, error) {
	id, err := m.getKV("next_col_id")
	if err != nil {
		return 0, err
	}
	tx, err := m.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO columns(col_id,name,type) VALUES(?,?,?)`, id, name, int(typ)); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE meta_kv SET value=? WHERE key='next_col_id'`, id+1); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// recordAppend 记录一次追加：对每列把 [startRow, startRow+n) 这一段
// 合并/新增到其 segments，并更新表行数。colIDs 为当前结构的列。
func (m *meta) recordAppend(colIDs []int64, startRow, n int64) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range colIDs {
		var seq, lastStart, lastCount int64
		err := tx.QueryRow(`
SELECT seq, start_row, count FROM segments
WHERE col_id=? ORDER BY seq DESC LIMIT 1`, id).Scan(&seq, &lastStart, &lastCount)
		switch {
		case err == sql.ErrNoRows:
			if _, err := tx.Exec(`INSERT INTO segments(col_id,seq,start_row,count) VALUES(?,0,?,?)`, id, startRow, n); err != nil {
				return err
			}
		case err != nil:
			return err
		case lastStart+lastCount == startRow:
			// 与上一段连续，合并，保持段数最少
			if _, err := tx.Exec(`UPDATE segments SET count=? WHERE col_id=? AND seq=?`, lastCount+n, id, seq); err != nil {
				return err
			}
		default:
			if _, err := tx.Exec(`INSERT INTO segments(col_id,seq,start_row,count) VALUES(?,?,?,?)`, id, seq+1, startRow, n); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`UPDATE meta_kv SET value=? WHERE key='row_count'`, startRow+n); err != nil {
		return err
	}
	return tx.Commit()
}

// segmentsFor 返回一列的全部段（按追加顺序）。
func (m *meta) segmentsFor(colID int64) ([]segment, error) {
	rows, err := m.db.Query(`SELECT start_row, count FROM segments WHERE col_id=? ORDER BY seq`, colID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []segment
	for rows.Next() {
		var s segment
		if err := rows.Scan(&s.StartRow, &s.Count); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// fileRowCounts 返回每列数据文件中已有的记录数（各段 count 之和）。
func (m *meta) fileRowCounts() (map[int64]int64, error) {
	rows, err := m.db.Query(`SELECT col_id, COALESCE(SUM(count),0) FROM segments GROUP BY col_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// fileRowCount 返回一列数据文件中已有的记录数。
func (m *meta) fileRowCount(colID int64) (int64, error) {
	var n int64
	err := m.db.QueryRow(`SELECT COALESCE(SUM(count),0) FROM segments WHERE col_id=?`, colID).Scan(&n)
	return n, err
}
