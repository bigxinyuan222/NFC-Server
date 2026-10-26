package main

import (
	"bytes"
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
	dingTalkUploadURL           = "https://oapi.dingtalk.com/topapi/attendance/record/upload"
	dingTalkTokenURL            = "https://oapi.dingtalk.com/gettoken"
	dingTalkUserByMobileURL     = "https://oapi.dingtalk.com/topapi/v2/user/getbymobile"
	dingTalkUserListURL         = "https://oapi.dingtalk.com/topapi/v2/user/list"
	dingTalkDepartmentListURL   = "https://oapi.dingtalk.com/topapi/v2/department/listsub"
	dingTalkUserDetailURL       = "https://oapi.dingtalk.com/topapi/v2/user/get"
	dingTalkUserByUnionIDURL    = "https://oapi.dingtalk.com/topapi/user/getbyunionid"
	dingTalkUserAccessTokenURL  = "https://api.dingtalk.com/v1.0/oauth2/userAccessToken"
	dingTalkUserPersonalInfoURL = "https://api.dingtalk.com/v1.0/contact/users/me"
	dingTalkMaxHistory          = 180 * 24 * time.Hour
	dingTalkDefaultDeviceID     = "checkin-server-001"
	dingTalkDefaultDevice       = "外部系统同步"
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
	orgKey                 string
	clientID               string
	clientSecret           string
	accessToken            string
	staticAccessToken      bool
	accessTokenExpires     time.Time
	tokenEndpoint          string
	endpoint               string
	userEndpoint           string
	userListEndpoint       string
	departmentListEndpoint string
	userDetailEndpoint     string
	rootDeptID             int64
	deviceName             string
	deviceID               string
	format                 string
	httpClient             *http.Client
	tokenMu                sync.Mutex
}

// DingTalkClients keeps one authenticated client per DingTalk organization.
// The map key is persisted with local users and upload queue rows so queued
// records cannot accidentally be sent through another organization's app.
type DingTalkClients map[string]*DingTalkClient

const (
	dataDingTalkOrg = "org_lezhidata"
	eduDingTalkOrg  = "org_lezhiedu"
)

func (clients DingTalkClients) clientFor(orgKey string) *DingTalkClient {
	orgKey = normalizeDingTalkOrg(orgKey)
	return clients[orgKey]
}

func normalizeDingTalkOrg(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(envOrDefault("DINGTALK_DEFAULT_ORG", eduDingTalkOrg)))
	}
	if value == "" {
		return eduDingTalkOrg
	}
	return value
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

type dingTalkUserDetailResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
	Result  struct {
		UserID        string  `json:"userid"`
		Name          string  `json:"name"`
		Mobile        string  `json:"mobile"`
		DepartmentIDs []int64 `json:"dept_id_list"`
	} `json:"result"`
}

type dingTalkUserAccessTokenRequest struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	Code         string `json:"code"`
	GrantType    string `json:"grantType"`
}

type dingTalkUserAccessTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
}

type dingTalkUserPersonalInfoResponse struct {
	Nick      string `json:"nick"`
	OpenID    string `json:"openId"`
	UnionID   string `json:"unionId"`
	Mobile    string `json:"mobile"`
	Email     string `json:"email"`
	StateCode string `json:"stateCode"`
}

type dingTalkUserByUnionIDResponse struct {
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
			UserID        string  `json:"userid"`
			Name          string  `json:"name"`
			Mobile        string  `json:"mobile"`
			DepartmentIDs []int64 `json:"dept_id_list"`
		} `json:"list"`
	} `json:"result"`
}

type dingTalkDepartmentListResponse struct {
	ErrCode int             `json:"errcode"`
	ErrMsg  string          `json:"errmsg"`
	Result  json.RawMessage `json:"result"`
}

