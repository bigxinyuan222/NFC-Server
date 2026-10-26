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
	db              *sql.DB
	jwtSecret       []byte
	dingTalkClients DingTalkClients
)

const jwtIssuer = "checkin-server"

const defaultMySQLLocation = "UTC"

var checkinLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

type accessClaims struct {
	UID int64 `json:"uid"`
	jwt.RegisteredClaims
}

type checkinReq struct {
	PointID  string `json:"pointId"`
	UID      string `json:"uid"`
	DeviceID string `json:"deviceId"`
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
	DingTalkOrg    string `json:"dingTalkOrg"`
}

type changePasswordReq struct {
	OldPassword string `json:"oldPassword"`
	NewPassword string `json:"newPassword"`
}

type dingTalkAuthReq struct {
	AuthCode string `json:"authCode"`
	State    string `json:"state"`
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

func dingTalkLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": 405, "msg": "方法不允许"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req dingTalkAuthReq
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 400, "msg": "请求体格式错误或包含未知字段"})
		return
	}
	req.AuthCode = strings.TrimSpace(req.AuthCode)
	req.State = strings.ToLower(strings.TrimSpace(req.State))
	if req.AuthCode == "" || len(req.AuthCode) > 1024 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 1, "msg": "authCode 格式错误"})
		return
	}
	if req.State != dataDingTalkOrg && req.State != eduDingTalkOrg {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 400, "msg": "state 必须是 org_lezhiedu 或 org_lezhidata"})
		return
	}
	client := dingTalkClients.clientFor(req.State)
	if client == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 400, "msg": "未知的钉钉组织"})
		return
	}
	if client.clientID == "" || client.clientSecret == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"code": 1, "msg": "该钉钉组织未配置登录凭证，请联系管理员"})
		return
	}
	dingUserID, err := client.ResolveUserIDByAuthCode(r.Context(), req.AuthCode)
	if err != nil {
		log.Println("dingtalk resolve user failed:", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"code": 1, "msg": "钉钉登录失败：" + err.Error()})
		return
	}
	var id int64
	var name, department string
	var status bool
	err = db.QueryRowContext(r.Context(), "SELECT id, name, department, status FROM user WHERE dingtalk_org=? AND dingtalk_userid=? AND status=1", req.State, dingUserID).
		Scan(&id, &name, &department, &status)
	if err == sql.ErrNoRows {
		// 未绑定本地用户时，用钉钉身份信息自动注册并登录，初始密码 123456
		id, name, department, err = autoRegisterDingTalkUser(r.Context(), client, req.State, dingUserID)
		if err != nil {
			log.Println("auto register dingtalk user failed:", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"code": 1, "msg": "钉钉账号自动注册失败：" + err.Error()})
			return
		}
	} else if err != nil {
		log.Println("query user by dingtalk id failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "查询失败"})
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

// autoRegisterDingTalkUser 在钉钉账号未绑定本地用户时，用钉钉身份信息自动建号并返回用户ID、姓名、部门。
// 姓名/手机号/部门全部取自钉钉，初始密码为 123456（APP 内支持后续修改）。
func autoRegisterDingTalkUser(ctx context.Context, client *DingTalkClient, orgKey, dingUserID string) (int64, string, string, error) {
	if client == nil || db == nil {
		return 0, "", "", errors.New("钉钉服务未配置")
	}
	details, err := client.GetUserDetails(ctx, dingUserID)
	if err != nil {
		return 0, "", "", fmt.Errorf("获取钉钉用户详情失败: %w", err)
	}
	name := strings.TrimSpace(details.Name)
	if name == "" {
		name = dingUserID
	}
	mobile := strings.TrimSpace(details.Mobile)
	department := formatDingTalkDepartment(details.DepartmentIDs)

	// 先按手机号匹配同组织已有用户，命中则补绑 dingtalk_userid 后直接登录，避免重复建号
	if mobile != "" {
		var uid int64
		findErr := db.QueryRowContext(ctx, "SELECT id FROM user WHERE dingtalk_org=? AND mobile=? AND status=1 LIMIT 1", orgKey, mobile).Scan(&uid)
		if findErr == nil {
			if _, updateErr := db.ExecContext(ctx, "UPDATE user SET dingtalk_userid=? WHERE id=? AND dingtalk_org=?", dingUserID, uid, orgKey); updateErr != nil {
				return 0, "", "", fmt.Errorf("绑定钉钉账号失败: %w", updateErr)
			}
			var uname, dept string
			if err := db.QueryRowContext(ctx, "SELECT name, department FROM user WHERE id=?", uid).Scan(&uname, &dept); err != nil {
				return 0, "", "", fmt.Errorf("读取用户信息失败: %w", err)
			}
			return uid, uname, dept, nil
		}
		if findErr != sql.ErrNoRows {
			return 0, "", "", fmt.Errorf("查询用户失败: %w", findErr)
		}
	}

	hash, err := hashPassword("123456")
	if err != nil {
		return 0, "", "", fmt.Errorf("生成初始密码失败: %w", err)
	}
	result, err := db.ExecContext(ctx, "INSERT INTO user(name, department, password_hash, mobile, dingtalk_userid, dingtalk_org, status) VALUES(?,?,?,?,?,?,1)",
		name, department, hash, nullableString(mobile), dingUserID, orgKey)
	if err != nil {
		return 0, "", "", fmt.Errorf("自动注册失败: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil || id <= 0 {
		return 0, "", "", errors.New("自动注册后无法获取用户ID")
	}
	return id, name, department, nil
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
	req.DingTalkOrg = normalizeDingTalkOrg(req.DingTalkOrg)
	if req.DingTalkOrg != dataDingTalkOrg && req.DingTalkOrg != eduDingTalkOrg {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 1, "msg": "钉钉组织参数错误"})
		return
	}
	if req.UserName == "" || req.Password == "" || req.Department == "" || req.Mobile == "" || len(req.UserName) > 64 || len(req.Password) > 128 || len(req.Department) > 64 || len(req.Mobile) > 32 || len(req.DingTalkUserID) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 1, "msg": "用户名、密码、部门或手机号格式错误"})
		return
	}
	hash, err := hashPassword(req.Password)
	if err != nil {
		log.Println("hash password failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "注册失败"})
		return
	}
	result, err := db.ExecContext(r.Context(), "INSERT INTO user(name, department, password_hash, mobile, dingtalk_userid, dingtalk_org, status) VALUES(?,?,?,?,?,?,1)", req.UserName, req.Department, hash, nullableString(req.Mobile), nullableString(req.DingTalkUserID), req.DingTalkOrg)
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
	req.DeviceID = strings.TrimSpace(req.DeviceID)
	if req.DeviceID == "" || len(req.DeviceID) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 1, "msg": "设备标识无效"})
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
	// 同一设备 1 小时内只允许同一用户打卡;窗口阈值与写入时间用同一 Go 时钟(东八区),不依赖 MySQL 时区
	windowStart := time.Now().In(checkinLocation).Add(-time.Hour)
	var deviceUserID int64
	err = db.QueryRowContext(r.Context(), "SELECT user_id FROM checkin_log WHERE device_id=? AND created_at >= ? ORDER BY created_at DESC LIMIT 1", req.DeviceID, windowStart).
		Scan(&deviceUserID)
	if err != nil && err != sql.ErrNoRows {
		log.Println("query device checkin failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "查询设备打卡记录失败"})
		return
	}
	if err != sql.ErrNoRows && deviceUserID != uid {
		writeJSON(w, http.StatusConflict, map[string]any{"code": 409, "msg": "该设备已经打过一次卡"})
		return
	}
	checkTime := time.Now().In(checkinLocation)
	result, err := db.ExecContext(r.Context(), "INSERT INTO checkin_log(user_id, point_id, device_id, created_at) VALUES(?,?,?,?)",
		uid, req.PointID, req.DeviceID, checkTime)
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
	dingTalkSync, syncErr := queueDingTalkUpload(r.Context(), checkinID, uid, checkTime, dingTalkClients)
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

// myDingTalkUserIDPrefix returns the first six characters of the authenticated
// user's DingTalk userid. The userid is always loaded using the JWT subject.
func myDingTalkUserIDPrefix(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": 405, "msg": "方法不允许"})
		return
	}
	uid, ok := userIDFromReq(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"code": 401, "msg": "未登录"})
		return
	}

	var dingTalkUserID sql.NullString
	err := db.QueryRowContext(r.Context(), "SELECT dingtalk_userid FROM user WHERE id=? AND status=1", uid).Scan(&dingTalkUserID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "msg": "用户未绑定钉钉账号"})
		return
	}
	if err != nil {
		log.Println("query dingtalk userid failed:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 1, "msg": "查询钉钉用户信息失败"})
		return
	}

	value := strings.TrimSpace(dingTalkUserID.String)
	if !dingTalkUserID.Valid || value == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "msg": "用户未绑定钉钉账号"})
		return
	}
	prefix := value
	if runes := []rune(value); len(runes) > 6 {
		prefix = string(runes[:6])
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code": 0,
		"msg":  "查询成功",
		"data": map[string]any{"dingtalkUserIdPrefix": prefix},
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
	dingTalkClients = newDingTalkClientsFromEnv()
	if len(os.Args) == 2 && os.Args[1] == "sync-dingtalk-users" {
		if len(dingTalkClients) == 0 {
			log.Fatal("DingTalk credentials are required for sync-dingtalk-users")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for orgKey, client := range dingTalkClients {
			stats, syncErr := importDingTalkUsers(ctx, orgKey, client)
			if syncErr != nil {
				log.Fatalf("sync DingTalk users for %s failed: %v", orgKey, syncErr)
			}
			fmt.Printf("DingTalk users synced for %s: fetched=%d inserted=%d updated=%d skipped=%d\n", orgKey, stats.Fetched, stats.Inserted, stats.Updated, stats.Skipped)
		}
		return
	}
	if len(dingTalkClients) > 0 && strings.EqualFold(envOrDefault("DINGTALK_SYNC_USERS_ON_START", "true"), "true") {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for orgKey, client := range dingTalkClients {
				updated, syncErr := syncDingTalkUserMappings(ctx, orgKey, client)
				if syncErr != nil {
					log.Printf("sync dingtalk user mappings for %s failed: %v", orgKey, syncErr)
					continue
				}
				if updated > 0 {
					log.Printf("synced %d dingtalk user mappings for %s", updated, orgKey)
				}
			}
		}()
	}
	startDingTalkWorker(context.Background(), dingTalkClients)
	http.HandleFunc("/api/login", login)
	http.HandleFunc("/api/dingtalk/login", dingTalkLogin)
	http.HandleFunc("/api/register", register)
	http.HandleFunc("/api/change-password", changePassword)
	http.HandleFunc("/api/checkin", checkin)
	http.HandleFunc("/api/checkins/me", myCheckins)
	http.HandleFunc("/api/dingtalk-userid-prefix", myDingTalkUserIDPrefix)
	http.HandleFunc("/openapi.json", serveSpec)
	listenAddr := envOrDefault("LISTEN_ADDR", "0.0.0.0:8080")
	log.Println("checkin server on", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}
