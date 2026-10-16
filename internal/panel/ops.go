// ops.go 账号运维端点：单号刷 token、凭证脱敏预览、批量导入、签到状态、排程状态。
//
// 移植来源：linguo2625469/workbuddy2api-panel PR #183 里那套 TraE 面板运维 API
// （`internal/trae/server/{accounts,admin}.go`）。按本仓管线重写：凭证一律走
// parseCallback → completeLogin（不另造一套换票/落盘），region 由凭证自带 host 判，
// 不做 PR 那种写死 trae.cn / api.trae.com.cn 的写法。
package panel

import (
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"traework2api/internal/auth"
)

// maskSecret 只回前缀 + 长度，绝不回完整 token（与 overview 同口径）。
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "«" + strconv.Itoa(len(s)) + " 字符»"
	}
	return s[:8] + "…«" + strconv.Itoa(len(s)) + " 字符»"
}

// accountRefreshToken 单号刷 token（只动这一个号，省掉整池保活）。
func (p *Panel) accountRefreshToken(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if err := p.cfg.Upstream.RefreshToken(a); err != nil {
		writeErr(w, http.StatusBadGateway, publicErr(err))
		return
	}
	// 刷新成功即落盘（不落盘 = 进程重启后带着旧 refreshToken 回来，被判 session 失效）。
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "换到新票但落盘失败")
		return
	}
	p.cfg.Pool.ClearSessionDead(uid) // 新票在手，session 失效计数清零（同调度器口径）
	st, _ := p.cfg.Pool.Status(uid)
	log.Printf("panel: refresh token uid=%s", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": st, "expires_at": a.ExpiresAtValue()})
}

// accountCredential 盘上凭证的脱敏预览（排障用：这份文件到底是谁、什么时候过期）。
func (p *Panel) accountCredential(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	path, err := authPath(p.cfg.AuthDir, uid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad uid")
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "凭证文件不存在")
		return
	}
	a, err := auth.Parse(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "凭证文件解析失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": a.UID, "nickname": a.Nickname, "realm": a.Realm(),
		"domain": a.Domain, "api_host": a.ApiHost, "file": path,
		"expires_at":    a.ExpiresAtValue(),
		"access_token":  maskSecret(a.JWT()),
		"refresh_token": maskSecret(a.RefreshTokenValue()),
		"machine_id":    a.MachineID, "device_id": a.DeviceID, "device_seed": a.DeviceSeedValue(),
	})
}

// credentialOnDiskNewer 盘中该 uid 已有一份**更新且未过期**的凭证吗。
// 旧备份覆盖刚登录的新会话 = 真实事故（PR #183 accounts.go:139-155 同一条）：旧 token 顶掉
// 新票 → session dead。新件到期时间未知/已过期时无从比较，不拦。
func credentialOnDiskNewer(dir, uid string, newExpire int64) bool {
	if newExpire <= time.Now().Unix() {
		return false
	}
	path, err := authPath(dir, uid)
	if err != nil {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	a, err := auth.Parse(raw)
	if err != nil {
		return false
	}
	old := a.ExpiresAtValue()
	return old > newExpire && old > time.Now().Unix()
}

// accountImport 批量导入凭证：body 是「回调链接」或「凭证 JSON」，裸文本直接判前缀。
// 与面板「添加账号」共用 completeLogin（换票 → GetUserInfo → 落盘 → 进池）。
func (p *Panel) accountImport(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body failed")
		return
	}
	trim := strings.TrimSpace(string(raw))
	if trim == "" {
		writeErr(w, http.StatusBadRequest, "请求体为空")
		return
	}
	var cb callbackCreds
	if strings.HasPrefix(trim, "{") {
		a, perr := auth.Parse([]byte(trim))
		if perr != nil {
			writeErr(w, http.StatusBadRequest, "凭证 JSON 解析失败："+perr.Error()+
				"（只有 refreshToken 的备份请用「添加账号 → refreshToken 直登」）")
			return
		}
		cb = callbackCreds{
			Access: a.JWT(), Refresh: a.RefreshTokenValue(), Expires: a.ExpiresAtValue(),
			UID: a.UID, Nickname: a.Nickname, Ent: a.EnterpriseID, Host: a.ApiHost,
		}
	} else if cb, err = parseCallback(trim); err != nil {
		writeErr(w, http.StatusBadRequest, "回调链接解析失败："+err.Error())
		return
	}
	// 已知 uid 才做「旧备份不覆盖」预检；回调里没有 userInfo 时不知道 uid，交给
	// completeLogin 的「上游未确认不许覆盖已有账号」那道闸。
	if cb.UID != "" && credentialOnDiskNewer(p.cfg.AuthDir, cb.UID, cb.Expires) {
		log.Printf("panel: import skipped uid=%s（盘中凭证更新）", cb.UID)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "action": "skipped_existing_newer", "uid": cb.UID,
			"message": "盘中已有更新的凭证，未覆盖",
		})
		return
	}
	// 一次性登录会话：completeLogin 从会话里取 machine/device（新导入的号用全新设备号）。
	machine, e1 := randHex(16)
	device, e2 := randHex(16)
	id, e3 := randHex(16)
	if e1 != nil || e2 != nil || e3 != nil {
		writeErr(w, http.StatusInternalServerError, "random failed")
		return
	}
	p.loginMu.Lock()
	if p.logins == nil {
		p.logins = map[string]loginSession{}
	}
	p.logins[id] = loginSession{Machine: machine, Device: device, At: time.Now()}
	p.loginMu.Unlock()

	uid, nick, code, err := p.completeLogin(id, cb)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "imported", "uid": uid, "nickname": nick})
}

// checkinStatus 批量签到状态（**只读**，只查 status 不 claim）：面板点「签到状态」时查一次。
func (p *Panel) checkinStatus(w http.ResponseWriter, r *http.Request) {
	type row struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname,omitempty"`
		Realm    string `json:"realm,omitempty"`
		Checked  bool   `json:"checked_in"`
		Credits  int64  `json:"credits,omitempty"`
		Enable   bool   `json:"enable"`
		Skipped  string `json:"skipped,omitempty"`
		Error    string `json:"error,omitempty"`
	}
	out := []row{}
	checked, unchecked, failed := 0, 0, 0
	for _, st := range p.cfg.Pool.List() {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		rw := row{UID: st.UID, Nickname: st.Nickname, Realm: st.Realm}
		switch {
		case a.Realm() == auth.RealmIntl:
			rw.Skipped = "国际版没有签到接口"
		default:
			ci, credits, enable, err := p.cfg.Upstream.CheckinStatus(a)
			switch {
			case err != nil:
				rw.Error = publicErr(err)
				failed++
			default:
				rw.Checked, rw.Credits, rw.Enable = ci, credits, enable
				if ci {
					checked++
				} else {
					unchecked++
				}
			}
		}
		out = append(out, rw)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "checked": checked, "unchecked": unchecked, "failed": failed, "accounts": out,
	})
}
