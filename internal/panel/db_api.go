package panel

import (
	"context"
	"net/http"
	"time"

	"github.com/solitudeit/nfr/internal/api"
)

func (p *Panel) apiGetDB(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.srv.DBStatus())
}

func (p *Panel) apiTestDB(w http.ResponseWriter, r *http.Request) {
	var req api.DBConfigRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ver, err := p.srv.TestMySQL(ctx, req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":      false,
			"error":   err.Error(),
			"message": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, api.DBTestResponse{
		OK:      true,
		Version: ver,
		Message: "连接成功，建表与写入权限正常",
	})
}

func (p *Panel) apiSetupDB(w http.ResponseWriter, r *http.Request) {
	var req api.DBConfigRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	adopted, err := p.srv.SwitchToMySQL(ctx, req)
	if err != nil {
		writeSrvErr(w, err)
		return
	}
	reauth := adopted && p.current(r) == nil
	msg := "MySQL 数据库初始化完成，现有数据已迁移并切换至 MySQL 存储"
	if adopted {
		msg = "已接入现有 MySQL 数据库并保留库内原有数据"
	}
	writeJSON(w, http.StatusOK, api.DBSetupResponse{
		OK:              true,
		AdoptedExisting: adopted,
		ReauthRequired:  reauth,
		Status:          p.srv.DBStatus(),
		Message:         msg,
	})
}
