// Package session 实现服务端会话：签发、校验（含续期）、吊销。
//
// 设计要点（docs/auth-redesign.md §5.4）：
//   - 服务端只保存 sha256(token)，明文永不落库（不变量 I3）；
//   - 双过期：idle 可续、absolute 不可破；
//   - 续期只在 idle 剩余不足阈值时发生，避免每个请求都写库；
//   - 吊销按 user_id 批量执行，使"改邮箱/停用/被踢"立即生效。
package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/secure"
	"gorm.io/gorm"
)

// 校验失败的原因。对外一律映射为统一的 401 文案（设计 §6.6），
// 这些哨兵错误只用于内部判断与测试。
var (
	ErrInvalid = errors.New("session: invalid token")
	ErrExpired = errors.New("session: token expired")
	ErrRevoked = errors.New("session: token revoked")
)

// Config 是会话策略。零值可用 DefaultConfig 填充。
type Config struct {
	// IdleTTL 是普通用户会话的空闲有效期。
	IdleTTL time.Duration
	// AbsoluteTTL 是普通用户会话的绝对有效期（续期不可突破）。
	AbsoluteTTL time.Duration
	// RenewWindow 是**用户会话**的续期阈值：idle 剩余不足该值时续期。
	// 为 0 时取 IdleTTL/2（7 天 → 3.5 天）。
	RenewWindow time.Duration

	// AdminRenewWindow 是**管理会话**的续期阈值，为 0 时取 AdminIdleTTL/2（30 分钟 → 15 分钟）。
	//
	// 必须按 scope 分别取值：管理会话 idle 只有 30 分钟，若沿用用户会话的 3.5 天阈值，
	// "剩余不足 3.5 天"永远成立 ⇒ 每次校验都写一次库（实测确认过这个写放大）。
	AdminRenewWindow time.Duration

	// AdminIdleTTL / AdminAbsoluteTTL 是管理会话的策略（更短）。
	AdminIdleTTL     time.Duration
	AdminAbsoluteTTL time.Duration

	// Now 可注入时钟，便于测试；为 nil 时使用 UTC 当前时间。
	Now func() time.Time
}

// DefaultConfig 返回设计文档 §5.4 确定的默认策略。
func DefaultConfig() Config {
	return Config{
		IdleTTL:          7 * 24 * time.Hour,
		AbsoluteTTL:      30 * 24 * time.Hour,
		AdminIdleTTL:     30 * time.Minute,
		AdminAbsoluteTTL: 8 * time.Hour,
	}
}

func (c Config) withDefaults() Config {
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}

	// 顺序很关键：先把两个 scope 的基准 TTL 补全，再据此推导续期阈值。
	// （曾把 AdminRenewWindow 的推导写在 AdminIdleTTL 之前，结果阈值恒为 0，
	// "剩余不足 0"永远成立 ⇒ 管理会话**永不续期**，30 分钟必定掉线。）
	if c.IdleTTL <= 0 {
		c.IdleTTL = 7 * 24 * time.Hour
	}
	if c.AbsoluteTTL <= 0 {
		c.AbsoluteTTL = 30 * 24 * time.Hour
	}
	if c.AdminIdleTTL <= 0 {
		c.AdminIdleTTL = 30 * time.Minute
	}
	if c.AdminAbsoluteTTL <= 0 {
		c.AdminAbsoluteTTL = 8 * time.Hour
	}

	if c.RenewWindow <= 0 {
		c.RenewWindow = c.IdleTTL / 2
	}
	if c.AdminRenewWindow <= 0 {
		c.AdminRenewWindow = c.AdminIdleTTL / 2
	}
	return c
}

// Service 是会话服务。它不持有任何包级状态，可安全地被多个实例使用（L2）。
type Service struct {
	db  *gorm.DB
	cfg Config
}

// New 构造会话服务。
func New(db *gorm.DB, cfg Config) *Service {
	return &Service{db: db, cfg: cfg.withDefaults()}
}

// WithTx 返回绑定到同一事务的副本，供编排层在一个事务里组合多个服务。
func (s *Service) WithTx(tx *gorm.DB) *Service {
	c := *s
	c.db = tx
	return &c
}

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// renewWindowFor 返回给定作用域的续期阈值。
func (s *Service) renewWindowFor(scope string) time.Duration {
	if scope == domain.ScopeAdmin {
		return s.cfg.AdminRenewWindow
	}
	return s.cfg.RenewWindow
}