type dingTalkTokenResponse struct {
	ErrCode     int    `json:"errcode"`
	ErrMsg      string `json:"errmsg"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

func newDingTalkClientFromEnv() *DingTalkClient {
	return newDingTalkClientFromEnvWithPrefix("", eduDingTalkOrg)
}

// newDingTalkClientFromEnvWithPrefix reads one organization's credentials.
// An empty prefix preserves the original DINGTALK_* variables. For example,
// prefix ORG_LEZHIDATA reads that organization's credentials.
func newDingTalkClientFromEnvWithPrefix(prefix, orgKey string) *DingTalkClient {
	envValue := func(name string) string {
		if prefix == "" {
			return os.Getenv("DINGTALK_" + name)
		}
		return os.Getenv("DINGTALK_" + prefix + "_" + name)
	}
	accessToken := strings.TrimSpace(envValue("ACCESS_TOKEN"))
	clientID := strings.TrimSpace(envValue("CLIENT_ID"))
	clientSecret := strings.TrimSpace(envValue("CLIENT_SECRET"))
	if prefix == "" {
		clientID = firstNonEmpty(clientID, os.Getenv("DINGTALK_APP_KEY"))
		clientSecret = firstNonEmpty(clientSecret, os.Getenv("DINGTALK_APP_SECRET"))
	}
	if accessToken == "" && (clientID == "" || clientSecret == "") {
		return nil
	}
	endpoint := strings.TrimSpace(envValue("UPLOAD_URL"))
	if endpoint == "" {
		endpoint = dingTalkUploadURL
	}
	format := strings.ToLower(strings.TrimSpace(envValue("UPLOAD_FORMAT")))
	if format == "" {
		format = "form"
	}
	if format != "form" && format != "json" {
		log.Printf("unsupported DINGTALK_UPLOAD_FORMAT %q; using form", format)
		format = "form"
	}
	rootDeptID := int64(1)
	if raw := strings.TrimSpace(envValue("ROOT_DEPT_ID")); raw != "" {
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
		orgKey:                 normalizeDingTalkOrg(orgKey),
		clientID:               clientID,
		clientSecret:           clientSecret,
		accessToken:            accessToken,
		staticAccessToken:      accessToken != "",
		tokenEndpoint:          firstNonEmpty(envValue("TOKEN_URL"), dingTalkTokenURL),
		endpoint:               endpoint,
		userEndpoint:           firstNonEmpty(envValue("USER_BY_MOBILE_URL"), dingTalkUserByMobileURL),
		userListEndpoint:       firstNonEmpty(envValue("USER_LIST_URL"), dingTalkUserListURL),
		departmentListEndpoint: firstNonEmpty(envValue("DEPARTMENT_LIST_URL"), dingTalkDepartmentListURL),
		userDetailEndpoint:     firstNonEmpty(envValue("USER_DETAIL_URL"), dingTalkUserDetailURL),
		rootDeptID:             rootDeptID,
		deviceName:             firstNonEmpty(envValue("DEVICE_NAME"), dingTalkDefaultDevice),
		deviceID:               firstNonEmpty(envValue("DEVICE_ID"), dingTalkDefaultDeviceID),
		format:                 format,
		httpClient:             httpClient,
	}
}

func newDingTalkClientsFromEnv() DingTalkClients {
	clients := make(DingTalkClients)
	if client := newDingTalkClientFromEnvWithPrefix("", eduDingTalkOrg); client != nil {
		clients[eduDingTalkOrg] = client
	}
	if client := newDingTalkClientFromEnvWithPrefix("ORG_LEZHIDATA", dataDingTalkOrg); client != nil {
		clients[dataDingTalkOrg] = client
	}
	if len(clients) == 0 {
		log.Println("DINGTALK_CLIENT_ID/DINGTALK_CLIENT_SECRET are not configured; DingTalk sync disabled")
	}
	return clients
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

func (c *DingTalkClient) getUserAccessTokenByAuthCode(ctx context.Context, authCode string) (string, error) {
	if c == nil {
		return "", errors.New("dingtalk client is nil")
	}
	authCode = strings.TrimSpace(authCode)
	if authCode == "" {
		return "", errors.New("authCode is empty")
	}
	if c.clientID == "" || c.clientSecret == "" {
		return "", errors.New("dingtalk client id/secret not configured")
	}
	body, err := json.Marshal(dingTalkUserAccessTokenRequest{
		ClientID:     c.clientID,
		ClientSecret: c.clientSecret,
		Code:         authCode,
		GrantType:    "authorization_code",
	})
	if err != nil {
		return "", fmt.Errorf("encode user access token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dingTalkUserAccessTokenURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create user access token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call user access token endpoint: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read user access token response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("user access token request failed (%d): %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	var result dingTalkUserAccessTokenResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return "", fmt.Errorf("decode user access token response: %w", err)
	}
	if strings.TrimSpace(result.AccessToken) == "" {
		return "", errors.New("user access token response missing accessToken")
	}
	return strings.TrimSpace(result.AccessToken), nil
}

func (c *DingTalkClient) getPersonalUnionIDByAccessToken(ctx context.Context, userAccessToken string) (string, error) {
	if c == nil {
		return "", errors.New("dingtalk client is nil")
	}
	userAccessToken = strings.TrimSpace(userAccessToken)
	if userAccessToken == "" {
		return "", errors.New("user access token is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dingTalkUserPersonalInfoURL, nil)
	if err != nil {
		return "", fmt.Errorf("create personal info request: %w", err)
	}
	req.Header.Set("x-acs-dingtalk-access-token", userAccessToken)
	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call personal info endpoint: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read personal info response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("personal info request failed (%d): %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	var result dingTalkUserPersonalInfoResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return "", fmt.Errorf("decode personal info response: %w", err)
	}
	if strings.TrimSpace(result.UnionID) == "" {
		return "", errors.New("personal info response missing unionId")
	}
	return strings.TrimSpace(result.UnionID), nil
}

func (c *DingTalkClient) getUserIDByUnionID(ctx context.Context, unionID string) (string, error) {
	if c == nil {
		return "", errors.New("dingtalk client is nil")
	}
	unionID = strings.TrimSpace(unionID)
	if unionID == "" {
		return "", errDingTalkUserNotMapped
	}
	accessToken, err := c.getAccessToken(ctx)
	if err != nil {
		return "", err
	}
	endpoint, err := url.Parse(dingTalkUserByUnionIDURL)
	if err != nil {
		return "", fmt.Errorf("parse getbyunionid endpoint: %w", err)
	}
	query := endpoint.Query()
	query.Set("access_token", accessToken)
	endpoint.RawQuery = query.Encode()
	body, err := json.Marshal(map[string]string{"unionid": unionID})
	if err != nil {
		return "", fmt.Errorf("encode getbyunionid request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create getbyunionid request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call getbyunionid endpoint: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read getbyunionid response: %w", err)
	}
	var result dingTalkUserByUnionIDResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return "", fmt.Errorf("decode getbyunionid response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || result.ErrCode != 0 {
		return "", fmt.Errorf("getbyunionid failed (%d): %s", result.ErrCode, result.ErrMsg)
	}
	if strings.TrimSpace(result.Result.UserID) == "" {
		return "", errDingTalkUserNotMapped
	}
	return strings.TrimSpace(result.Result.UserID), nil
}

func (c *DingTalkClient) ResolveUserIDByAuthCode(ctx context.Context, authCode string) (string, error) {
	userAccessToken, err := c.getUserAccessTokenByAuthCode(ctx, authCode)
	if err != nil {
		return "", err
	}
	unionID, err := c.getPersonalUnionIDByAccessToken(ctx, userAccessToken)
	if err != nil {
		return "", err
	}
	return c.getUserIDByUnionID(ctx, unionID)
}

func (c *DingTalkClient) GetUserDetails(ctx context.Context, userID string) (dingTalkDirectoryUser, error) {
	if c == nil {
		return dingTalkDirectoryUser{}, errors.New("dingtalk client is nil")
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return dingTalkDirectoryUser{}, errDingTalkUserNotMapped
	}
	accessToken, err := c.getAccessToken(ctx)
	if err != nil {
		return dingTalkDirectoryUser{}, err
	}
	endpointURL := c.userDetailEndpoint
	if strings.TrimSpace(endpointURL) == "" {
		endpointURL = dingTalkUserDetailURL
	}
	endpoint, err := url.Parse(endpointURL)
	if err != nil {
		return dingTalkDirectoryUser{}, fmt.Errorf("parse dingtalk user detail endpoint: %w", err)
	}
	query := endpoint.Query()
	query.Set("access_token", accessToken)
	endpoint.RawQuery = query.Encode()
	body, err := json.Marshal(map[string]string{"userid": userID})
	if err != nil {
		return dingTalkDirectoryUser{}, fmt.Errorf("encode dingtalk user detail request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(string(body)))
	if err != nil {
		return dingTalkDirectoryUser{}, fmt.Errorf("create dingtalk user detail request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(req)
	if err != nil {
		return dingTalkDirectoryUser{}, fmt.Errorf("call dingtalk user detail: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return dingTalkDirectoryUser{}, fmt.Errorf("read dingtalk user detail response: %w", err)
	}
	var result dingTalkUserDetailResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return dingTalkDirectoryUser{}, fmt.Errorf("decode dingtalk user detail response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || result.ErrCode != 0 {
		return dingTalkDirectoryUser{}, fmt.Errorf("dingtalk user detail failed (%d): %s", result.ErrCode, result.ErrMsg)
	}
	return dingTalkDirectoryUser{
		UserID:        strings.TrimSpace(result.Result.UserID),
		Name:          strings.TrimSpace(result.Result.Name),
		Mobile:        strings.TrimSpace(result.Result.Mobile),
		DepartmentIDs: append([]int64(nil), result.Result.DepartmentIDs...),
	}, nil
}

type dingTalkDirectoryUser struct {
	UserID        string
	Name          string
	Mobile        string
	DepartmentIDs []int64
}

// ListUsers returns active users from the configured root department. The
// endpoint is paginated so large organizations do not require one request per
// local user.
func (c *DingTalkClient) ListUsers(ctx context.Context) ([]dingTalkDirectoryUser, error) {
	return c.listUsersInDepartment(ctx, c.rootDeptID)
}

func (c *DingTalkClient) listUsersInDepartment(ctx context.Context, deptID int64) ([]dingTalkDirectoryUser, error) {
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
			if strings.TrimSpace(item.UserID) == "" {
				continue
			}
			users = append(users, dingTalkDirectoryUser{
				UserID:        strings.TrimSpace(item.UserID),
				Name:          strings.TrimSpace(item.Name),
				Mobile:        strings.TrimSpace(item.Mobile),
				DepartmentIDs: append([]int64(nil), item.DepartmentIDs...),
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

func (c *DingTalkClient) ListDepartmentIDs(ctx context.Context) ([]int64, error) {
	if c == nil {
		return nil, errors.New("dingtalk client is nil")
	}
	accessToken, err := c.getAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	endpointURL := c.departmentListEndpoint
	if strings.TrimSpace(endpointURL) == "" {
		endpointURL = dingTalkDepartmentListURL
	}
	endpoint, err := url.Parse(endpointURL)
	if err != nil {
		return nil, fmt.Errorf("parse dingtalk department endpoint: %w", err)
	}
	query := endpoint.Query()
	query.Set("access_token", accessToken)
	endpoint.RawQuery = query.Encode()
	rootID := c.rootDeptID
	if rootID <= 0 {
		rootID = 1
	}
	queue := []int64{rootID}
	seen := map[int64]bool{rootID: true}
	departmentIDs := make([]int64, 0, 16)
	for len(queue) > 0 {
		deptID := queue[0]
		queue = queue[1:]
		departmentIDs = append(departmentIDs, deptID)
		body, err := json.Marshal(map[string]int64{"dept_id": deptID})
		if err != nil {
			return nil, fmt.Errorf("encode dingtalk department request: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(string(body)))
		if err != nil {
			return nil, fmt.Errorf("create dingtalk department request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		httpClient := c.httpClient
		if httpClient == nil {
			httpClient = http.DefaultClient
		}
		response, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("call dingtalk department list: %w", err)
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read dingtalk department response: %w", readErr)
		}
		var result dingTalkDepartmentListResponse
		if err := json.Unmarshal(responseBody, &result); err != nil {
			return nil, fmt.Errorf("decode dingtalk department response: %w", err)
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || result.ErrCode != 0 {
			return nil, fmt.Errorf("dingtalk department list failed (%d): %s", result.ErrCode, result.ErrMsg)
		}
		childIDs, decodeErr := decodeDingTalkDepartmentIDs(result.Result)
		if decodeErr != nil {
			return nil, decodeErr
		}
		for _, childID := range childIDs {
			if childID > 0 && !seen[childID] {
				seen[childID] = true
				queue = append(queue, childID)
			}
		}
		if len(departmentIDs) > 10000 {
			return nil, errors.New("dingtalk department list exceeded safety limit")
		}
	}
	return departmentIDs, nil
}

func decodeDingTalkDepartmentIDs(raw json.RawMessage) ([]int64, error) {
	var direct []struct {
		DeptID int64 `json:"dept_id"`
	}
	if err := json.Unmarshal(raw, &direct); err == nil {
		ids := make([]int64, 0, len(direct))
		for _, item := range direct {
			ids = append(ids, item.DeptID)
		}
		return ids, nil
	}
	var wrapped struct {
		Dept []struct {
			DeptID int64 `json:"dept_id"`
		} `json:"dept"`
		Departments []struct {
			DeptID int64 `json:"dept_id"`
		} `json:"departments"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("decode dingtalk department result: %w", err)
	}
	ids := make([]int64, 0, len(wrapped.Dept)+len(wrapped.Departments))
	for _, item := range wrapped.Dept {
		ids = append(ids, item.DeptID)
	}
	for _, item := range wrapped.Departments {
		ids = append(ids, item.DeptID)
	}
	return ids, nil
}

