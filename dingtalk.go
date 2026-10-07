package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	dingTalkUploadURL       = "https://oapi.dingtalk.com/topapi/attendance/record/upload"
	dingTalkTokenURL        = "https://oapi.dingtalk.com/gettoken"
	dingTalkUserByMobileURL = "https://oapi.dingtalk.com/topapi/v2/user/getbymobile"
	dingTalkUserListURL     = "https://oapi.dingtalk.com/topapi/v2/user/list"
	dingTalkMaxHistory      = 180 * 24 * time.Hour
	dingTalkDefaultDeviceID = "checkin-server-001"
	dingTalkDefaultDevice   = "外部系统同步"
)

var errDingTalkUserNotMapped = errors.New("dingtalk user id is not mapped")

// DingTalkUploader represents the external attendance upload dependency.
// Keeping this interface small makes check-in tests independent of the network.
type DingTalkUploader interface {
	Upload(ctx context.Context, userID string, checkTime time.Time, photoURL string) (DingTalkUploadResult, error)
}

type DingTalkUploadResult struct {
	RequestID string
}

type DingTalkClient struct {
	clientID           string
	clientSecret       string
	accessToken        string
	staticAccessToken  bool
	accessTokenExpires time.Time
	tokenEndpoint      string
	endpoint           string
	userEndpoint       string
	userListEndpoint   string
	rootDeptID         int64
	deviceName         string
	deviceID           string
	format             string
	httpClient         *http.Client
	tokenMu            sync.Mutex
}

type dingTalkUploadPayload struct {
	UserID        string `json:"userid"`
	DeviceName    string `json:"device_name"`
	DeviceID      string `json:"device_id"`
	UserCheckTime int64  `json:"user_check_time"`
	PhotoURL      string `json:"photo_url,omitempty"`
}

type dingTalkUploadResponse struct {
	ErrCode   int    `json:"errcode"`
	ErrMsg    string `json:"errmsg"`
	Success   bool   `json:"success"`
	RequestID string `json:"request_id"`
}

type dingTalkUserByMobileResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
	Result  struct {
		UserID string `json:"userid"`
	} `json:"result"`
}

type dingTalkUserListRequest struct {
	DeptID   int64  `json:"dept_id"`
	Cursor   int64  `json:"cursor"`
	Size     int    `json:"size"`
	Language string `json:"language,omitempty"`
}

type dingTalkUserListResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
	Result  struct {
		HasMore    bool  `json:"has_more"`
		NextCursor int64 `json:"next_cursor"`
		List       []struct {
			UserID string `json:"userid"`
			Mobile string `json:"mobile"`
		} `json:"list"`
	} `json:"result"`
}

