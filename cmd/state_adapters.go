package cmd

// Optional, dependency-free RuntimeStateStore adapters.  Redis speaks the
// small subset of RESP needed here; PostgreSQL uses database/sql so callers
// can bring any driver they already use (no driver is linked by xrok).

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

const (
	maxRedisResponseLine = 64 << 10
	maxRedisBulkResponse = 16 << 20
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
	if id == "" {
		return RuntimeState{}, false, fmt.Errorf("runtime state id is required")
	}
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
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return "", err
	}
	defer c.Close()
	deadline := time.Time{}
	deadline = time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && (deadline.IsZero() || dl.Before(deadline)) {
		deadline = dl
	}
	if !deadline.IsZero() {
		if err := c.SetDeadline(deadline); err != nil {
			return "", err
		}
	}
	stopCancel := context.AfterFunc(ctx, func() {
		_ = c.SetDeadline(time.Now())
	})
	defer stopCancel()
	r := bufio.NewReader(c)
	if s.Password != "" {
		if _, err = resp(c, r, "AUTH", s.Password); err != nil {
			return "", err
		}
	}
	return resp(c, r, args...)
}
func resp(c net.Conn, r *bufio.Reader, args ...string) (string, error) {
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
	h, e := r.ReadByte()
	if e != nil {
		return "", e
	}
	line, e := readRESPLine(r)
	if e != nil {
		return "", e
	}
	if h == '-' {
		return "", fmt.Errorf("redis error: %s", line)
	}
	if h == '$' {
		l, err := strconv.Atoi(line)
		if err != nil {
			return "", fmt.Errorf("invalid redis bulk length %q: %w", line, err)
		}
		if l < 0 {
			if l != -1 {
				return "", fmt.Errorf("invalid redis bulk length %d", l)
			}
			return "", nil
		}
		if l > maxRedisBulkResponse {
			return "", fmt.Errorf("redis bulk response exceeds %d bytes", maxRedisBulkResponse)
		}
		v := make([]byte, l+2)
		if _, err := io.ReadFull(r, v); err != nil {
			return "", err
		}
		if v[l] != '\r' || v[l+1] != '\n' {
			return "", fmt.Errorf("invalid redis bulk terminator")
		}
		return string(v[:l]), nil
	}
	if h != '+' && h != ':' {
		return "", fmt.Errorf("unsupported redis response type %q", h)
	}
	return line, nil
}

func readRESPLine(r *bufio.Reader) (string, error) {
	line := make([]byte, 0, 128)
	for {
		fragment, err := r.ReadSlice('\n')
		if len(line)+len(fragment) > maxRedisResponseLine {
			return "", fmt.Errorf("redis response line exceeds %d bytes", maxRedisResponseLine)
		}
		line = append(line, fragment...)
		if err == nil {
			break
		}
		if err != bufio.ErrBufferFull {
			return "", err
		}
	}
	if len(line) < 2 || line[len(line)-2] != '\r' || line[len(line)-1] != '\n' {
		return "", fmt.Errorf("invalid redis response line terminator")
	}
	return string(line[:len(line)-2]), nil
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
	if errors.Is(e, sql.ErrNoRows) {
		return RuntimeState{}, false, nil
	}
	return st, e == nil, e
}