// ListUsersAllDepartments traverses the department tree and de-duplicates
// users who belong to multiple departments.
func (c *DingTalkClient) ListUsersAllDepartments(ctx context.Context) ([]dingTalkDirectoryUser, error) {
	departmentIDs, err := c.ListDepartmentIDs(ctx)
	if err != nil {
		return nil, err
	}
	usersByID := make(map[string]dingTalkDirectoryUser)
	for _, departmentID := range departmentIDs {
		users, err := c.listUsersInDepartment(ctx, departmentID)
		if err != nil {
			return nil, err
		}
		for _, user := range users {
			if existing, ok := usersByID[user.UserID]; ok {
				if existing.Mobile == "" {
					existing.Mobile = user.Mobile
				}
				if existing.Name == "" {
					existing.Name = user.Name
				}
				usersByID[user.UserID] = existing
				continue
			}
			usersByID[user.UserID] = user
		}
	}
	users := make([]dingTalkDirectoryUser, 0, len(usersByID))
	for _, user := range usersByID {
		users = append(users, user)
	}
	return users, nil
}

// syncDingTalkUserMappings resolves all local mobile numbers in one paginated
// directory read and persists the matching DingTalk user IDs.
func syncDingTalkUserMappings(ctx context.Context, orgKey string, client *DingTalkClient) (int64, error) {
	if client == nil || db == nil {
		return 0, nil
	}
	orgKey = normalizeDingTalkOrg(orgKey)
	rows, err := db.QueryContext(ctx, "SELECT id, mobile FROM user WHERE dingtalk_org=? AND status=1 AND mobile IS NOT NULL AND mobile<>''", orgKey)
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
	directoryUsers, err := client.ListUsersAllDepartments(ctx)
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
			"UPDATE user SET dingtalk_userid=? WHERE id=? AND dingtalk_org=? AND status=1",
			directoryUser.UserID, uid, orgKey)
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

