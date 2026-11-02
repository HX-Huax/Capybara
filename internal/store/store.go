// Package store 通过 SQLite 事务维护状态与 GPU 分配。
package store

import (
	"capybara/internal/model"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"time"
)

// Store 使用单连接串行化写入；事务相当于保护状态转换的临界区。
type Store struct{ db *sql.DB }

// Open 初始化数据库。分配表的唯一约束阻止同一张卡被重复分配。
func Open(path string) (*Store, error) {
	db, e := sql.Open("sqlite", path)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS jobs(id INTEGER PRIMARY KEY AUTOINCREMENT,state TEXT NOT NULL,payload BLOB NOT NULL,env BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS allocations(uuid TEXT PRIMARY KEY,job_id INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS jobs_state ON jobs(state,id);`)
	if e != nil {
		db.Close()
		return nil, e
	}
	return &Store{db}, nil
}

// Close 释放数据库连接。
func (s *Store) Close() error { return s.db.Close() }

// Create 写入新作业并取得自增 ID。
func (s *Store) Create(j *model.Job) error {
	j.State = "pending"
	j.SubmitAt = time.Now().UTC()
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	env, e := json.Marshal(j.Env)
	if e != nil {
		return e
	}
	r, e := s.db.Exec("INSERT INTO jobs(state,payload,env) VALUES(?,?,?)", j.State, b, env)
	if e != nil {
		return e
	}
	j.ID, e = r.LastInsertId()
	return e
}

// List 返回按提交顺序排列的快照，环境变量仅在内部保留。
func (s *Store) List() ([]model.Job, error) {
	rows, e := s.db.Query("SELECT id,payload,env FROM jobs ORDER BY id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []model.Job{}
	for rows.Next() {
		var j model.Job
		var b, env []byte
		var id int64
		if e = rows.Scan(&id, &b, &env); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &j); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(env, &j.Env); e != nil {
			return nil, e
		}
		j.ID = id
		out = append(out, j)
	}
	return out, rows.Err()
}

// Get 返回单个作业。
func (s *Store) Get(id int64) (model.Job, error) {
	var j model.Job
	var b, env []byte
	e := s.db.QueryRow("SELECT payload,env FROM jobs WHERE id=?", id).Scan(&b, &env)
	if e != nil {
		return j, e
	}
	if e = json.Unmarshal(b, &j); e != nil {
		return j, e
	}
	e = json.Unmarshal(env, &j.Env)
	j.ID = id
	return j, e
}

// Transition 采用预期状态检查，避免取消与调度并发时覆盖彼此的结果。
func (s *Store) Transition(j model.Job, from string) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	r, e := tx.Exec("UPDATE jobs SET state=?,payload=? WHERE id=? AND state=?", j.State, b, j.ID, from)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return errors.New("作业状态已改变")
	}
	if _, e = tx.Exec("DELETE FROM allocations WHERE job_id=?", j.ID); e != nil {
		return e
	}
	if j.State == "running" {
		for _, uuid := range j.GPUs {
			if _, e = tx.Exec("INSERT INTO allocations(uuid,job_id) VALUES(?,?)", uuid, j.ID); e != nil {
				return fmt.Errorf("GPU 分配冲突: %w", e)
			}
		}
	}
	return tx.Commit()
}
