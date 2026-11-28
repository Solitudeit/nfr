package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/auth"
	"github.com/solitudeit/nfr/internal/config"
	"github.com/solitudeit/nfr/internal/store"
)

// SetOnStateReload registers a callback fired after the active store state is
// replaced (for example after adopting an existing MySQL database).
func (s *Server) SetOnStateReload(fn func()) {
	s.mu.Lock()
	s.onStateReload = fn
	s.mu.Unlock()
}

// SetOnUserRevoke registers a callback fired when a user's panel sessions must
// be revoked immediately (disable, delete, or admin password reset).
func (s *Server) SetOnUserRevoke(fn func(userID string)) {
	s.mu.Lock()
	s.onUserRevoke = fn
	s.mu.Unlock()
}

func (s *Server) invokeUserRevoke(userID string) {
	s.mu.Lock()
	fn := s.onUserRevoke
	s.mu.Unlock()
	if fn != nil {
		fn(userID)
	}
}

func (s *Server) invokeStateReload() {
	s.mu.Lock()
	fn := s.onStateReload
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// AdminUser returns the sole administrator account.
func (s *Server) AdminUser() (store.User, bool) {
	st := s.st.Snapshot()
	u, ok := st.AdminUser()
	if !ok {
		return store.User{}, false
	}
	return *u, true
}

// UserByID returns one user record by ID.
func (s *Server) UserByID(id string) (store.User, bool) {
	st := s.st.Snapshot()
	u, ok := st.UserByID(id)
	if !ok {
		return store.User{}, false
	}
	return *u, true
}

// UserAuthToken returns the active client auth_token for userID. If userID is
// the admin and server config/env pins auth_token, the pinned token takes
// precedence.
func (s *Server) UserAuthToken(userID string) string {
	st := s.st.Snapshot()
	u, ok := st.UserByID(userID)
	if !ok {
		return ""
	}
	if u.Role == store.RoleAdmin {
		if pinned := strings.TrimSpace(s.cfg.AuthToken); pinned != "" {
			return pinned
		}
		if u.AuthToken != "" {
			return u.AuthToken
		}
		return st.Settings.AuthToken
	}
	return u.AuthToken
}

// AuthenticatePanel verifies username + password for web console sign-in.
// An empty username defaults to the sole admin user so legacy scripts continue
// working seamlessly.
func (s *Server) AuthenticatePanel(username, password string) (store.User, error) {
	st := s.st.Snapshot()
	username = strings.TrimSpace(username)
	var u *store.User
	var ok bool
	if username == "" {
		u, ok = st.AdminUser()
	} else {
		u, ok = st.UserByUsername(username)
	}
	if !ok {
		return store.User{}, errStatusUnauthorized("用户名或密码错误")
	}
	if !config.CheckPassword(u.PasswordHash, password) {
		return store.User{}, errStatusUnauthorized("用户名或密码错误")
	}
	if u.Disabled {
		return store.User{}, errForbidden("user_disabled", "该账号已被管理员停用")
	}
	return *u, nil
}

func errStatusUnauthorized(msg string) error {
	return &errStatus{status: http.StatusUnauthorized, code: "unauthorized", msg: msg}
}

func validateUsername(name string) (string, error) {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	if n < 2 || n > 32 {
		return "", errBad("username", "用户名长度需在 2 到 32 个字符之间")
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return "", errBad("username", "用户名仅支持字母、数字、点、下划线和短横线")
		}
	}
	return name, nil
}

func validateQuotaBounds(maxTunnels, maxBW int, monthlyMB int64) error {
	if maxTunnels < 0 || maxTunnels > 10000 {
		return errBad("max_tunnels", "最大隧道数必须在 0 到 10000 之间")
	}
	if maxBW < 0 || maxBW > 4<<20 {
		return errBad("max_bandwidth_kb", "带宽上限必须在 0 到 4194304 KB/s 之间")
	}
	if monthlyMB < 0 || monthlyMB > 10<<20 {
		return errBad("monthly_quota_mb", "月流量配额必须在 0 到 10485760 MB 之间")
	}
	return nil
}