const importedDingTalkPasswordHash = "!dingtalk-imported-account-password-not-set"

type dingTalkImportStats struct {
	Fetched  int64
	Inserted int64
	Updated  int64
	Skipped  int64
}

// importDingTalkUsers imports the company directory into the local user table.
// Existing passwords are never changed; newly imported accounts receive an
// unusable placeholder password until an administrator sets one locally.
func importDingTalkUsers(ctx context.Context, orgKey string, client *DingTalkClient) (dingTalkImportStats, error) {
	var stats dingTalkImportStats
	if client == nil || db == nil {
		return stats, errors.New("dingtalk client or database is not configured")
	}
	orgKey = normalizeDingTalkOrg(orgKey)
	users, err := client.ListUsersAllDepartments(ctx)
	if err != nil {
		return stats, err
	}
	for index := range users {
		if users[index].Mobile != "" {
			continue
		}
		details, detailErr := client.GetUserDetails(ctx, users[index].UserID)
		if detailErr != nil {
			return stats, detailErr
		}
		if details.Name != "" {
			users[index].Name = details.Name
		}
		users[index].Mobile = details.Mobile
		if len(details.DepartmentIDs) > 0 {
			users[index].DepartmentIDs = details.DepartmentIDs
		}
	}
	for _, directoryUser := range users {
		stats.Fetched++
		name := strings.TrimSpace(directoryUser.Name)
		if name == "" {
			name = directoryUser.UserID
		}
		mobile := strings.TrimSpace(directoryUser.Mobile)
		department := formatDingTalkDepartment(directoryUser.DepartmentIDs)

		var localID int64
		findErr := db.QueryRowContext(ctx,
			"SELECT id FROM user WHERE dingtalk_org=? AND dingtalk_userid=? LIMIT 1", orgKey, directoryUser.UserID).Scan(&localID)
		if findErr == sql.ErrNoRows && mobile != "" {
			findErr = db.QueryRowContext(ctx,
				"SELECT id FROM user WHERE dingtalk_org=? AND mobile=? LIMIT 1", orgKey, mobile).Scan(&localID)
		}
		if findErr == sql.ErrNoRows && name != "" {
			findErr = db.QueryRowContext(ctx,
				"SELECT id FROM user WHERE dingtalk_org=? AND name=? LIMIT 1", orgKey, name).Scan(&localID)
		}
		if findErr == nil {
			result, updateErr := db.ExecContext(ctx, `
				UPDATE user
				SET dingtalk_userid=?,
				    mobile=CASE WHEN ?<>'' THEN ? ELSE mobile END,
				    department=CASE WHEN (department IS NULL OR department='') THEN ? ELSE department END
				WHERE id=? AND dingtalk_org=?`, directoryUser.UserID, mobile, mobile, department, localID, orgKey)
			if updateErr != nil {
				if isDuplicateUserError(updateErr) {
					stats.Skipped++
					continue
				}
				return stats, fmt.Errorf("update imported user %s: %w", directoryUser.UserID, updateErr)
			}
			affected, rowsErr := result.RowsAffected()
			if rowsErr != nil {
				return stats, rowsErr
			}
			stats.Updated += affected
			continue
		}
		if findErr != sql.ErrNoRows {
			return stats, fmt.Errorf("find local user for %s: %w", directoryUser.UserID, findErr)
		}

		_, insertErr := db.ExecContext(ctx, `
			INSERT INTO user(name, department, password_hash, mobile, dingtalk_userid, dingtalk_org, status)
			VALUES(?,?,?,?,?,?,1)`, name, department, importedDingTalkPasswordHash, nullableString(mobile), directoryUser.UserID, orgKey)
		if insertErr != nil {
			if isDuplicateUserError(insertErr) {
				stats.Skipped++
				continue
			}
			return stats, fmt.Errorf("insert imported user %s: %w", directoryUser.UserID, insertErr)
		}
		stats.Inserted++
	}
	return stats, nil
}

