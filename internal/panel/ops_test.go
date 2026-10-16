package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
	"traework2api/internal/scheduler"
	"traework2api/internal/upstream"
)

const opsKey = "k"

func doOps(t *testing.T, p *Panel, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+opsKey)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// writeCred 落一份真凭证文件（走 auth.SaveAtomic，与线上同格式）。
func writeCred(t *testing.T, dir, uid, access string, expires int64) string {
	t.Helper()
	a := &auth.Auth{
		UID: uid, Nickname: "甲", AccessToken: access, RefreshToken: "rt-" + uid,
		ExpiresAt: expires, Domain: "trae.cn",
		FilePath: filepath.Join(dir, "trae-"+uid+".json"),
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	return a.FilePath
}

// TestOpsRoutesRequireKey 新加的运维端点必须和别的面板接口一样吃鉴权（不然就是越权读凭证/刷票）。
func TestOpsRoutesRequireKey(t *testing.T) {
	p := New(Config{Pool: pool.New(""), AuthDir: t.TempDir(), APIKey: opsKey, Version: "test"})
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/panel/api/accounts/u1/refresh"},
		{http.MethodGet, "/panel/api/accounts/u1/json"},
		{http.MethodPost, "/panel/api/accounts/import"},
		{http.MethodGet, "/panel/api/checkin_status"},
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 没鉴权也放行: code=%d", c.method, c.path, rec.Code)
		}
		rec = doOps(t, p, c.method, c.path, "{}")
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s %s 带密钥仍被拒", c.method, c.path)
		}
	}
}

// TestCredentialPreviewMasksTokens 凭证预览只回前缀与长度：整串 token 一个字符都不许出。
func TestCredentialPreviewMasksTokens(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "u1", "SUPERSECRET-ACCESS-TOKEN-VALUE", time.Now().Add(48*time.Hour).Unix())
	p := New(Config{Pool: pool.New(""), AuthDir: dir, APIKey: opsKey})

	rec := doOps(t, p, http.MethodGet, "/panel/api/accounts/u1/json", "")
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "SUPERSECRET-ACCESS-TOKEN-VALUE") || strings.Contains(body, "rt-u1") {
		t.Fatalf("预览回出了完整凭证: %s", body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got["access_token"].(string), "…«") {
		t.Errorf("access_token 没脱敏: %v", got["access_token"])
	}
	if got["uid"] != "u1" || got["nickname"] != "甲" {
		t.Errorf("预览缺账号信息: %v", got)
	}
}

// TestImportSkipsNewerExistingCredential 旧备份不许覆盖盘中更新且未过期的凭证（PR #183 那条真实事故）。
func TestImportSkipsNewerExistingCredential(t *testing.T) {
	dir := t.TempDir()
	path := writeCred(t, dir, "u1", "NEW-LIVE-ACCESS", time.Now().Add(30*24*time.Hour).Unix())
	p := New(Config{Pool: pool.New(""), AuthDir: dir, APIKey: opsKey})

	older := `{"accessToken":"OLD-BACKUP-ACCESS","refreshToken":"rt-old","expiresAt":` +
		strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10) +
		`,"uid":"u1","nickname":"甲"}`
	rec := doOps(t, p, http.MethodPost, "/panel/api/accounts/import", older)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "skipped_existing_newer") {
		t.Fatalf("旧备份没有被拦下: code=%d body=%s", rec.Code, rec.Body)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "NEW-LIVE-ACCESS") {
		t.Fatalf("盘中凭证被旧备份覆盖了: %s", raw)
	}
}