type dingTalkTokenResponse struct {
	ErrCode     int    `json:"errcode"`
	ErrMsg      string `json:"errmsg"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

func newDingTalkClientFromEnv() *DingTalkClient {
	accessToken := strings.TrimSpace(os.Getenv("DINGTALK_ACCESS_TOKEN"))
	clientID := firstNonEmpty(os.Getenv("DINGTALK_CLIENT_ID"), os.Getenv("DINGTALK_APP_KEY"))
	clientSecret := firstNonEmpty(os.Getenv("DINGTALK_CLIENT_SECRET"), os.Getenv("DINGTALK_APP_SECRET"))
	if accessToken == "" && (clientID == "" || clientSecret == "") {
		log.Println("DINGTALK_CLIENT_ID/DINGTALK_CLIENT_SECRET are not configured; DingTalk sync disabled")
		return nil
	}
	endpoint := strings.TrimSpace(os.Getenv("DINGTALK_UPLOAD_URL"))
	if endpoint == "" {
		endpoint = dingTalkUploadURL
	}
	format := strings.ToLower(strings.TrimSpace(os.Getenv("DINGTALK_UPLOAD_FORMAT")))
	if format == "" {
		format = "form"
	}
	if format != "form" && format != "json" {
		log.Printf("unsupported DINGTALK_UPLOAD_FORMAT %q; using form", format)
		format = "form"
	}
	rootDeptID := int64(1)
	if raw := strings.TrimSpace(os.Getenv("DINGTALK_ROOT_DEPT_ID")); raw != "" {
		if parsed, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil && parsed > 0 {
			rootDeptID = parsed
		}
	}
	httpClient, err := newDingTalkHTTPClient()
	if err != nil {
		log.Printf("invalid DINGTALK_PROXY_URL: %v; using direct connection", err)
		httpClient = directDingTalkHTTPClient()
	}
	return &DingTalkClient{
		clientID:          clientID,
		clientSecret:      clientSecret,
		accessToken:       accessToken,
		staticAccessToken: accessToken != "",
		tokenEndpoint:     envOrDefault("DINGTALK_TOKEN_URL", dingTalkTokenURL),
		endpoint:          endpoint,
		userEndpoint:      envOrDefault("DINGTALK_USER_BY_MOBILE_URL", dingTalkUserByMobileURL),
		userListEndpoint:  envOrDefault("DINGTALK_USER_LIST_URL", dingTalkUserListURL),
		rootDeptID:        rootDeptID,
		deviceName:        envOrDefault("DINGTALK_DEVICE_NAME", dingTalkDefaultDevice),
		deviceID:          envOrDefault("DINGTALK_DEVICE_ID", dingTalkDefaultDeviceID),
		format:            format,
		httpClient:        httpClient,
	}
}

// directDingTalkHTTPClient avoids inheriting an unrelated process-wide proxy
// (for example a stale local proxy at 127.0.0.1:9). Deployments that need a
// proxy can opt in explicitly with DINGTALK_PROXY_URL.
func directDingTalkHTTPClient() *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	}
	clone := transport.Clone()
	clone.Proxy = nil
	return &http.Client{Timeout: 10 * time.Second, Transport: clone}
}

func newDingTalkHTTPClient() (*http.Client, error) {
	proxyURL := strings.TrimSpace(os.Getenv("DINGTALK_PROXY_URL"))
	if proxyURL == "" {
		return directDingTalkHTTPClient(), nil
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		if err == nil {
			err = errors.New("proxy URL must include scheme and host")
		}
		return nil, err
	}
	client := directDingTalkHTTPClient()
	transport := client.Transport.(*http.Transport)
	transport.Proxy = http.ProxyURL(parsed)
	return client, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func (c *DingTalkClient) getAccessToken(ctx context.Context) (string, error) {
	if c == nil {
		return "", errors.New("dingtalk client is nil")
	}
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	if c.staticAccessToken && strings.TrimSpace(c.accessToken) != "" {
		return strings.TrimSpace(c.accessToken), nil
	}
	if c.clientID == "" || c.clientSecret == "" {
		if strings.TrimSpace(c.accessToken) != "" {
			return strings.TrimSpace(c.accessToken), nil
		}
		return "", errors.New("dingtalk client credentials are not configured")
	}
	if strings.TrimSpace(c.accessToken) != "" && time.Now().Before(c.accessTokenExpires) {
		return strings.TrimSpace(c.accessToken), nil
	}

	tokenEndpoint := c.tokenEndpoint
	if strings.TrimSpace(tokenEndpoint) == "" {
		tokenEndpoint = dingTalkTokenURL
	}
	endpoint, err := url.Parse(tokenEndpoint)
	if err != nil {
		return "", fmt.Errorf("parse dingtalk token endpoint: %w", err)
	}
	query := endpoint.Query()
	query.Set("appkey", c.clientID)
	query.Set("appsecret", c.clientSecret)
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", fmt.Errorf("create dingtalk token request: %w", err)
	}
	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call dingtalk token endpoint: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read dingtalk token response: %w", err)
	}
	var result dingTalkTokenResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return "", fmt.Errorf("decode dingtalk token response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || result.ErrCode != 0 {
		return "", fmt.Errorf("dingtalk token request failed (%d): %s", result.ErrCode, result.ErrMsg)
	}
	if strings.TrimSpace(result.AccessToken) == "" {
		return "", errors.New("dingtalk token response did not include access_token")
	}
	c.accessToken = strings.TrimSpace(result.AccessToken)
	cacheDuration := time.Duration(result.ExpiresIn) * time.Second
	if cacheDuration > 5*time.Minute {
		cacheDuration -= 5 * time.Minute
	} else {
		cacheDuration /= 2
	}
	if cacheDuration <= 0 {
		cacheDuration = time.Minute
	}
	c.accessTokenExpires = time.Now().Add(cacheDuration)
	return c.accessToken, nil
}

func (c *DingTalkClient) Upload(ctx context.Context, userID string, checkTime time.Time, photoURL string) (DingTalkUploadResult, error) {
	if c == nil {
		return DingTalkUploadResult{}, errors.New("dingtalk client is nil")
	}
	if strings.TrimSpace(userID) == "" {
		return DingTalkUploadResult{}, errDingTalkUserNotMapped
	}
	now := time.Now()
	if checkTime.Before(now.Add(-dingTalkMaxHistory)) || checkTime.After(now.Add(5*time.Minute)) {
		return DingTalkUploadResult{}, fmt.Errorf("check-in time must be within the last 180 days")
	}

	payload := dingTalkUploadPayload{
		UserID:        userID,
		DeviceName:    c.deviceName,
		DeviceID:      c.deviceID,
		UserCheckTime: checkTime.UnixMilli(),
		PhotoURL:      strings.TrimSpace(photoURL),
	}
	body, contentType, err := c.encodePayload(payload)
	if err != nil {
		return DingTalkUploadResult{}, err
	}
	endpoint, err := url.Parse(c.endpoint)
	if err != nil {
		return DingTalkUploadResult{}, fmt.Errorf("parse dingtalk endpoint: %w", err)
	}
	accessToken, err := c.getAccessToken(ctx)
	if err != nil {
		return DingTalkUploadResult{}, err
	}
	query := endpoint.Query()
	query.Set("access_token", accessToken)
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(body))
	if err != nil {
		return DingTalkUploadResult{}, fmt.Errorf("create dingtalk request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(req)
	if err != nil {
		return DingTalkUploadResult{}, fmt.Errorf("call dingtalk upload: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return DingTalkUploadResult{}, fmt.Errorf("read dingtalk response: %w", err)
	}
	var result dingTalkUploadResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return DingTalkUploadResult{}, fmt.Errorf("decode dingtalk response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return DingTalkUploadResult{RequestID: result.RequestID}, fmt.Errorf("dingtalk returned HTTP %d: %s", response.StatusCode, result.ErrMsg)
	}
	if result.ErrCode != 0 || !result.Success {
		return DingTalkUploadResult{RequestID: result.RequestID}, fmt.Errorf("dingtalk upload failed (%d): %s", result.ErrCode, result.ErrMsg)
	}
	return DingTalkUploadResult{RequestID: result.RequestID}, nil
}

func (c *DingTalkClient) ResolveUserIDByMobile(ctx context.Context, mobile string) (string, error) {
	mobile = strings.TrimSpace(mobile)
	if mobile == "" {
		return "", errDingTalkUserNotMapped
	}
	if c == nil {
		return "", errors.New("dingtalk client is nil")
	}
	accessToken, err := c.getAccessToken(ctx)
	if err != nil {
		return "", err
	}
	userEndpoint := c.userEndpoint
	if strings.TrimSpace(userEndpoint) == "" {
		userEndpoint = dingTalkUserByMobileURL
	}
	endpoint, err := url.Parse(userEndpoint)
	if err != nil {
		return "", fmt.Errorf("parse dingtalk user endpoint: %w", err)
	}
	query := endpoint.Query()
	query.Set("access_token", accessToken)
	endpoint.RawQuery = query.Encode()
	body, err := json.Marshal(map[string]string{"mobile": mobile})
	if err != nil {
		return "", fmt.Errorf("encode dingtalk user request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("create dingtalk user request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call dingtalk user lookup: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read dingtalk user response: %w", err)
	}
	var result dingTalkUserByMobileResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return "", fmt.Errorf("decode dingtalk user response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || result.ErrCode != 0 {
		return "", fmt.Errorf("dingtalk user lookup failed (%d): %s", result.ErrCode, result.ErrMsg)
	}
	if strings.TrimSpace(result.Result.UserID) == "" {
		return "", errDingTalkUserNotMapped
	}
	return strings.TrimSpace(result.Result.UserID), nil
}

type dingTalkDirectoryUser struct {
	UserID string
	Mobile string
}

// ListUsers returns active users from the configured root department. The
// endpoint is paginated so large organizations do not require one request per
// local user.
func (c *DingTalkClient) ListUsers(ctx context.Context) ([]dingTalkDirectoryUser, error) {
	if c == nil {
		return nil, errors.New("dingtalk client is nil")
	}
	accessToken, err := c.getAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	endpointURL := c.userListEndpoint
	if strings.TrimSpace(endpointURL) == "" {
		endpointURL = dingTalkUserListURL
	}
	endpoint, err := url.Parse(endpointURL)
	if err != nil {
		return nil, fmt.Errorf("parse dingtalk user list endpoint: %w", err)
	}
	query := endpoint.Query()
	query.Set("access_token", accessToken)
	endpoint.RawQuery = query.Encode()

	deptID := c.rootDeptID
	if deptID <= 0 {
		deptID = 1
	}
	const pageSize = 100
	var cursor int64
	users := make([]dingTalkDirectoryUser, 0)
	for page := 0; page < 1000; page++ {
		payload, err := json.Marshal(dingTalkUserListRequest{
			DeptID: deptID,
			Cursor: cursor,
			Size:   pageSize,
		})
		if err != nil {
			return nil, fmt.Errorf("encode dingtalk user list request: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(string(payload)))
		if err != nil {
			return nil, fmt.Errorf("create dingtalk user list request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		httpClient := c.httpClient
		if httpClient == nil {
			httpClient = http.DefaultClient
		}
		response, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("call dingtalk user list: %w", err)
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		response.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read dingtalk user list response: %w", readErr)
		}
		var result dingTalkUserListResponse
		if err := json.Unmarshal(responseBody, &result); err != nil {
			return nil, fmt.Errorf("decode dingtalk user list response: %w", err)
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || result.ErrCode != 0 {
			return nil, fmt.Errorf("dingtalk user list failed (%d): %s", result.ErrCode, result.ErrMsg)
		}
		for _, item := range result.Result.List {
			if strings.TrimSpace(item.UserID) == "" || strings.TrimSpace(item.Mobile) == "" {
				continue
			}
			users = append(users, dingTalkDirectoryUser{
				UserID: strings.TrimSpace(item.UserID),
				Mobile: strings.TrimSpace(item.Mobile),
			})
		}
		if !result.Result.HasMore {
			return users, nil
		}
		if result.Result.NextCursor <= cursor {
			return nil, errors.New("dingtalk user list returned an invalid next_cursor")
		}
		cursor = result.Result.NextCursor
	}
	return nil, errors.New("dingtalk user list exceeded pagination limit")
}

// syncDingTalkUserMappings resolves all local mobile numbers in one paginated
// directory read and persists the matching DingTalk user IDs.
func syncDingTalkUserMappings(ctx context.Context, client *DingTalkClient) (int64, error) {
	if client == nil || db == nil {
		return 0, nil
	}
	rows, err := db.QueryContext(ctx, "SELECT id, mobile FROM user WHERE status=1 AND mobile IS NOT NULL AND mobile<>''")
	if err != nil {
		return 0, fmt.Errorf("query local mobile mappings: %w", err)
	}
	defer rows.Close()
	localUsers := make(map[string]int64)
	for rows.Next() {
		var id int64
		var mobile string
		if err := rows.Scan(&id, &mobile); err != nil {
			return 0, fmt.Errorf("scan local mobile mapping: %w", err)
		}
		mobile = strings.TrimSpace(mobile)
		if mobile != "" {
			localUsers[mobile] = id
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate local mobile mappings: %w", err)
	}
	if len(localUsers) == 0 {
		return 0, nil
	}
	directoryUsers, err := client.ListUsers(ctx)
	if err != nil {
		return 0, err
	}
	var updated int64
	for _, directoryUser := range directoryUsers {
		uid, ok := localUsers[directoryUser.Mobile]
		if !ok {
			continue
		}
		result, err := db.ExecContext(ctx,
			"UPDATE user SET dingtalk_userid=? WHERE id=? AND status=1",
			directoryUser.UserID, uid)
		if err != nil {
			return updated, fmt.Errorf("save dingtalk user mapping: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return updated, fmt.Errorf("read mapping update result: %w", err)
		}
		updated += affected
	}
	return updated, nil
}

func (c *DingTalkClient) encodePayload(payload dingTalkUploadPayload) (string, string, error) {
	if c.format == "json" {
		body, err := json.Marshal(payload)
		return string(body), "application/json", err
	}
	form := url.Values{}
	form.Set("userid", payload.UserID)
	form.Set("device_name", payload.DeviceName)
	form.Set("device_id", payload.DeviceID)
	form.Set("user_check_time", strconv.FormatInt(payload.UserCheckTime, 10))
	if payload.PhotoURL != "" {
		form.Set("photo_url", payload.PhotoURL)
	}
	return form.Encode(), "application/x-www-form-urlencoded", nil
}

func lookupDingTalkUserID(ctx context.Context, uid int64) (string, string, error) {
	var dingTalkUserID, mobile sql.NullString
	err := db.QueryRowContext(ctx, "SELECT dingtalk_userid, mobile FROM user WHERE id=? AND status=1", uid).Scan(&dingTalkUserID, &mobile)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", "", errDingTalkUserNotMapped
		}
		return "", "", err
	}
	if dingTalkUserID.Valid && strings.TrimSpace(dingTalkUserID.String) != "" {
		return strings.TrimSpace(dingTalkUserID.String), strings.TrimSpace(mobile.String), nil
	}
	if mobile.Valid && strings.TrimSpace(mobile.String) != "" {
		return "", strings.TrimSpace(mobile.String), errDingTalkUserNotMapped
	}
	return "", "", errDingTalkUserNotMapped
}

func queueDingTalkUpload(ctx context.Context, checkinID, uid int64, checkTime time.Time, uploader DingTalkUploader) (string, error) {
	if uploader == nil {
		return "disabled", nil
	}
	dingTalkUserID, mobile, err := lookupDingTalkUserID(ctx, uid)
	if err != nil {
		if errors.Is(err, errDingTalkUserNotMapped) {
			resolver, canResolve := uploader.(interface {
				ResolveUserIDByMobile(context.Context, string) (string, error)
			})
			if !canResolve || mobile == "" {
				return "skipped_missing_mapping", nil
			}
			dingTalkUserID, err = resolver.ResolveUserIDByMobile(ctx, mobile)
			if err != nil {
				return "skipped_user_lookup_failed", nil
			}
			if _, err := db.ExecContext(ctx, "UPDATE user SET dingtalk_userid=? WHERE id=? AND (dingtalk_userid IS NULL OR dingtalk_userid='')", dingTalkUserID, uid); err != nil {
				return "", err
			}
		} else {
			return "", err
		}
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO dingtalk_upload (checkin_id, dingtalk_userid, user_check_time, status, next_attempt_at)
		VALUES (?, ?, ?, 'pending', CURRENT_TIMESTAMP)`, checkinID, dingTalkUserID, checkTime.UnixMilli())
	if err != nil {
		return "", err
	}
	return "pending", nil
}

