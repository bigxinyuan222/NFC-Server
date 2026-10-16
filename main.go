package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/golang-jwt/jwt/v5"
)

//go:embed openapi.json
var openapiSpec []byte

var (
	db               *sql.DB
	jwtSecret        []byte
	dingTalkUploader DingTalkUploader
)

const jwtIssuer = "checkin-server"

const defaultMySQLLocation = "UTC"

var checkinLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

type accessClaims struct {
	UID int64 `json:"uid"`
	jwt.RegisteredClaims
}

type checkinReq struct {
	PointID string `json:"pointId"`
	UID     string `json:"uid"`
}

// checkinCardWhitelist 打卡卡片 UID 白名单(前端刷卡读到的卡号,统一大写)
var checkinCardWhitelist = map[string]bool{
	"5317C716430001": true,
	"5318C716430001": true,
	"5319C716430001": true,
	"530EC716430001": true,
	"530FC716430001": true,
	"5310C716430001": true,
}

type checkinRecord struct {
	ID        int64  `json:"id"`
	PointID   string `json:"pointId"`
	CreatedAt string `json:"createdAt"`
}

const checkinTimeFormat = "2006-01-02 15:04:05"

type loginReq struct {
	UserName string `json:"userName"`
	Password string `json:"password"`
}

type registerReq struct {
	UserName       string `json:"userName"`
	Password       string `json:"password"`
	Department     string `json:"department"`
	Mobile         string `json:"mobile"`
	DingTalkUserID string `json:"dingTalkUserId"`
}

type changePasswordReq struct {
	OldPassword string `json:"oldPassword"`
	NewPassword string `json:"newPassword"`
}

func genToken(uid int64) (string, error) {
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims{
		UID: uid,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    jwtIssuer,
			Subject:   fmt.Sprintf("%d", uid),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(72 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	})
	return t.SignedString(jwtSecret)
}

func userIDFromReq(r *http.Request) (int64, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return 0, false
	}
	tk := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	if tk == "" {
		return 0, false
	}
	claims := &accessClaims{}
	t, err := jwt.ParseWithClaims(tk, claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, jwt.ErrTokenSignatureInvalid
		}
		return jwtSecret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithIssuer(jwtIssuer))
	if err != nil || !t.Valid {
		return 0, false
	}
	if claims.UID <= 0 {
		return 0, false
	}
	return claims.UID, true
}

func login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": 405, "msg": "方法不允许"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req loginReq
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 400, "msg": "请求体格式错误或包含未知字段"})
		return
	}
	req.UserName = strings.TrimSpace(req.UserName)
	if req.UserName == "" || req.Password == "" || len(req.UserName) > 64 || len(req.Password) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 1, "msg": "用户名或密码格式错误"})
		return
	}
	var id int64
	var name, department, passwordHash string
	var status bool
	err := db.QueryRowContext(r.Context(), "SELECT id, name, department, password_hash, status FROM user WHERE name=?", req.UserName).
		Scan(&id, &name, &department, &passwordHash, &status)
	if err != nil && err != sql.ErrNoRows {
		log.Println("query user failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "查询失败"})
		return
	}
	if err == sql.ErrNoRows || !status || !verifyPassword(passwordHash, req.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"code": 401, "msg": "用户名或密码错误"})
		return
	}
	tk, err := genToken(id)
	if err != nil {
		log.Println("sign token failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "签发token失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "msg": "登录成功", "data": map[string]any{"token": tk, "userId": id, "userName": name, "department": department}})
}

