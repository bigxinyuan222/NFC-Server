package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var fakeDriverID uint64

type fakeLoginState struct {
	values       []driver.Value
	pointValues  []driver.Value
	checkinCount int64
	checkinRows  [][]driver.Value
	checkinErr   error
	err          error
	execErr      error
	execCalls    int
	insertID     int64
}

type fakeLoginDriver struct {
	state *fakeLoginState
}

func (d *fakeLoginDriver) Open(string) (driver.Conn, error) {
	return &fakeLoginConn{state: d.state}, nil
}

type fakeLoginConn struct {
	state *fakeLoginState
}

func (c *fakeLoginConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported by fake driver")
}

func (c *fakeLoginConn) Close() error { return nil }

func (c *fakeLoginConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported by fake driver")
}

func (c *fakeLoginConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "checkin_log") {
		if c.state.checkinErr != nil {
			return nil, c.state.checkinErr
		}
		if strings.Contains(query, "COUNT(*)") {
			return &fakeLoginRows{
				columns: []string{"COUNT(*)"},
				records: [][]driver.Value{{c.state.checkinCount}},
			}, nil
		}
		return &fakeLoginRows{
			columns: []string{"id", "point_id", "created_at"},
			records: c.state.checkinRows,
		}, nil
	}
	if c.state.err != nil {
		return nil, c.state.err
	}
	values := c.state.values
	columns := []string{"id", "name", "department", "password_hash", "status"}
	if strings.Contains(query, "checkin_point") {
		values = c.state.pointValues
		columns = []string{"status"}
	}
	return &fakeLoginRows{
		columns: columns,
		records: func() [][]driver.Value {
			if len(values) == 0 {
				return nil
			}
			return [][]driver.Value{values}
		}(),
	}, nil
}

func (c *fakeLoginConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	if c.state.execErr != nil {
		return nil, c.state.execErr
	}
	c.state.execCalls++
	insertID := c.state.insertID
	if insertID == 0 {
		insertID = 1
	}
	return fakeDriverResult{insertID: insertID}, nil
}

type fakeDriverResult struct {
	insertID int64
}

func (r fakeDriverResult) LastInsertId() (int64, error) { return r.insertID, nil }

func (r fakeDriverResult) RowsAffected() (int64, error) { return 1, nil }

type fakeLoginRows struct {
	columns []string
	records [][]driver.Value
	index   int
}

func (r *fakeLoginRows) Columns() []string { return r.columns }

func (r *fakeLoginRows) Close() error { return nil }

func (r *fakeLoginRows) Next(dest []driver.Value) error {
	if r.index >= len(r.records) {
		return io.EOF
	}
	copy(dest, r.records[r.index])
	r.index++
	return nil
}

func newFakeLoginDB(t *testing.T, values []driver.Value, queryErr error) *sql.DB {
	return newFakeDB(t, &fakeLoginState{values: values, err: queryErr})
}

