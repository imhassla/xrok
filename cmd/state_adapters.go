package cmd

// Optional, dependency-free RuntimeStateStore adapters.  Redis speaks the
// small subset of RESP needed here; PostgreSQL uses database/sql so callers
// can bring any driver they already use (no driver is linked by xrok).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

type RedisStateStore struct {
	Addr, Password, Prefix string
	Timeout                time.Duration
}

func NewRedisStateStore(addr string) *RedisStateStore {
	return &RedisStateStore{Addr: addr, Prefix: "xrok:state:", Timeout: 3 * time.Second}
}
func (s *RedisStateStore) Save(ctx context.Context, st RuntimeState) error {
	if st.ID == "" {
		return fmt.Errorf("runtime state id is required")
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = s.command(ctx, "SET", s.Prefix+st.ID, string(b))
	return err
}
func (s *RedisStateStore) Load(ctx context.Context, id string) (RuntimeState, bool, error) {
	v, err := s.command(ctx, "GET", s.Prefix+id)
	if err != nil {
		return RuntimeState{}, false, err
	}
	if v == "" {
		return RuntimeState{}, false, nil
	}
	var st RuntimeState
	err = json.Unmarshal([]byte(v), &st)
	return st, err == nil, err
}
func (s *RedisStateStore) command(ctx context.Context, args ...string) (string, error) {
	if s.Addr == "" {
		return "", fmt.Errorf("redis address is required")
	}
	d := net.Dialer{Timeout: s.Timeout}
	c, err := d.DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return "", err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if s.Password != "" {
		if _, err = resp(c, "AUTH", s.Password); err != nil {
			return "", err
		}
	}
	return resp(c, args...)
}
func resp(c net.Conn, args ...string) (string, error) {
	var b []byte
	b = append(b, '*')
	b = append(b, []byte(strconv.Itoa(len(args)))...)
	b = append(b, '\r', '\n')
	for _, a := range args {
		b = append(b, '$')
		b = append(b, []byte(strconv.Itoa(len(a)))...)
		b = append(b, '\r', '\n')
		b = append(b, []byte(a)...)
		b = append(b, '\r', '\n')
	}
	if _, e := c.Write(b); e != nil {
		return "", e
	}
	h := make([]byte, 1)
	if _, e := io.ReadFull(c, h); e != nil {
		return "", e
	}
	if h[0] == '-' {
		return "", fmt.Errorf("redis error")
	}
	line := make([]byte, 256)
	n := 0
	for n < len(line) {
		if _, e := io.ReadFull(c, line[n:n+1]); e != nil {
			return "", e
		}
		if n > 0 && line[n-1] == '\r' && line[n] == '\n' {
			break
		}
		n++
	}
	if h[0] == '$' {
		l, _ := strconv.Atoi(string(line[:n-1]))
		if l < 0 {
			return "", nil
		}
		v := make([]byte, l+2)
		_, e := io.ReadFull(c, v)
		return string(v[:l]), e
	}
	return string(line[:n-1]), nil
}

type PostgresStateStore struct {
	DB    *sql.DB
	Table string
}

func NewPostgresStateStore(db *sql.DB) *PostgresStateStore {
	return &PostgresStateStore{DB: db, Table: "xrok_runtime_state"}
}
func (s *PostgresStateStore) EnsureSchema(ctx context.Context) error {
	if s.DB == nil {
		return fmt.Errorf("postgres db is nil")
	}
	_, e := s.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS xrok_runtime_state (id TEXT PRIMARY KEY,status TEXT NOT NULL,updated_at TIMESTAMPTZ NOT NULL)`)
	return e
}
func (s *PostgresStateStore) Save(ctx context.Context, st RuntimeState) error {
	if s.DB == nil {
		return fmt.Errorf("postgres db is nil")
	}
	if st.ID == "" {
		return fmt.Errorf("runtime state id is required")
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}
	_, e := s.DB.ExecContext(ctx, `INSERT INTO xrok_runtime_state(id,status,updated_at) VALUES($1,$2,$3) ON CONFLICT(id) DO UPDATE SET status=EXCLUDED.status,updated_at=EXCLUDED.updated_at`, st.ID, st.Status, st.UpdatedAt)
	return e
}
func (s *PostgresStateStore) Load(ctx context.Context, id string) (RuntimeState, bool, error) {
	if s.DB == nil {
		return RuntimeState{}, false, fmt.Errorf("postgres db is nil")
	}
	var st RuntimeState
	e := s.DB.QueryRowContext(ctx, `SELECT id,status,updated_at FROM xrok_runtime_state WHERE id=$1`, id).Scan(&st.ID, &st.Status, &st.UpdatedAt)
	if e == sql.ErrNoRows {
		return RuntimeState{}, false, nil
	}
	return st, e == nil, e
}