func register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": 405, "msg": "方法不允许"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req registerReq
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 400, "msg": "请求体格式错误或包含未知字段"})
		return
	}
	req.UserName = strings.TrimSpace(req.UserName)
	req.Department = strings.TrimSpace(req.Department)
	req.Mobile = strings.TrimSpace(req.Mobile)
	req.DingTalkUserID = strings.TrimSpace(req.DingTalkUserID)
	if req.UserName == "" || req.Password == "" || req.Department == "" || len(req.UserName) > 64 || len(req.Password) > 128 || len(req.Department) > 64 || len(req.Mobile) > 32 || len(req.DingTalkUserID) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 1, "msg": "用户名、密码或部门格式错误"})
		return
	}
	hash, err := hashPassword(req.Password)
	if err != nil {
		log.Println("hash password failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "注册失败"})
		return
	}
	result, err := db.ExecContext(r.Context(), "INSERT INTO user(name, department, password_hash, mobile, dingtalk_userid, status) VALUES(?,?,?,?,?,1)", req.UserName, req.Department, hash, nullableString(req.Mobile), nullableString(req.DingTalkUserID))
	if err != nil {
		if isDuplicateUserError(err) {
			writeJSON(w, http.StatusConflict, map[string]any{"code": 409, "msg": "用户名已存在"})
			return
		}
		log.Println("insert user failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "注册失败"})
		return
	}
	id, err := result.LastInsertId()
	if err != nil || id <= 0 {
		log.Println("get inserted user id failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "注册失败"})
		return
	}
	tk, err := genToken(id)
	if err != nil {
		log.Println("sign token failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "签发token失败"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"code": 0, "msg": "注册成功", "data": map[string]any{"token": tk, "userId": id, "userName": req.UserName, "department": req.Department}})
}

func changePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": 405, "msg": "方法不允许"})
		return
	}
	uid, ok := userIDFromReq(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"code": 401, "msg": "未登录"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req changePasswordReq
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 400, "msg": "请求体格式错误或包含未知字段"})
		return
	}
	req.OldPassword = strings.TrimSpace(req.OldPassword)
	req.NewPassword = strings.TrimSpace(req.NewPassword)
	if req.OldPassword == "" || req.NewPassword == "" || len(req.NewPassword) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 1, "msg": "旧密码或新密码格式错误"})
		return
	}
	if req.OldPassword == req.NewPassword {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 1, "msg": "新密码不能与旧密码相同"})
		return
	}
	var passwordHash string
	err := db.QueryRowContext(r.Context(), "SELECT password_hash FROM user WHERE id=?", uid).Scan(&passwordHash)
	if err != nil {
		log.Println("query user password failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "查询用户失败"})
		return
	}
	if !verifyPassword(passwordHash, req.OldPassword) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"code": 401, "msg": "旧密码错误"})
		return
	}
	newHash, err := hashPassword(req.NewPassword)
	if err != nil {
		log.Println("hash new password failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "修改密码失败"})
		return
	}
	if _, err := db.ExecContext(r.Context(), "UPDATE user SET password_hash=? WHERE id=?", newHash, uid); err != nil {
		log.Println("update password failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "修改密码失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "msg": "密码修改成功"})
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.TrimSpace(value)
}

func isDuplicateUserError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1062
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate") || strings.Contains(strings.ToLower(err.Error()), "already exists")
}

func checkin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": 405, "msg": "方法不允许"})
		return
	}
	uid, ok := userIDFromReq(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"code": 401, "msg": "未登录"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req checkinReq
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 400, "msg": "请求体格式错误或包含未知字段"})
		return
	}
	req.PointID = strings.TrimSpace(req.PointID)
	req.UID = strings.ToUpper(strings.TrimSpace(req.UID))
	if !checkinCardWhitelist[req.UID] {
		writeJSON(w, http.StatusForbidden, map[string]any{"code": 403, "msg": "未授权的卡片"})
		return
	}
	if req.PointID == "" || len(req.PointID) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 1, "msg": "签到点无效"})
		return
	}
	var pointStatus bool
	err := db.QueryRowContext(r.Context(), "SELECT status FROM checkin_point WHERE id=?", req.PointID).
		Scan(&pointStatus)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "msg": "签到点不存在"})
		return
	}
	if err != nil {
		log.Println("query checkin point failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "查询签到点失败"})
		return
	}
	if !pointStatus {
		writeJSON(w, http.StatusConflict, map[string]any{"code": 409, "msg": "签到点不可用"})
		return
	}
	checkTime := time.Now().In(checkinLocation)
	result, err := db.ExecContext(r.Context(), "INSERT INTO checkin_log(user_id, point_id, created_at) VALUES(?,?,?)",
		uid, req.PointID, checkTime)
	if err != nil {
		log.Println("insert failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "打卡失败"})
		return
	}
	checkinID, err := result.LastInsertId()
	if err != nil {
		log.Println("get checkin id failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "打卡失败"})
		return
	}
	dingTalkSync, syncErr := queueDingTalkUpload(r.Context(), checkinID, uid, checkTime, dingTalkUploader)
	if syncErr != nil {
		log.Println("queue dingtalk upload failed:", syncErr)
		dingTalkSync = "queue_failed"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code": 0,
		"msg":  "打卡成功",
		"data": map[string]any{
			"dingtalkSync": dingTalkSync,
		},
	})
}

func parsePagination(r *http.Request) (page, pageSize int, err error) {
	page, pageSize = 1, 20
	query := r.URL.Query()
	if raw := strings.TrimSpace(query.Get("page")); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 || page > 1000000 {
			return 0, 0, errors.New("page must be between 1 and 1000000")
		}
	}
	if raw := strings.TrimSpace(query.Get("pageSize")); raw != "" {
		pageSize, err = strconv.Atoi(raw)
		if err != nil || pageSize < 1 || pageSize > 100 {
			return 0, 0, errors.New("pageSize must be between 1 and 100")
		}
	}
	return page, pageSize, nil
}

