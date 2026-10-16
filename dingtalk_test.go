package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDingTalkClientGetsAndCachesAccessToken(t *testing.T) {
	var tokenCalls int32
	var uploadCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gettoken":
			atomic.AddInt32(&tokenCalls, 1)
			if r.URL.Query().Get("appkey") != "client-id" || r.URL.Query().Get("appsecret") != "client-secret" {
				t.Errorf("token query = %v", r.URL.Query())
			}
			_, _ = w.Write([]byte(`{"errcode":0,"access_token":"cached-token","expires_in":7200}`))
		case "/upload":
			atomic.AddInt32(&uploadCalls, 1)
			if got := r.URL.Query().Get("access_token"); got != "cached-token" {
				t.Errorf("upload access_token = %q", got)
			}
			_, _ = w.Write([]byte(`{"errcode":0,"success":true,"request_id":"req-cache"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &DingTalkClient{
		clientID:      "client-id",
		clientSecret:  "client-secret",
		tokenEndpoint: server.URL + "/gettoken",
		endpoint:      server.URL + "/upload",
		deviceName:    "sync",
		deviceID:      "device-001",
		format:        "form",
		httpClient:    server.Client(),
	}
	checkTime := time.Now().Add(-time.Minute)
	if _, err := client.Upload(context.Background(), "ding-user-001", checkTime, ""); err != nil {
		t.Fatalf("first Upload() error = %v", err)
	}
	if _, err := client.Upload(context.Background(), "ding-user-001", checkTime, ""); err != nil {
		t.Fatalf("second Upload() error = %v", err)
	}
	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Fatalf("token calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&uploadCalls); got != 2 {
		t.Fatalf("upload calls = %d, want 2", got)
	}
}

func TestDingTalkUploadUsesConfiguredFormFields(t *testing.T) {
	checkTime := time.Now().Add(-2 * time.Minute).Truncate(time.Millisecond)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Query().Get("access_token") != "test-access-token" {
			t.Errorf("access_token = %q", r.URL.Query().Get("access_token"))
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("content type = %q", r.Header.Get("Content-Type"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		checks := map[string]string{
			"userid":          "ding-user-001",
			"device_name":     "外部系统同步",
			"device_id":       "manual_sync_001",
			"user_check_time": strconv.FormatInt(checkTime.UnixMilli(), 10),
		}
		for key, want := range checks {
			if got := form.Get(key); got != want {
				t.Errorf("form %s = %q, want %q", key, got, want)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"success":true,"request_id":"req-123"}`))
	}))
	defer server.Close()

	client := &DingTalkClient{
		accessToken: "test-access-token",
		endpoint:    server.URL,
		deviceName:  "外部系统同步",
		deviceID:    "manual_sync_001",
		format:      "form",
		httpClient:  server.Client(),
	}
	result, err := client.Upload(context.Background(), "ding-user-001", checkTime, "")
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if result.RequestID != "req-123" {
		t.Fatalf("RequestID = %q, want req-123", result.RequestID)
	}
}

func TestDingTalkUploadRejectsOldTime(t *testing.T) {
	client := &DingTalkClient{accessToken: "test-access-token", endpoint: "http://127.0.0.1:1", httpClient: http.DefaultClient}
	_, err := client.Upload(context.Background(), "ding-user-001", time.Now().Add(-181*24*time.Hour), "")
	if err == nil || !strings.Contains(err.Error(), "180 days") {
		t.Fatalf("Upload() error = %v, want 180-day validation error", err)
	}
}

func TestDingTalkResolveUserIDByMobile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type = %q", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"mobile":"13800138000"}` {
			t.Errorf("request body = %s", body)
		}
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok","result":{"userid":"ding-user-001"}}`))
	}))
	defer server.Close()

	client := &DingTalkClient{
		accessToken:  "test-access-token",
		userEndpoint: server.URL,
		httpClient:   server.Client(),
	}
	got, err := client.ResolveUserIDByMobile(context.Background(), "13800138000")
	if err != nil {
		t.Fatalf("ResolveUserIDByMobile() error = %v", err)
	}
	if got != "ding-user-001" {
		t.Fatalf("userid = %q, want ding-user-001", got)
	}
}

func TestDingTalkListUsersPaginates(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("access_token") != "test-access-token" {
			t.Errorf("access_token = %q", r.URL.Query().Get("access_token"))
		}
		body, _ := io.ReadAll(r.Body)
		var request dingTalkUserListRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		call := atomic.AddInt32(&calls, 1)
		switch call {
		case 1:
			if request.DeptID != 1 || request.Cursor != 0 || request.Size != 100 {
				t.Fatalf("first request = %+v", request)
			}
			_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok","result":{"has_more":true,"next_cursor":100,"list":[{"userid":"u1","mobile":"13800000001"}]}}`))
		case 2:
			if request.Cursor != 100 {
				t.Fatalf("second cursor = %d, want 100", request.Cursor)
			}
			_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok","result":{"has_more":false,"next_cursor":0,"list":[{"userid":"u2","mobile":"13800000002"}]}}`))
		default:
			t.Fatal("unexpected extra page")
		}
	}))
	defer server.Close()

	client := &DingTalkClient{
		accessToken:      "test-access-token",
		userListEndpoint: server.URL,
		rootDeptID:       1,
		httpClient:       server.Client(),
	}
	users, err := client.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("ListUsers() error = %v", err)
	}
	if len(users) != 2 || users[0].UserID != "u1" || users[1].UserID != "u2" {
		t.Fatalf("users = %+v", users)
	}
}

func TestNewDingTalkHTTPClientDefaultsToDirectConnection(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:9")
	t.Setenv("DINGTALK_PROXY_URL", "")

	client, err := newDingTalkHTTPClient()
	if err != nil {
		t.Fatalf("newDingTalkHTTPClient() error = %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("default DingTalk client must not inherit process proxy")
	}
}

func TestNewDingTalkHTTPClientUsesExplicitProxy(t *testing.T) {
	t.Setenv("DINGTALK_PROXY_URL", "http://proxy.example:8080")
	client, err := newDingTalkHTTPClient()
	if err != nil {
		t.Fatalf("newDingTalkHTTPClient() error = %v", err)
	}
	transport := client.Transport.(*http.Transport)
	if transport.Proxy == nil {
		t.Fatal("explicit proxy was not configured")
	}
	proxyReq, err := transport.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "oapi.dingtalk.com"}})
	if err != nil {
		t.Fatalf("proxy function error = %v", err)
	}
	if proxyReq == nil || proxyReq.Host != "proxy.example:8080" {
		t.Fatalf("proxy URL = %v, want proxy.example:8080", proxyReq)
	}
}