func newFakeDB(t *testing.T, state *fakeLoginState) *sql.DB {
	t.Helper()
	name := "checkin-test-" + strconv.FormatUint(atomic.AddUint64(&fakeDriverID, 1), 10)
	driverName := name
	sql.Register(driverName, &fakeLoginDriver{state: state})
	db, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestLoginCredentials(t *testing.T) {
	passwordHash, err := hashPassword("correct-password")
	if err != nil {
		t.Fatalf("hashPassword() error = %v", err)
	}

	oldDB, oldSecret := db, jwtSecret
	t.Cleanup(func() {
		db = oldDB
		jwtSecret = oldSecret
	})
	jwtSecret = []byte("test-secret-that-is-at-least-32-bytes")

	tests := []struct {
		name       string
		values     []driver.Value
		queryErr   error
		password   string
		wantStatus int
	}{
		{
			name:       "valid credentials issue token",
			values:     []driver.Value{int64(7), "alice", "engineering", passwordHash, true},
			password:   "correct-password",
			wantStatus: http.StatusOK,
		},
		{
			name:       "wrong password is unauthorized",
			values:     []driver.Value{int64(7), "alice", "engineering", passwordHash, true},
			password:   "wrong-password",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "disabled user is unauthorized",
			values:     []driver.Value{int64(7), "alice", "engineering", passwordHash, false},
			password:   "correct-password",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "unknown user is unauthorized",
			password:   "correct-password",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "database error is internal error",
			queryErr:   errors.New("database unavailable"),
			password:   "correct-password",
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db = newFakeLoginDB(t, tt.values, tt.queryErr)
			req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"userName":"alice","password":"`+tt.password+`"}`))
			response := httptest.NewRecorder()

			login(response, req)
			if response.Code != tt.wantStatus {
				t.Fatalf("login() status = %d, want %d; body = %s", response.Code, tt.wantStatus, response.Body.String())
			}

			if tt.wantStatus != http.StatusOK {
				return
			}
			var payload struct {
				Data struct {
					Token string `json:"token"`
				} `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode login response: %v", err)
			}
			if payload.Data.Token == "" {
				t.Fatal("login response did not include a token")
			}
			requestWithToken := httptest.NewRequest(http.MethodPost, "/api/checkin", nil)
			requestWithToken.Header.Set("Authorization", "Bearer "+payload.Data.Token)
			if got, ok := userIDFromReq(requestWithToken); !ok || got != 7 {
				t.Fatalf("userIDFromReq() = (%d, %v), want (7, true)", got, ok)
			}
		})
	}
}

func TestLoginRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name   string
		method string
		body   string
	}{
		{name: "method must be post", method: http.MethodGet, body: "{}"},
		{name: "malformed json", method: http.MethodPost, body: "{"},
		{name: "missing password", method: http.MethodPost, body: `{"userName":"alice"}`},
		{name: "unknown field", method: http.MethodPost, body: `{"userName":"alice","password":"secret","department":"engineering"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			login(response, httptest.NewRequest(tt.method, "/api/login", strings.NewReader(tt.body)))
			if response.Code != http.StatusBadRequest && tt.method != http.MethodGet {
				t.Fatalf("login() status = %d, want %d", response.Code, http.StatusBadRequest)
			}
			if tt.method == http.MethodGet && response.Code != http.StatusMethodNotAllowed {
				t.Fatalf("login() status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}

func TestRegisterCreatesUserAndToken(t *testing.T) {
	oldDB, oldSecret := db, jwtSecret
	t.Cleanup(func() {
		db = oldDB
		jwtSecret = oldSecret
	})
	jwtSecret = []byte("test-secret-that-is-at-least-32-bytes")
	state := &fakeLoginState{insertID: 11}
	db = newFakeDB(t, state)
	req := httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(`{"userName":"alice","password":"correct-password","department":"engineering"}`))
	response := httptest.NewRecorder()

	register(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("register() status = %d, want %d; body = %s", response.Code, http.StatusCreated, response.Body.String())
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Token      string `json:"token"`
			UserID     int64  `json:"userId"`
			UserName   string `json:"userName"`
			Department string `json:"department"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if payload.Code != 0 || payload.Data.Token == "" || payload.Data.UserID != 11 || payload.Data.UserName != "alice" || payload.Data.Department != "engineering" {
		t.Fatalf("unexpected register response: %s", response.Body.String())
	}
	requestWithToken := httptest.NewRequest(http.MethodPost, "/api/checkin", nil)
	requestWithToken.Header.Set("Authorization", "Bearer "+payload.Data.Token)
	if got, ok := userIDFromReq(requestWithToken); !ok || got != 11 {
		t.Fatalf("userIDFromReq() = (%d, %v), want (11, true)", got, ok)
	}
	if state.execCalls != 1 {
		t.Fatalf("register() writes = %d, want 1", state.execCalls)
	}
}

func TestRegisterRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		body       string
		wantStatus int
	}{
		{name: "method must be post", method: http.MethodGet, body: `{}`, wantStatus: http.StatusMethodNotAllowed},
		{name: "malformed json", method: http.MethodPost, body: `{`, wantStatus: http.StatusBadRequest},
		{name: "missing department", method: http.MethodPost, body: `{"userName":"alice","password":"secret"}`, wantStatus: http.StatusBadRequest},
		{name: "blank department", method: http.MethodPost, body: `{"userName":"alice","password":"secret","department":"   "}`, wantStatus: http.StatusBadRequest},
		{name: "unknown field", method: http.MethodPost, body: `{"userName":"alice","password":"secret","department":"engineering","extra":true}`, wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldDB := db
			t.Cleanup(func() { db = oldDB })
			state := &fakeLoginState{}
			db = newFakeDB(t, state)
			response := httptest.NewRecorder()
			register(response, httptest.NewRequest(tt.method, "/api/register", strings.NewReader(tt.body)))
			if response.Code != tt.wantStatus {
				t.Fatalf("register() status = %d, want %d; body = %s", response.Code, tt.wantStatus, response.Body.String())
			}
			if state.execCalls != 0 {
				t.Fatalf("invalid register request wrote %d rows", state.execCalls)
			}
		})
	}
}