// myCheckins returns only the records belonging to the authenticated user.
// The user ID is always taken from the JWT; no userId query parameter is accepted.
func myCheckins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": 405, "msg": "方法不允许"})
		return
	}
	uid, ok := userIDFromReq(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"code": 401, "msg": "未登录"})
		return
	}
	page, pageSize, err := parsePagination(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 400, "msg": err.Error()})
		return
	}
	offset := int64(page-1) * int64(pageSize)

	var total int64
	if err := db.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM checkin_log WHERE user_id=?", uid).Scan(&total); err != nil {
		log.Println("count user checkins failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "查询签到记录失败"})
		return
	}

	rows, err := db.QueryContext(r.Context(), `
		SELECT id, point_id, created_at
		FROM checkin_log
		WHERE user_id=?
		ORDER BY created_at DESC, id DESC
		LIMIT ? OFFSET ?`, uid, pageSize, offset)
	if err != nil {
		log.Println("query user checkins failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "查询签到记录失败"})
		return
	}
	defer rows.Close()

	items := make([]checkinRecord, 0)
	for rows.Next() {
		var item checkinRecord
		var createdAt time.Time
		if err := rows.Scan(&item.ID, &item.PointID, &createdAt); err != nil {
			log.Println("scan user checkin failed:", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "读取签到记录失败"})
			return
		}
		item.CreatedAt = createdAt.In(checkinLocation).Format(checkinTimeFormat)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		log.Println("iterate user checkins failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "读取签到记录失败"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"code": 0,
		"msg":  "查询成功",
		"data": map[string]any{
			"items":    items,
			"page":     page,
			"pageSize": pageSize,
			"total":    total,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ensureMySQLLocation makes DATETIME parsing deterministic. MySQL DATETIME has
// no timezone, so the driver stores and reads instants as UTC; API responses
// convert them to China Standard Time explicitly.
func ensureMySQLLocation(dsn string) string {
	queryStart := strings.IndexByte(dsn, '?')
	if queryStart < 0 {
		return dsn + "?loc=" + defaultMySQLLocation
	}
	query := dsn[queryStart+1:]
	parameters := strings.Split(query, "&")
	for i, parameter := range parameters {
		key, _, _ := strings.Cut(parameter, "=")
		if strings.EqualFold(strings.TrimSpace(key), "loc") {
			parameters[i] = "loc=" + defaultMySQLLocation
			return dsn[:queryStart+1] + strings.Join(parameters, "&")
		}
	}
	if query == "" {
		return dsn + "loc=" + defaultMySQLLocation
	}
	return dsn + "&loc=" + defaultMySQLLocation
}

func serveSpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(openapiSpec)
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "hash-password" {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 129))
		if err != nil {
			log.Fatal("read password failed:", err)
		}
		password := strings.TrimRight(string(data), "\r\n")
		if password == "" || len(password) > 128 {
			log.Fatal("password must be between 1 and 128 bytes")
		}
		hash, err := hashPassword(password)
		if err != nil {
			log.Fatal("hash password failed:", err)
		}
		fmt.Println(hash)
		return
	}
	secret := strings.TrimSpace(os.Getenv("JWT_SECRET"))
	if len(secret) < 32 {
		log.Fatal("JWT_SECRET must be at least 32 characters")
	}
	jwtSecret = []byte(secret)
	dsn := strings.TrimSpace(os.Getenv("DB_DSN"))
	if dsn == "" {
		log.Fatal("DB_DSN is required")
	}
	dsn = ensureMySQLLocation(dsn)
	var err error
	db, err = sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal("open db failed:", err)
	}
	if err = db.Ping(); err != nil {
		log.Fatal("ping db failed:", err)
	}
	dingTalkUploader = newDingTalkClientFromEnv()
	if len(os.Args) == 2 && os.Args[1] == "sync-dingtalk-users" {
		client, ok := dingTalkUploader.(*DingTalkClient)
		if !ok || client == nil {
			log.Fatal("DingTalk credentials are required for sync-dingtalk-users")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		stats, syncErr := importDingTalkUsers(ctx, client)
		if syncErr != nil {
			log.Fatal("sync DingTalk users failed:", syncErr)
		}
		fmt.Printf("DingTalk users synced: fetched=%d inserted=%d updated=%d skipped=%d\n", stats.Fetched, stats.Inserted, stats.Updated, stats.Skipped)
		return
	}
	if dingTalkClient, ok := dingTalkUploader.(*DingTalkClient); ok && strings.EqualFold(envOrDefault("DINGTALK_SYNC_USERS_ON_START", "true"), "true") {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			updated, syncErr := syncDingTalkUserMappings(ctx, dingTalkClient)
			if syncErr != nil {
				log.Println("sync dingtalk user mappings failed:", syncErr)
				return
			}
			if updated > 0 {
				log.Printf("synced %d dingtalk user mappings", updated)
			}
		}()
	}
	startDingTalkWorker(context.Background(), dingTalkUploader)
	http.HandleFunc("/api/login", login)
	http.HandleFunc("/api/register", register)
	http.HandleFunc("/api/change-password", changePassword)
	http.HandleFunc("/api/checkin", checkin)
	http.HandleFunc("/api/checkins/me", myCheckins)
	http.HandleFunc("/openapi.json", serveSpec)
	listenAddr := envOrDefault("LISTEN_ADDR", "0.0.0.0:8080")
	log.Println("checkin server on", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}