func formatDingTalkDepartment(ids []int64) string {
	if len(ids) == 0 {
		return "钉钉通讯录"
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	department := "钉钉部门:" + strings.Join(parts, ",")
	if len(department) > 64 {
		return department[:64]
	}
	return department
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

func lookupDingTalkUserID(ctx context.Context, uid int64) (string, string, string, error) {
	var orgKey, dingTalkUserID, mobile sql.NullString
	err := db.QueryRowContext(ctx, "SELECT dingtalk_org, dingtalk_userid, mobile FROM user WHERE id=? AND status=1", uid).Scan(&orgKey, &dingTalkUserID, &mobile)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", "", "", errDingTalkUserNotMapped
		}
		return "", "", "", err
	}
	org := normalizeDingTalkOrg(orgKey.String)
	if dingTalkUserID.Valid && strings.TrimSpace(dingTalkUserID.String) != "" {
		return org, strings.TrimSpace(dingTalkUserID.String), strings.TrimSpace(mobile.String), nil
	}
	if mobile.Valid && strings.TrimSpace(mobile.String) != "" {
		return org, "", strings.TrimSpace(mobile.String), errDingTalkUserNotMapped
	}
	return org, "", "", errDingTalkUserNotMapped
}

func queueDingTalkUpload(ctx context.Context, checkinID, uid int64, checkTime time.Time, clients DingTalkClients) (string, error) {
	if len(clients) == 0 {
		return "disabled", nil
	}
	orgKey, dingTalkUserID, mobile, err := lookupDingTalkUserID(ctx, uid)
	client := clients.clientFor(orgKey)
	if client == nil {
		return "skipped_org_not_configured", nil
	}
	if err != nil {
		if errors.Is(err, errDingTalkUserNotMapped) {
			if mobile == "" {
				return "skipped_missing_mapping", nil
			}
			dingTalkUserID, err = client.ResolveUserIDByMobile(ctx, mobile)
			if err != nil {
				return "skipped_user_lookup_failed", nil
			}
			if _, err := db.ExecContext(ctx, "UPDATE user SET dingtalk_userid=? WHERE id=? AND dingtalk_org=? AND (dingtalk_userid IS NULL OR dingtalk_userid='')", dingTalkUserID, uid, orgKey); err != nil {
				return "", err
			}
		} else {
			return "", err
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	for sequence := 1; sequence <= 2; sequence++ {
		uploadTime := checkTime.Add(time.Duration(sequence-1) * time.Second)
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO dingtalk_upload
				(checkin_id, upload_sequence, dingtalk_org, dingtalk_userid, user_check_time, status, next_attempt_at)
			VALUES (?, ?, ?, ?, ?, 'pending', CURRENT_TIMESTAMP)`,
			checkinID, sequence, orgKey, dingTalkUserID, uploadTime.UnixMilli()); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return "pending", nil
}

func startDingTalkWorker(ctx context.Context, clients DingTalkClients) {
	if len(clients) == 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			processDingTalkUploads(ctx, clients)
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
	OrgKey         string
	DingTalkUserID string
	UserCheckTime  int64
	Attempts       int
}

func processDingTalkUploads(ctx context.Context, clients DingTalkClients) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, dingtalk_org, dingtalk_userid, user_check_time, attempts
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
		if err := rows.Scan(&item.ID, &item.OrgKey, &item.DingTalkUserID, &item.UserCheckTime, &item.Attempts); err != nil {
			log.Println("scan pending dingtalk upload failed:", err)
			continue
		}
		client := clients.clientFor(item.OrgKey)
		if client == nil {
			log.Printf("skip dingtalk upload %d: organization %q is not configured", item.ID, item.OrgKey)
			continue
		}
		processOneDingTalkUpload(ctx, client, item)
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