func TestRegisterRejectsDuplicateAndDatabaseErrors(t *testing.T) {
	oldDB, oldSecret := db, jwtSecret
	t.Cleanup(func() {
		db = oldDB
		jwtSecret = oldSecret
	})
	jwtSecret = []byte("test-secret-that-is-at-least-32-bytes")
	for _, tt := range []struct {
		name       string
		execErr    error
		wantStatus int
	}{
		{name: "duplicate user", execErr: errors.New("Duplicate entry 'alice' for key 'uk_name'"), wantStatus: http.StatusConflict},
		{name: "database error", execErr: errors.New("database unavailable"), wantStatus: http.StatusInternalServerError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state := &fakeLoginState{execErr: tt.execErr}
			db = newFakeDB(t, state)
			body := `{"userName":"alice","password":"secret","department":"engineering"}`
			response := httptest.NewRecorder()
			register(response, httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(body)))
			if response.Code != tt.wantStatus {
				t.Fatalf("register() status = %d, want %d; body = %s", response.Code, tt.wantStatus, response.Body.String())
			}
		})
	}
}

func TestUserIDFromReqRejectsWrongIssuer(t *testing.T) {
	oldSecret := jwtSecret
	t.Cleanup(func() { jwtSecret = oldSecret })
	jwtSecret = []byte("test-secret-that-is-at-least-32-bytes")
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims{
		UID: 7,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "unexpected-issuer",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	signed, err := token.SignedString(jwtSecret)
	if err != nil {
		t.Fatalf("SignedString() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/checkin", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	if got, ok := userIDFromReq(req); ok || got != 0 {
		t.Fatalf("userIDFromReq() = (%d, %v), want (0, false)", got, ok)
	}
}

func TestCheckinCreatesRecordForKnownPoint(t *testing.T) {
	oldDB, oldSecret := db, jwtSecret
	t.Cleanup(func() {
		db = oldDB
		jwtSecret = oldSecret
	})
	jwtSecret = []byte("test-secret-that-is-at-least-32-bytes")
	token, err := genToken(7)
	if err != nil {
		t.Fatalf("genToken() error = %v", err)
	}

	const (
		pointID      = "office"
		whitelistUID = "5317C716430001"
	)
	tests := []struct {
		name       string
		point      []driver.Value
		uid        string
		wantStatus int
		wantWrites int
	}{
		{
			name:       "known available point with whitelisted uid is accepted",
			point:      []driver.Value{true},
			uid:        whitelistUID,
			wantStatus: http.StatusOK,
			wantWrites: 1,
		},
		{
			name:       "unknown point with whitelisted uid is rejected",
			uid:        whitelistUID,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "card uid not in whitelist is rejected",
			point:      []driver.Value{true},
			uid:        "5399C716430099",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "empty uid is rejected",
			point:      []driver.Value{true},
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &fakeLoginState{pointValues: tt.point}
			db = newFakeDB(t, state)
			body := `{"pointId":"` + pointID + `","uid":"` + tt.uid + `"}`
			req := httptest.NewRequest(http.MethodPost, "/api/checkin", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()

			checkin(response, req)
			if response.Code != tt.wantStatus {
				t.Fatalf("checkin() status = %d, want %d; body = %s", response.Code, tt.wantStatus, response.Body.String())
			}
			if state.execCalls != tt.wantWrites {
				t.Fatalf("checkin() writes = %d, want %d", state.execCalls, tt.wantWrites)
			}
		})
	}
}

func TestMyCheckinsReturnsOnlyAuthenticatedUserRecords(t *testing.T) {
	oldDB, oldSecret := db, jwtSecret
	t.Cleanup(func() {
		db = oldDB
		jwtSecret = oldSecret
	})
	jwtSecret = []byte("test-secret-that-is-at-least-32-bytes")
	token, err := genToken(7)
	if err != nil {
		t.Fatalf("genToken() error = %v", err)
	}
	createdAt := time.Date(2026, 9, 29, 10, 30, 0, 0, time.UTC)
	state := &fakeLoginState{
		checkinCount: 2,
		checkinRows: [][]driver.Value{
			{int64(20), "point-001", createdAt},
			{int64(19), "point-001", createdAt.Add(-time.Hour)},
		},
	}
	db = newFakeDB(t, state)
	req := httptest.NewRequest(http.MethodGet, "/api/checkins/me?page=1&pageSize=20&userId=999", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()

	myCheckins(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("myCheckins() status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Items    []checkinRecord `json:"items"`
			Page     int             `json:"page"`
			PageSize int             `json:"pageSize"`
			Total    int64           `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode myCheckins response: %v", err)
	}
	if payload.Code != 0 || payload.Data.Total != 2 || payload.Data.Page != 1 || payload.Data.PageSize != 20 || len(payload.Data.Items) != 2 {
		t.Fatalf("unexpected myCheckins response: %s", response.Body.String())
	}
	if payload.Data.Items[0].ID != 20 || payload.Data.Items[0].PointID != "point-001" {
		t.Fatalf("unexpected first record: %+v", payload.Data.Items[0])
	}
	if payload.Data.Items[0].CreatedAt != "2026-09-29 10:30:00" {
		t.Fatalf("createdAt = %q, want %q", payload.Data.Items[0].CreatedAt, "2026-09-29 10:30:00")
	}
	if strings.Contains(response.Body.String(), "T10:30:00Z") || strings.Contains(response.Body.String(), "createdAt\":\"2026-09-29T") {
		t.Fatalf("createdAt response still contains ISO-8601 UTC suffix: %s", response.Body.String())
	}
}

func TestMyCheckinsRejectsUnauthorizedAndInvalidPagination(t *testing.T) {
	oldDB, oldSecret := db, jwtSecret
	t.Cleanup(func() {
		db = oldDB
		jwtSecret = oldSecret
	})
	jwtSecret = []byte("test-secret-that-is-at-least-32-bytes")
	for _, tt := range []struct {
		name       string
		path       string
		withToken  bool
		wantStatus int
	}{
		{name: "missing token", path: "/api/checkins/me", wantStatus: http.StatusUnauthorized},
		{name: "invalid page", path: "/api/checkins/me?page=0", withToken: true, wantStatus: http.StatusBadRequest},
		{name: "invalid page size", path: "/api/checkins/me?pageSize=101", withToken: true, wantStatus: http.StatusBadRequest},
		{name: "wrong method", path: "/api/checkins/me", withToken: true, wantStatus: http.StatusMethodNotAllowed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db = newFakeDB(t, &fakeLoginState{})
			method := http.MethodGet
			if tt.name == "wrong method" {
				method = http.MethodPost
			}
			req := httptest.NewRequest(method, tt.path, nil)
			if tt.withToken {
				token, err := genToken(7)
				if err != nil {
					t.Fatalf("genToken() error = %v", err)
				}
				req.Header.Set("Authorization", "Bearer "+token)
			}
			response := httptest.NewRecorder()
			myCheckins(response, req)
			if response.Code != tt.wantStatus {
				t.Fatalf("myCheckins() status = %d, want %d; body = %s", response.Code, tt.wantStatus, response.Body.String())
			}
		})
	}
}