// ttlFor 返回给定作用域的空闲/绝对有效期。
func (s *Service) ttlFor(scope string) (idle, absolute time.Duration) {
	if scope == domain.ScopeAdmin {
		return s.cfg.AdminIdleTTL, s.cfg.AdminAbsoluteTTL
	}
	return s.cfg.IdleTTL, s.cfg.AbsoluteTTL
}

// IssueParams 是签发会话所需的上下文。
type IssueParams struct {
	UserID int64
	UID    string
	Scope  string // domain.ScopeUser / domain.ScopeAdmin
	IP     string
	UAHash string
	// Device 是签发时要一并落库的设备信息（由调用方解析 UA 后传入）。
	//
	// 零值表示"没有设备信息"：此时 device_id 记 NULL，读出来归入"未知设备"。
	Device DeviceInfo
}

// DeviceInfo 是设备信息的**列形状**（与 sessions 表的 device_* 列一一对应）。
//
// 之所以不直接用 internal/device.Info：本包只关心"往哪些列里写什么"，
// 解析规则属于上层，包的依赖方向保持不变。
type DeviceInfo struct {
	// ID 是**加盐哈希**后的设备指纹，空串表示未知（落库为 NULL）。
	ID string
	// Type / Model / OS / Browser 是给人看的展示字段。
	Type    string
	Model   string
	OS      string
	Browser string
}

// Issued 是签发结果。Plain 只在此处出现一次。
type Issued struct {
	Plain   string
	Session *domain.Session
}

// Issue 签发一个新会话。
func (s *Service) Issue(ctx context.Context, p IssueParams) (*Issued, error) {
	if p.UserID == 0 {
		return nil, errors.New("session: UserID is required")
	}
	scope := p.Scope
	if scope == "" {
		scope = domain.ScopeUser
	}

	plain, hash, err := secure.NewToken()
	if err != nil {
		return nil, err
	}

	now := s.now()
	idleTTL, absTTL := s.ttlFor(scope)
	rec := &domain.Session{
		TokenHash:         hash,
		UserID:            p.UserID,
		UID:               p.UID,
		Scope:             scope,
		IssuedAt:          now,
		IdleExpiresAt:     now.Add(idleTTL),
		AbsoluteExpiresAt: now.Add(absTTL),
		LastSeenAt:        now,
		IP:                optString(p.IP),
		UAHash:            optString(p.UAHash),
		DeviceID:          optString(p.Device.ID),
		DeviceType:        optString(p.Device.Type),
		DeviceModel:       optString(p.Device.Model),
		DeviceOS:          optString(p.Device.OS),
		DeviceBrowser:     optString(p.Device.Browser),
	}
	if err := s.db.WithContext(ctx).Create(rec).Error; err != nil {
		return nil, fmt.Errorf("session: create: %w", err)
	}
	return &Issued{Plain: plain, Session: rec}, nil
}

// Verify 校验 token 并返回会话。命中且满足续期条件时会顺带续期。
func (s *Service) Verify(ctx context.Context, plain string) (*domain.Session, error) {
	if plain == "" {
		return nil, ErrInvalid
	}

	var rec domain.Session
	err := s.db.WithContext(ctx).
		Where("token_hash = ?", secure.HashToken(plain)).
		First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("session: lookup: %w", err)
	}

	now := s.now()
	if rec.RevokedAt != nil {
		return nil, ErrRevoked
	}
	if !now.Before(rec.AbsoluteExpiresAt) {
		return nil, ErrExpired
	}
	if !now.Before(rec.IdleExpiresAt) {
		return nil, ErrExpired
	}

	if err := s.maybeRenew(ctx, &rec, now); err != nil {
		// 续期失败不应导致校验失败：会话本身仍然有效。
		// 这里不返回错误，把可用性放在首位；失败会在下一次请求重试。
		_ = err
	}
	return &rec, nil
}