func (s *Server) renderUser(st *store.State, u store.User, onlineNodes map[string]bool, includeToken bool) api.User {
	tunnels, onlineTunnels := 0, 0
	for _, t := range st.Tunnels {
		if t.OwnerID != u.ID {
			continue
		}
		tunnels++
		if t.Enabled && t.NodeID != "" && onlineNodes[t.NodeID] {
			onlineTunnels++
		}
	}
	liveIn, liveOut := s.unflushedTrafficForOwner(st, u.ID)
	out := api.User{
		ID:             u.ID,
		Username:       u.Username,
		Role:           u.Role,
		Disabled:       u.Disabled,
		MaxTunnels:     u.MaxTunnels,
		MaxBandwidthKB: u.MaxBandwidthKB,
		MonthlyQuotaMB: u.MonthlyQuotaMB,
		UsedBytes:      u.UsedBytes + liveIn + liveOut,
		UsageMonth:     u.UsageMonth,
		TotalInBytes:   u.TotalInBytes + liveIn,
		TotalOutBytes:  u.TotalOutBytes + liveOut,
		TunnelCount:    tunnels,
		OnlineTunnels:  onlineTunnels,
		HasAvatar:      len(u.AvatarData) > 0 && u.AvatarMime != "",
		CreatedAt:      u.CreatedAt,
		UpdatedAt:      u.UpdatedAt,
	}
	if out.HasAvatar {
		out.AvatarURL = fmt.Sprintf("/nfr/api/avatars/%s?v=%d", u.ID, u.AvatarVer)
	}
	if includeToken {
		if u.Role == store.RoleAdmin && strings.TrimSpace(s.cfg.AuthToken) != "" {
			out.AuthToken = strings.TrimSpace(s.cfg.AuthToken)
		} else {
			out.AuthToken = u.AuthToken
		}
	}
	return out
}

func (s *Server) onlineNodeMap() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[string]bool, len(s.agents))
	for id := range s.agents {
		m[id] = true
	}
	return m
}

// UserProfile returns the profile DTO for userID (including their auth_token).
func (s *Server) UserProfile(userID string) (api.User, bool) {
	st := s.st.Snapshot()
	u, ok := st.UserByID(userID)
	if !ok {
		return api.User{}, false
	}
	return s.renderUser(&st, *u, s.onlineNodeMap(), true), true
}

// ListUsers returns all accounts for the admin user management tab.
func (s *Server) ListUsers() []api.User {
	st := s.st.Snapshot()
	online := s.onlineNodeMap()
	out := make([]api.User, 0, len(st.Users))
	for _, u := range st.Users {
		out = append(out, s.renderUser(&st, u, online, false))
	}
	return out
}

// GetUserDetail returns one user plus all of their tunnels and nodes.
func (s *Server) GetUserDetail(id string) (api.UserDetail, error) {
	st := s.st.Snapshot()
	u, ok := st.UserByID(id)
	if !ok {
		return api.UserDetail{}, errNotFound("用户不存在")
	}
	online := s.onlineNodeMap()
	return api.UserDetail{
		User:    s.renderUser(&st, *u, online, false),
		Tunnels: s.TunnelsForOwner(u.ID),
		Nodes:   s.NodesForOwner(u.ID),
	}, nil
}