// TestImportEmptyBodyRejected 空请求体要报错，不能静默成功。
func TestImportEmptyBodyRejected(t *testing.T) {
	p := New(Config{Pool: pool.New(""), AuthDir: t.TempDir(), APIKey: opsKey})
	rec := doOps(t, p, http.MethodPost, "/panel/api/accounts/import", "   ")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestAccountRefreshTokenPersists 单号刷票：换到的新票必须落盘（不落盘 = 重启回来还是旧票）。
func TestAccountRefreshTokenPersists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Result":{"Token":"at-NEW","RefreshToken":"rt-NEW","TokenExpireAt":1900000000}}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	path := writeCred(t, dir, "u1", "at-OLD", 1)
	pl := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at-OLD", RefreshToken: "rt-OLD", ExpiresAt: 1,
		Domain: "trae.cn", ApiHost: srv.URL, FilePath: path}
	pl.Add(a)
	p := New(Config{Pool: pl, Upstream: &upstream.Client{HTTP: srv.Client(), ClientID: upstream.ClientID},
		AuthDir: dir, APIKey: opsKey})

	rec := doOps(t, p, http.MethodPost, "/panel/api/accounts/u1/refresh", "")
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "at-NEW") {
		t.Fatalf("新票没落盘: %s", raw)
	}
	if a.ExpiresAtValue() != 1900000000 {
		t.Errorf("内存里的到期时间没更新: %d", a.ExpiresAtValue())
	}
}

// TestCheckinStatusSweep 批量签到状态：只读（一次 claim 都不许发），国际号标 skipped。
func TestCheckinStatusSweep(t *testing.T) {
	var claims atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/status"):
			_, _ = w.Write([]byte(`{"checked_in":true,"credits":150,"enable":true}`))
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/claim"):
			claims.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
		}
	}))
	defer srv.Close()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "甲", AccessToken: "at", ExpiresAt: 9999999999})
	pl.Add(&auth.Auth{UID: "u2", Nickname: "乙", AccessToken: "at", ExpiresAt: 9999999999})
	pl.Add(&auth.Auth{UID: "i1", Nickname: "丙", AccessToken: "at", ExpiresAt: 9999999999,
		Domain: "trae.ai", ApiHost: "https://api-sg-central.trae.ai"})
	p := New(Config{Pool: pl, Upstream: &upstream.Client{HTTP: srv.Client(), UgHost: srv.URL}, APIKey: opsKey})

	rec := doOps(t, p, http.MethodGet, "/panel/api/checkin_status", "")
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got struct {
		Checked, Unchecked, Failed int
		Accounts                   []struct {
			UID, Realm, Skipped string
			Checked             bool
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Checked != 2 || got.Unchecked != 0 || got.Failed != 0 || len(got.Accounts) != 3 {
		t.Fatalf("汇总不对: %+v", got)
	}
	skipped := 0
	for _, a := range got.Accounts {
		if a.Skipped != "" {
			skipped++
		}
	}
	if skipped != 1 {
		t.Errorf("国际号应标 skipped: %+v", got.Accounts)
	}
	if n := claims.Load(); n != 0 {
		t.Fatalf("只读查询发了 %d 次 claim（会真签到）", n)
	}
}

// TestOverviewPublishesSchedule overview 要带排程现状与 token 到期（面板「下次签到」卡与 Token 列的数据源）。
func TestOverviewPublishesSchedule(t *testing.T) {
	dir := t.TempDir()
	exp := time.Now().Add(72 * time.Hour).Unix()
	writeCred(t, dir, "u1", "at", exp)
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: exp})
	sch := scheduler.New(scheduler.Config{Pool: pl, CheckinHours: []int{9}, KeepaliveHours: []int{3},
		CheckinEnabled: true, KeepaliveEnabled: true})
	p := New(Config{Pool: pl, Scheduler: sch, AuthDir: dir, APIKey: opsKey})

	rec := doOps(t, p, http.MethodGet, "/panel/api/overview", "")
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var got struct {
		Schedule map[string]any `json:"schedule"`
		Accounts []struct {
			UID            string `json:"uid"`
			TokenExpiresAt int64  `json:"token_expires_at"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Schedule == nil || got.Schedule["checkin_next"] == "" {
		t.Fatalf("overview 没带排程: %v", got.Schedule)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].TokenExpiresAt != exp {
		t.Fatalf("账号行没带 token 到期: %+v", got.Accounts)
	}
}
