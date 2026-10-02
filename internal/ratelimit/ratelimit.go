// Package ratelimit 实现多维度限流。
//
// 存储分工（docs/auth-redesign.md §6.2、§7.2）：
//   - **安全关键计数落 SQLite**（`mail_log`）：邮箱维度与全局熔断。
//     原因是进程内计数器会在重启后清零，而本库存在派生的 panic 重启路径记忆；
//   - **IP 维度用进程内窗口粗筛**：重启清零可接受，且避免高频攻击把
//     写压力放大到数据库。全局熔断是它的兜底。
//
// 约定：限流计数的是**请求**而非"实际发出的邮件"。冷却期复用（见
// internal/otp）会让同一窗口内的重复请求不产生邮件，但这里仍按请求计数——
// 这是有意为之的保守取值，宁可略严也不放过刷量。
package ratelimit

import (
	"context"
	"sync"
	"time"

	"github.com/tangthinker/user-center/v2/internal/secure"
	"gorm.io/gorm"
)

// Config 是限流参数。
type Config struct {
	// 邮箱维度（数据库计数）。
	EmailPerHour int
	EmailPerDay  int

	// IP 维度（进程内粗筛）。
	IPPerHour int
	IPPerDay  int

	// VerifyPerIPPerHour 限制单个 IP 的验证码校验次数。
	VerifyPerIPPerHour int

	// GlobalDailyCap 是全局日发信熔断阈值；0 表示不启用。
	GlobalDailyCap int

	// AdminInvitePerHour 限制管理员每小时发出的邀请数。
	AdminInvitePerHour int

	// AdminAPIPerMinute 限制 admin 接口的整体频率。
	AdminAPIPerMinute int

	// Now 可注入时钟。
	Now func() time.Time
}

// DefaultConfig 返回设计文档 §6.2 / §7.2 的默认值。
func DefaultConfig() Config {
	return Config{
		EmailPerHour:       5,
		EmailPerDay:        10,
		IPPerHour:          10,
		IPPerDay:           30,
		VerifyPerIPPerHour: 30,
		GlobalDailyCap:     100,
		AdminInvitePerHour: 20,
		AdminAPIPerMinute:  60,
	}
}

// withDefaults 把**零值**替换为默认限额；显式传**负数**表示关闭该维度。
//
// 这是刻意选择的安全默认：忘记配置不应该等于"完全不限流"。
// （开发中曾因零值即"不限流"导致连续 7 次发码全部放行，端到端验证时才发现。）
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}

	pick := func(v, def int) int {
		switch {
		case v == 0:
			return def
		case v < 0:
			return 0 // 显式关闭
		default:
			return v
		}
	}

	c.EmailPerHour = pick(c.EmailPerHour, d.EmailPerHour)
	c.EmailPerDay = pick(c.EmailPerDay, d.EmailPerDay)
	c.IPPerHour = pick(c.IPPerHour, d.IPPerHour)
	c.IPPerDay = pick(c.IPPerDay, d.IPPerDay)
	c.VerifyPerIPPerHour = pick(c.VerifyPerIPPerHour, d.VerifyPerIPPerHour)
	c.GlobalDailyCap = pick(c.GlobalDailyCap, d.GlobalDailyCap)
	c.AdminInvitePerHour = pick(c.AdminInvitePerHour, d.AdminInvitePerHour)
	c.AdminAPIPerMinute = pick(c.AdminAPIPerMinute, d.AdminAPIPerMinute)
	return c
}

// Decision 是一次限流判定结果。
type Decision struct {
	Allowed    bool
	Reason     string
	RetryAfter time.Duration
}

// Service 是限流服务。
type Service struct {
	db  *gorm.DB
	cfg Config
	mem *memCounter
}

// New 构造限流服务。
func New(db *gorm.DB, cfg Config) *Service {
	return &Service{db: db, cfg: cfg.withDefaults(), mem: newMemCounter()}
}

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// SendParams 是一次发信请求的维度。
type SendParams struct {
	Email   string // 已归一化
	Purpose string
	IP      string
}

// AllowSend 判定是否允许为该邮箱发送验证码/邀请，并记录本次请求。
//
// 判定顺序：IP 粗筛 → 全局熔断 → 邮箱小时/日额。被拒绝的请求也会记入
// mail_log（accepted=0），既便于观测，也不占用配额。
func (s *Service) AllowSend(ctx context.Context, p SendParams) (*Decision, error) {
	now := s.now()
	emailHash := secure.HashEmail(p.Email)

	if d := s.checkIP(p.IP, now); !d.Allowed {
		if err := s.recordMail(ctx, emailHash, p.Purpose, p.IP, false, now); err != nil {
			return nil, err
		}
		return &d, nil
	}

	var decision Decision
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if s.cfg.GlobalDailyCap > 0 {
			global, err := countAccepted(tx, "", "", now.Add(-24*time.Hour))
			if err != nil {
				return err
			}
			if int(global) >= s.cfg.GlobalDailyCap {
				decision = Decision{Reason: "global_day", RetryAfter: time.Hour}
				return insertMailLog(tx, emailHash, p.Purpose, p.IP, false, now)
			}
		}

		hour, err := countAccepted(tx, emailHash, p.Purpose, now.Add(-time.Hour))
		if err != nil {
			return err
		}
		if s.cfg.EmailPerHour > 0 && int(hour) >= s.cfg.EmailPerHour {
			decision = Decision{Reason: "email_hour", RetryAfter: time.Hour}
			return insertMailLog(tx, emailHash, p.Purpose, p.IP, false, now)
		}

		day, err := countAccepted(tx, emailHash, p.Purpose, now.Add(-24*time.Hour))
		if err != nil {
			return err
		}
		if s.cfg.EmailPerDay > 0 && int(day) >= s.cfg.EmailPerDay {
			decision = Decision{Reason: "email_day", RetryAfter: time.Hour}
			return insertMailLog(tx, emailHash, p.Purpose, p.IP, false, now)
		}

		decision = Decision{Allowed: true}
		return insertMailLog(tx, emailHash, p.Purpose, p.IP, true, now)
	})
	if err != nil {
		return nil, err
	}
	return &decision, nil
}