// maybeRenew 在 idle 剩余不足阈值时续期，且永不突破绝对有效期。
func (s *Service) maybeRenew(ctx context.Context, rec *domain.Session, now time.Time) error {
	if rec.IdleExpiresAt.Sub(now) >= s.renewWindowFor(rec.Scope) {
		return nil
	}
	idleTTL, _ := s.ttlFor(rec.Scope)

	next := now.Add(idleTTL)
	if next.After(rec.AbsoluteExpiresAt) {
		next = rec.AbsoluteExpiresAt
	}

	res := s.db.WithContext(ctx).
		Model(&domain.Session{}).
		Where("token_hash = ? AND revoked_at IS NULL", rec.TokenHash).
		Updates(map[string]any{
			"idle_expires_at": next,
			"last_seen_at":    now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 1 {
		rec.IdleExpiresAt = next
		rec.LastSeenAt = now
	}
	return nil
}

// Revoke 吊销单个会话（logout）。对不存在的 token 返回 nil（幂等）。
func (s *Service) Revoke(ctx context.Context, plain, reason string) error {
	if plain == "" {
		return ErrInvalid
	}
	now := s.now()
	res := s.db.WithContext(ctx).
		Model(&domain.Session{}).
		Where("token_hash = ? AND revoked_at IS NULL", secure.HashToken(plain)).
		Updates(map[string]any{
			"revoked_at":    now,
			"revoke_reason": optString(reason),
		})
	if res.Error != nil {
		return fmt.Errorf("session: revoke: %w", res.Error)
	}
	return nil
}

// RevokeAllForUser 吊销某用户的全部有效会话，返回被吊销的数量。
//
// 这是"改邮箱 / 停用 / 管理员踢人"的实现基础：由于两处代码共享同一个
// 数据库，吊销在下一次请求即生效（§5.4）。
func (s *Service) RevokeAllForUser(ctx context.Context, userID int64, reason string) (int64, error) {
	if userID == 0 {
		return 0, errors.New("session: UserID is required")
	}
	now := s.now()
	res := s.db.WithContext(ctx).
		Model(&domain.Session{}).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Updates(map[string]any{
			"revoked_at":    now,
			"revoke_reason": optString(reason),
		})
	if res.Error != nil {
		return 0, fmt.Errorf("session: revoke all: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// --- 在线设备 ---

// ActiveDevice 是一台"在线设备"：同一 (scope, 设备指纹) 下所有活跃会话的聚合。
//
// 为什么必须按设备聚合、而不是把会话直接列出来：验证码登录每次都会签发**新**会话
// 且不吊销旧的，同一个浏览器反复登录会攒出一串会话。直接列会话，界面上就会把
// "一台手机"显示成"十台设备"——这是这个界面最容易骗人的地方。
type ActiveDevice struct {
	// DeviceID 是设备指纹的加盐哈希；旧数据与识别不出来的设备为空串。
	DeviceID string
	// Scope 区分用户会话与管理会话：同一台电脑上二者是两条独立记录。
	Scope string
	// Type / Model / OS / Browser 是展示字段，可能为空（历史会话没有）。
	Type    string
	Model   string
	OS      string
	Browser string
	// IP 取该设备**最近一次**会话的来源地址（移动网络下会变，因此不参与聚合）。
	IP string
	// Sessions 是该设备当前仍有效的会话数（重复登录会累加）。
	Sessions int
	// LoginAt 是最近一次登入，FirstLoginAt 是这批在线会话里最早的一次。
	LoginAt      time.Time
	FirstLoginAt time.Time
	// LastSeenAt / ExpiresAt 取该设备最新一条会话的值。
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// UnknownDeviceID 是"没有设备指纹"的会话在聚合时的归类键，
// Go 侧的 groupDevices 与 SQL 侧的 COUNT(DISTINCT ...) 必须用同一个值。
const UnknownDeviceID = "unknown"

// activeSessionClause 是"此刻仍然在线"的唯一判据。
//
// 必须把 idle 过期也算进去：只判 revoked_at IS NULL 会把"早就闲置过期、
// 只是还没被 DeleteExpired 清掉"的会话算成在线设备（会话是按需清理的）。
const activeSessionClause = `revoked_at IS NULL AND idle_expires_at > ? AND absolute_expires_at > ?`

// ListActiveDevices 返回某用户当前的在线设备（按最近登入时间倒序）。
func (s *Service) ListActiveDevices(ctx context.Context, userID int64) ([]ActiveDevice, error) {
	if userID == 0 {
		return nil, errors.New("session: UserID is required")
	}
	now := s.now()
	var rows []domain.Session
	err := s.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Where(activeSessionClause, now, now).
		Order("issued_at DESC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("session: list active devices: %w", err)
	}
	return groupDevices(rows), nil
}

// CountActiveDevicesByUsers 返回一批用户各自的在线设备数（用于用户列表的徽标）。
//
// 一次分组查询解决：用户列表最多 500 行，逐行查询会是 N+1。
func (s *Service) CountActiveDevicesByUsers(ctx context.Context, userIDs []int64) (map[int64]int, error) {
	out := map[int64]int{}
	ids := uniqueIDs(userIDs)
	if len(ids) == 0 {
		return out, nil
	}
	now := s.now()

	var rows []struct {
		UserID  int64
		Devices int
	}
	// 聚合键必须与 groupDevices 完全一致：scope + 设备指纹（NULL/空 → unknown）。
	err := s.db.WithContext(ctx).
		Model(&domain.Session{}).
		Select(`user_id, COUNT(DISTINCT scope || ':' || COALESCE(NULLIF(device_id, ''), ?)) AS devices`, UnknownDeviceID).
		Where("user_id IN ?", ids).
		Where(activeSessionClause, now, now).
		Group("user_id").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("session: count active devices: %w", err)
	}
	for _, row := range rows {
		out[row.UserID] = row.Devices
	}
	return out, nil
}

// groupDevices 把同一用户的活跃会话按 (scope, 设备指纹) 聚合。
//
// rows 必须按 issued_at 倒序：每组的第一行就是最新的那条会话，展示字段与 IP
// 以它为准；更早的会话只在字段为空时补位（历史数据进行过迁移，可能没有设备字段）。
func groupDevices(rows []domain.Session) []ActiveDevice {
	out := make([]ActiveDevice, 0, len(rows))
	index := make(map[string]int, len(rows))

	for _, r := range rows {
		key := r.Scope + ":" + deviceKeyOf(r)
		at, ok := index[key]
		if !ok {
			out = append(out, ActiveDevice{
				DeviceID:     derefString(r.DeviceID),
				Scope:        r.Scope,
				Type:         derefString(r.DeviceType),
				Model:        derefString(r.DeviceModel),
				OS:           derefString(r.DeviceOS),
				Browser:      derefString(r.DeviceBrowser),
				IP:           derefString(r.IP),
				LoginAt:      r.IssuedAt,
				FirstLoginAt: r.IssuedAt,
				LastSeenAt:   r.LastSeenAt,
				ExpiresAt:    r.IdleExpiresAt,
			})
			at = len(out) - 1
			index[key] = at
		}
		d := &out[at]
		d.Sessions++
		if r.IssuedAt.Before(d.FirstLoginAt) {
			d.FirstLoginAt = r.IssuedAt
		}
		if r.IssuedAt.After(d.LoginAt) {
			d.LoginAt = r.IssuedAt
		}
		if r.LastSeenAt.After(d.LastSeenAt) {
			d.LastSeenAt = r.LastSeenAt
		}
		if r.IdleExpiresAt.After(d.ExpiresAt) {
			d.ExpiresAt = r.IdleExpiresAt
		}
		fillString(&d.Type, r.DeviceType)
		fillString(&d.Model, r.DeviceModel)
		fillString(&d.OS, r.DeviceOS)
		fillString(&d.Browser, r.DeviceBrowser)
		fillString(&d.IP, r.IP)
	}
	return out
}

func deviceKeyOf(r domain.Session) string {
	if id := derefString(r.DeviceID); id != "" {
		return id
	}
	return UnknownDeviceID
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func fillString(dst *string, src *string) {
	if *dst == "" && src != nil {
		*dst = *src
	}
}

func uniqueIDs(ids []int64) []int64 {
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// DeleteExpired 清理已过期或已吊销超过 30 天的会话，返回删除行数。
func (s *Service) DeleteExpired(ctx context.Context) (int64, error) {
	now := s.now()
	res := s.db.WithContext(ctx).
		Where("absolute_expires_at < ? OR (revoked_at IS NOT NULL AND revoked_at < ?)",
			now, now.Add(-30*24*time.Hour)).
		Delete(&domain.Session{})
	if res.Error != nil {
		return 0, fmt.Errorf("session: delete expired: %w", res.Error)
	}
	return res.RowsAffected, nil
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