func startDingTalkWorker(ctx context.Context, uploader DingTalkUploader) {
	if uploader == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			processDingTalkUploads(ctx, uploader)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

type pendingDingTalkUpload struct {
	ID             int64
	DingTalkUserID string
	UserCheckTime  int64
	Attempts       int
}

func processDingTalkUploads(ctx context.Context, uploader DingTalkUploader) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, dingtalk_userid, user_check_time, attempts
		FROM dingtalk_upload
		WHERE status='pending' AND (next_attempt_at IS NULL OR next_attempt_at <= CURRENT_TIMESTAMP)
		ORDER BY id
		LIMIT 20`)
	if err != nil {
		log.Println("query pending dingtalk uploads failed:", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var item pendingDingTalkUpload
		if err := rows.Scan(&item.ID, &item.DingTalkUserID, &item.UserCheckTime, &item.Attempts); err != nil {
			log.Println("scan pending dingtalk upload failed:", err)
			continue
		}
		processOneDingTalkUpload(ctx, uploader, item)
	}
	if err := rows.Err(); err != nil {
		log.Println("iterate pending dingtalk uploads failed:", err)
	}
}

func processOneDingTalkUpload(ctx context.Context, uploader DingTalkUploader, item pendingDingTalkUpload) {
	claim, err := db.ExecContext(ctx, `
		UPDATE dingtalk_upload
		SET status='processing'
		WHERE id=? AND status='pending'`, item.ID)
	if err != nil {
		log.Println("claim dingtalk upload failed:", err)
		return
	}
	claimed, err := claim.RowsAffected()
	if err != nil || claimed != 1 {
		return
	}

	result, err := uploader.Upload(ctx, item.DingTalkUserID, time.UnixMilli(item.UserCheckTime), "")
	if err == nil {
		_, updateErr := db.ExecContext(ctx, `
			UPDATE dingtalk_upload
			SET status='success', attempts=attempts+1, request_id=?, uploaded_at=CURRENT_TIMESTAMP, last_error=NULL
			WHERE id=? AND status='processing'`, result.RequestID, item.ID)
		if updateErr != nil {
			log.Println("mark dingtalk upload success failed:", updateErr)
		}
		return
	}

	attempts := item.Attempts + 1
	backoff := time.Duration(1<<min(attempts, 10)) * time.Minute
	if backoff > time.Hour {
		backoff = time.Hour
	}
	_, updateErr := db.ExecContext(ctx, `
		UPDATE dingtalk_upload
		SET status='pending', attempts=?, next_attempt_at=DATE_ADD(CURRENT_TIMESTAMP, INTERVAL ? SECOND), last_error=?
		WHERE id=? AND status='processing'`, attempts, int(backoff/time.Second), truncateError(err.Error()), item.ID)
	if updateErr != nil {
		log.Println("schedule dingtalk upload retry failed:", updateErr)
	}
}

func truncateError(message string) string {
	if len(message) > 500 {
		return message[:500]
	}
	return message
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