func (s *Service) checkIP(ip string, now time.Time) Decision {
	if ip == "" {
		return Decision{Allowed: true}
	}
	if d := s.mem.allow("send:ip:hour:"+ip, s.cfg.IPPerHour, time.Hour, now); !d.Allowed {
		d.Reason = "ip_hour"
		return d
	}
	if d := s.mem.allow("send:ip:day:"+ip, s.cfg.IPPerDay, 24*time.Hour, now); !d.Allowed {
		d.Reason = "ip_day"
		return d
	}
	return Decision{Allowed: true}
}

// AllowVerifyByIP 限制单个 IP 的验证码校验频率。
func (s *Service) AllowVerifyByIP(ip string) Decision {
	if ip == "" {
		return Decision{Allowed: true}
	}
	d := s.mem.allow("verify:ip:hour:"+ip, s.cfg.VerifyPerIPPerHour, time.Hour, s.now())
	if !d.Allowed {
		d.Reason = "verify_ip_hour"
	}
	return d
}

// AllowAdminInvite 限制管理员发出邀请的频率。
func (s *Service) AllowAdminInvite(actorID int64) Decision {
	key := "admin:invite:" + itoa(actorID)
	d := s.mem.allow(key, s.cfg.AdminInvitePerHour, time.Hour, s.now())
	if !d.Allowed {
		d.Reason = "admin_invite_hour"
	}
	return d
}

// AllowAdminAPI 限制 admin 接口整体频率（按调用方标识，通常是本地地址）。
func (s *Service) AllowAdminAPI(key string) Decision {
	if key == "" {
		key = "local"
	}
	d := s.mem.allow("admin:api:"+key, s.cfg.AdminAPIPerMinute, time.Minute, s.now())
	if !d.Allowed {
		d.Reason = "admin_api_minute"
	}
	return d
}

// SentInLast24h 返回滚动 24 小时内被接受的发信请求数（熔断与告警的数据源）。
func (s *Service) SentInLast24h(ctx context.Context) (int64, error) {
	return countAccepted(s.db.WithContext(ctx), "", "", s.now().Add(-24*time.Hour))
}

// CleanupMailLog 清理保留期之外的发信日志，返回删除行数。
func (s *Service) CleanupMailLog(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		retention = 180 * 24 * time.Hour
	}
	res := s.db.WithContext(ctx).
		Exec(`DELETE FROM mail_log WHERE created_at < ?`, s.now().Add(-retention))
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}

func (s *Service) recordMail(ctx context.Context, emailHash, purpose, ip string, accepted bool, now time.Time) error {
	return insertMailLog(s.db.WithContext(ctx), emailHash, purpose, ip, accepted, now)
}

func countAccepted(tx *gorm.DB, emailHash, purpose string, since time.Time) (int64, error) {
	var count int64
	q := tx.Table("mail_log").Where("accepted = 1 AND created_at > ?", since)
	if emailHash != "" {
		q = q.Where("email_hash = ?", emailHash)
	}
	if purpose != "" {
		q = q.Where("purpose = ?", purpose)
	}
	if err := q.Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func insertMailLog(tx *gorm.DB, emailHash, purpose, ip string, accepted bool, now time.Time) error {
	var ipVal any
	if ip != "" {
		ipVal = ip
	}
	return tx.Exec(
		`INSERT INTO mail_log (email_hash, purpose, ip, accepted, created_at) VALUES (?, ?, ?, ?, ?)`,
		emailHash, purpose, ipVal, accepted, now,
	).Error
}

// memCounter 是进程内的固定窗口计数器。
type memCounter struct {
	mu      sync.Mutex
	buckets map[string]*memBucket
}

type memBucket struct {
	start time.Time
	count int
}

func newMemCounter() *memCounter {
	return &memCounter{buckets: make(map[string]*memBucket)}
}

// allow 在固定窗口内计数；拒绝时返回距窗口重置的时间。
func (m *memCounter) allow(key string, limit int, window time.Duration, now time.Time) Decision {
	if limit <= 0 {
		return Decision{Allowed: true}
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.buckets) > 4096 {
		m.sweepLocked(now)
	}

	b, ok := m.buckets[key]
	if !ok || now.Sub(b.start) >= window {
		m.buckets[key] = &memBucket{start: now, count: 1}
		return Decision{Allowed: true}
	}
	if b.count >= limit {
		return Decision{Allowed: false, RetryAfter: b.start.Add(window).Sub(now)}
	}
	b.count++
	return Decision{Allowed: true}
}

// sweepLocked 清理已过期的桶，防止攻击者用海量不同 key 撑爆内存。
func (m *memCounter) sweepLocked(now time.Time) {
	for k, b := range m.buckets {
		if now.Sub(b.start) >= 24*time.Hour {
			delete(m.buckets, k)
		}
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