// CreateUser creates a new normal user account with optional quotas.
func (s *Server) CreateUser(req api.CreateUserRequest) (api.User, error) {
	username, err := validateUsername(req.Username)
	if err != nil {
		return api.User{}, err
	}
	if len(req.Password) < 6 {
		return api.User{}, errBad("password", "密码长度至少需要 6 位")
	}
	if err := validateQuotaBounds(req.MaxTunnels, req.MaxBandwidthKB, req.MonthlyQuotaMB); err != nil {
		return api.User{}, err
	}
	hash, err := config.HashPassword(req.Password)
	if err != nil {
		return api.User{}, err
	}
	now := time.Now().UTC()
	var created store.User
	err = s.st.Update(func(st *store.State) error {
		if _, exists := st.UserByUsername(username); exists {
			return errConflict("username_taken", "用户名已存在")
		}
		created = store.User{
			ID:             store.NewID(),
			Username:       username,
			Role:           store.RoleUser,
			PasswordHash:   hash,
			AuthToken:      auth.NewSecret("nfr_t_", 18),
			MaxTunnels:     req.MaxTunnels,
			MaxBandwidthKB: req.MaxBandwidthKB,
			MonthlyQuotaMB: req.MonthlyQuotaMB,
			UsageMonth:     now.Format("2006-01"),
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		st.Users = append(st.Users, created)
		return nil
	})
	if err != nil {
		return api.User{}, err
	}
	st := s.st.Snapshot()
	s.notify("user created: " + created.Username)
	return s.renderUser(&st, created, s.onlineNodeMap(), false), nil
}

// UpdateUserQuotaAndStatus updates a user's quotas and/or disabled state.
func (s *Server) UpdateUserQuotaAndStatus(id string, req api.UpdateUserRequest) (api.User, error) {
	var (
		becameDisabled bool
		bwChanged      bool
		resumedAny     bool
		userNodeIDs    []string
		userTunnelIDs  []string
		updated        store.User
	)
	now := time.Now().UTC()
	month := now.Format("2006-01")

	err := s.st.Update(func(st *store.State) error {
		u, ok := st.UserByID(id)
		if !ok {
			return errNotFound("用户不存在")
		}
		if req.Disabled != nil && *req.Disabled && u.Role == store.RoleAdmin {
			return errBad("disabled", "不能停用管理员账号")
		}
		nextTunnels := u.MaxTunnels
		nextBW := u.MaxBandwidthKB
		nextQuota := u.MonthlyQuotaMB
		if req.MaxTunnels != nil {
			nextTunnels = *req.MaxTunnels
		}
		if req.MaxBandwidthKB != nil {
			nextBW = *req.MaxBandwidthKB
		}
		if req.MonthlyQuotaMB != nil {
			nextQuota = *req.MonthlyQuotaMB
		}
		if err := validateQuotaBounds(nextTunnels, nextBW, nextQuota); err != nil {
			return err
		}

		if nextBW != u.MaxBandwidthKB {
			bwChanged = true
		}
		if req.Disabled != nil && *req.Disabled && !u.Disabled {
			becameDisabled = true
		}
		u.MaxTunnels = nextTunnels
		u.MaxBandwidthKB = nextBW
		u.MonthlyQuotaMB = nextQuota
		if req.Disabled != nil {
			u.Disabled = *req.Disabled
		}
		u.UpdatedAt = now

		for _, n := range st.Nodes {
			if n.OwnerID == u.ID {
				userNodeIDs = append(userNodeIDs, n.ID)
			}
		}
		pausedAny := false
		for i := range st.Tunnels {
			t := &st.Tunnels[i]
			if t.OwnerID != u.ID {
				continue
			}
			userTunnelIDs = append(userTunnelIDs, t.ID)
			overUserQuota := u.Role != store.RoleAdmin && u.MonthlyQuotaMB > 0 && u.UsageMonth == month && u.UsedBytes > u.MonthlyQuotaMB<<20
			if overUserQuota && t.Enabled {
				t.Enabled = false
				t.QuotaPaused = true
				t.QuotaReason = "user"
				t.UpdatedAt = now
				pausedAny = true
			} else if !u.Disabled && !overUserQuota && t.QuotaPaused && t.QuotaReason == "user" {
				overTunnelQuota := t.QuotaMB > 0 && t.UsageMonth == month && t.UsedBytes > t.QuotaMB<<20
				if !overTunnelQuota {
					t.QuotaPaused = false
					t.QuotaReason = ""
					t.Enabled = true
					t.UpdatedAt = now
					resumedAny = true
				}
			}
		}
		if pausedAny {
			resumedAny = true // force reconcile + pushAll below
		}
		if becameDisabled {
			sessions := st.PanelSessions[:0]
			for _, ps := range st.PanelSessions {
				if ps.UserID != u.ID {
					sessions = append(sessions, ps)
				}
			}
			st.PanelSessions = sessions
		}
		updated = *u
		return nil
	})
	if err != nil {
		return api.User{}, err
	}

	if bwChanged {
		for _, tid := range userTunnelIDs {
			s.clearLimitBucket(tid)
		}
	}
	if becameDisabled {
		s.kickNodes(userNodeIDs, "账号已被管理员停用")
		s.invokeUserRevoke(id)
	}
	if becameDisabled || resumedAny || bwChanged || req.Disabled != nil {
		s.reconcile()
		s.pushAll()
	}
	st := s.st.Snapshot()
	s.notify("user updated: " + updated.Username)
	return s.renderUser(&st, updated, s.onlineNodeMap(), false), nil
}

// ResetUserPassword lets an admin overwrite a user's password and revokes their
// existing panel sessions.
func (s *Server) ResetUserPassword(id, newPassword string) error {
	if len(newPassword) < 6 {
		return errBad("password", "新密码长度至少需要 6 位")
	}
	hash, err := config.HashPassword(newPassword)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	err = s.st.Update(func(st *store.State) error {
		u, ok := st.UserByID(id)
		if !ok {
			return errNotFound("用户不存在")
		}
		u.PasswordHash = hash
		u.UpdatedAt = now
		if u.Role == store.RoleAdmin {
			st.PanelPassword = hash
		}
		sessions := st.PanelSessions[:0]
		for _, ps := range st.PanelSessions {
			if ps.UserID != u.ID {
				sessions = append(sessions, ps)
			}
		}
		st.PanelSessions = sessions
		return nil
	})
	if err != nil {
		return err
	}
	s.invokeUserRevoke(id)
	return nil
}

// DeleteUser removes a non-admin account along with all of its tunnels, nodes,
// leases, join codes, and panel sessions.
func (s *Server) DeleteUser(id string) error {
	var (
		username      string
		removedNodes  []string
		removedTuns   []string
	)
	err := s.st.Update(func(st *store.State) error {
		idx := -1
		for i := range st.Users {
			if st.Users[i].ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return errNotFound("用户不存在")
		}
		if st.Users[idx].Role == store.RoleAdmin {
			return errBad("role", "不能删除管理员账号")
		}
		username = st.Users[idx].Username
		st.Users = append(st.Users[:idx], st.Users[idx+1:]...)

		tunSet := map[string]bool{}
		tunnels := make([]store.Tunnel, 0, len(st.Tunnels))
		for _, t := range st.Tunnels {
			if t.OwnerID == id {
				removedTuns = append(removedTuns, t.ID)
				tunSet[t.ID] = true
				continue
			}
			tunnels = append(tunnels, t)
		}
		st.Tunnels = tunnels

		nodes := make([]store.Node, 0, len(st.Nodes))
		for _, n := range st.Nodes {
			if n.OwnerID == id {
				removedNodes = append(removedNodes, n.ID)
				continue
			}
			nodes = append(nodes, n)
		}
		st.Nodes = nodes

		leases := make([]store.Lease, 0, len(st.Leases))
		for _, l := range st.Leases {
			if !tunSet[l.TunnelID] {
				leases = append(leases, l)
			}
		}
		st.Leases = leases

		joins := make([]auth.JoinCode, 0, len(st.Joins))
		for _, j := range st.Joins {
			if j.OwnerID != id && !tunSet[j.TunnelID] {
				joins = append(joins, j)
			}
		}
		st.Joins = joins

		sessions := make([]store.PanelSession, 0, len(st.PanelSessions))
		for _, ps := range st.PanelSessions {
			if ps.UserID != id {
				sessions = append(sessions, ps)
			}
		}
		st.PanelSessions = sessions
		return nil
	})
	if err != nil {
		return err
	}
	s.kickNodes(removedNodes, "账号已被管理员删除")
	for _, tid := range removedTuns {
		s.clearTargetDown(tid)
		s.met.Drop(tid)
		s.dropLimit(tid)
	}
	s.invokeUserRevoke(id)
	s.reconcile()
	s.notify("user deleted: " + username)
	return nil
}

// UpdateProfile updates the signed-in user's username and/or password.
func (s *Server) UpdateProfile(userID string, req api.ProfileUpdateRequest) (api.User, error) {
	now := time.Now().UTC()
	var updated store.User
	err := s.st.Update(func(st *store.State) error {
		u, ok := st.UserByID(userID)
		if !ok {
			return errNotFound("用户不存在")
		}
		if req.Username != "" && !strings.EqualFold(strings.TrimSpace(req.Username), u.Username) {
			name, err := validateUsername(req.Username)
			if err != nil {
				return err
			}
			if other, exists := st.UserByUsername(name); exists && other.ID != u.ID {
				return errConflict("username_taken", "该用户名已被占用")
			}
			u.Username = name
		}
		if req.NewPassword != "" {
			if !config.CheckPassword(u.PasswordHash, req.OldPassword) {
				return errBad("old_password", "当前密码不正确")
			}
			if len(req.NewPassword) < 6 {
				return errBad("new_password", "新密码长度至少需要 6 位")
			}
			hash, err := config.HashPassword(req.NewPassword)
			if err != nil {
				return err
			}
			u.PasswordHash = hash
			if u.Role == store.RoleAdmin {
				st.PanelPassword = hash
			}
		}
		u.UpdatedAt = now
		updated = *u
		return nil
	})
	if err != nil {
		return api.User{}, err
	}
	st := s.st.Snapshot()
	return s.renderUser(&st, updated, s.onlineNodeMap(), true), nil
}

// RotateUserAuthToken generates a new client auth_token for userID and kicks
// all of that user's live agent connections.
func (s *Server) RotateUserAuthToken(userID string) (string, error) {
	var (
		newTok      string
		userNodeIDs []string
	)
	now := time.Now().UTC()
	err := s.st.Update(func(st *store.State) error {
		u, ok := st.UserByID(userID)
		if !ok {
			return errNotFound("用户不存在")
		}
		if u.Role == store.RoleAdmin && s.AuthTokenFromConfig() {
			return errBad("auth_token", "管理员密钥已由配置文件或环境变量锁定，无法在线重置")
		}
		newTok = auth.NewSecret("nfr_t_", 18)
		u.AuthToken = newTok
		u.UpdatedAt = now
		if u.Role == store.RoleAdmin {
			st.Settings.AuthToken = newTok
		}
		for _, n := range st.Nodes {
			if n.OwnerID == u.ID {
				userNodeIDs = append(userNodeIDs, n.ID)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	s.kickNodes(userNodeIDs, "连接密钥已重置，请更新客户端配置")
	s.notifyOwner(userID, "auth token rotated")
	return newTok, nil
}

// SetUserAvatar persists a validated avatar image for userID.
func (s *Server) SetUserAvatar(userID, mime string, data []byte) (int64, error) {
	now := time.Now().UTC()
	ver := now.UnixNano()
	err := s.st.Update(func(st *store.State) error {
		u, ok := st.UserByID(userID)
		if !ok {
			return errNotFound("用户不存在")
		}
		u.AvatarMime = mime
		u.AvatarData = append([]byte(nil), data...)
		u.AvatarVer = ver
		u.UpdatedAt = now
		return nil
	})
	return ver, err
}

// ClearUserAvatar removes userID's uploaded avatar.
func (s *Server) ClearUserAvatar(userID string) error {
	now := time.Now().UTC()
	return s.st.Update(func(st *store.State) error {
		u, ok := st.UserByID(userID)
		if !ok {
			return errNotFound("用户不存在")
		}
		u.AvatarMime = ""
		u.AvatarData = nil
		u.AvatarVer = 0
		u.UpdatedAt = now
		return nil
	})
}

// GetUserAvatar returns the stored avatar bytes and MIME type for userID.
func (s *Server) GetUserAvatar(userID string) (mime string, data []byte, ver int64, ok bool) {
	st := s.st.Snapshot()
	u, found := st.UserByID(userID)
	if !found || len(u.AvatarData) == 0 || u.AvatarMime == "" {
		return "", nil, 0, false
	}
	return u.AvatarMime, append([]byte(nil), u.AvatarData...), u.AvatarVer, true
}

func (s *Server) kickNodes(nodeIDs []string, reason string) {
	for _, nid := range nodeIDs {
		if a := s.sessionFor(nid); a != nil {
			a.kick(reason)
		}
	}
}

// DBStatus returns the active persistence backend status plus whether the
// mandatory post-login MySQL setup modal must be shown to admins.
func (s *Server) DBStatus() api.DBStatus {
	raw := s.st.DBStatus()
	fileModeOptOut := strings.EqualFold(strings.TrimSpace(os.Getenv("NFR_STORAGE")), "file")
	return api.DBStatus{
		Mode:           raw.Mode,
		Configured:     raw.Configured,
		FromConfig:     raw.FromConfig,
		MandatorySetup: !raw.Configured && !fileModeOptOut,
		Host:           raw.Host,
		Port:           raw.Port,
		Database:       raw.Database,
		Username:       raw.Username,
		HasPassword:    raw.HasPassword,
		Version:        raw.Version,
		Error:          raw.Error,
	}
}

// TestMySQL checks connectivity and DDL privileges on the supplied MySQL target.
func (s *Server) TestMySQL(ctx context.Context, req api.DBConfigRequest) (string, error) {
	return s.st.TestMySQL(ctx, store.DBConfig{
		Host:     req.Host,
		Port:     req.Port,
		Database: req.Database,
		Username: req.Username,
		Password: req.Password,
	})
}

// SwitchToMySQL connects and initializes/adopts a MySQL database, reconciles
// live listeners, and triggers panel session reload.
func (s *Server) SwitchToMySQL(ctx context.Context, req api.DBConfigRequest) (adoptedExisting bool, err error) {
	adopted, err := s.st.SwitchToMySQL(ctx, store.DBConfig{
		Host:     req.Host,
		Port:     req.Port,
		Database: req.Database,
		Username: req.Username,
		Password: req.Password,
	})
	if err != nil {
		return false, err
	}
	// Ensure legacy/empty admin normalization still holds after adoption.
	_, _ = s.st.UpdateIf(func(st *store.State) (bool, error) {
		return st.NormalizeLegacyUsers(), nil
	})
	s.reconcile()
	s.pushAll()
	if adopted {
		s.invokeStateReload()
	}
	return adopted, nil
}
