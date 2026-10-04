// Package domain 定义 user-center 的持久化模型与领域常量。
//
// 这些结构体只用于描述存储形状（GORM 映射）；表的创建与变更由
// internal/migrate 用显式 SQL 负责，**不使用 GORM AutoMigrate**
// （见 docs/auth-redesign.md §4.4）。
package domain

import (
	"strings"
	"time"
)

// 用户状态。invited → active 是单向的；disabled 可由管理员切换。
const (
	UserStatusInvited  = "invited"
	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"
)

// 一次性验证码与邀请的用途。**用途必须隔离**：
// login 的码绝不能通过 admin_login 的校验（设计 §5.3）。
const (
	PurposeLogin      = "login"
	PurposeAdminLogin = "admin_login"
	PurposeInvite     = "invite"
	// PurposeAdminUID 是"管理员设置自己的用户名"的邀请。
	// 与普通邀请共用同一套令牌机制与落地页，只是用途可区分（便于审计与排查）。
	PurposeAdminUID   = "admin_uid"
	PurposeEmailReset = "email_change"
)

// 会话作用域。
const (
	ScopeUser  = "user"
	ScopeAdmin = "admin"
)

// 邮件发件箱状态。
const (
	MailPending = "pending"
	MailSending = "sending"
	MailSent    = "sent"
	MailFailed  = "failed"
)

// MailReservedKey 是发件箱抢占用的固定值，便于测试与排查。
const MailReservedKey = "sending"

// User 是账号主体。
//
// 注意：v2 不使用软删除（没有 DeletedAt），因此 email 唯一索引不会出现
// "被软删的用户永久占住邮箱"的陷阱（设计 §4.1 与 §8 坑 #1）。
type User struct {
	ID          int64      `gorm:"column:id;primaryKey;autoIncrement"`
	UID         *string    `gorm:"column:uid"` // 激活时由用户设定；激活前为 NULL；之后不可变
	Email       string     `gorm:"column:email"`
	Status      string     `gorm:"column:status"`
	IsAdmin     bool       `gorm:"column:is_admin"`
	ActivatedAt *time.Time `gorm:"column:activated_at"`
	LastLoginAt *time.Time `gorm:"column:last_login_at"`
	DisabledAt  *time.Time `gorm:"column:disabled_at"`
	CreatedBy   *int64     `gorm:"column:created_by"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
}

func (User) TableName() string { return "users" }

// IsActive 表示该用户可以登录。
func (u *User) IsActive() bool { return u.Status == UserStatusActive }

// UIDValue 返回 uid 的字符串形式（未激活时为空串）。
func (u *User) UIDValue() string {
	if u.UID == nil {
		return ""
	}
	return *u.UID
}

// Invitation 是一次性邀请。明文 token 永不落库，只存 sha256。
type Invitation struct {
	TokenHash  []byte     `gorm:"column:token_hash;primaryKey"`
	UserID     int64      `gorm:"column:user_id"`
	Email      string     `gorm:"column:email"`
	Purpose    string     `gorm:"column:purpose"`
	IssuedAt   time.Time  `gorm:"column:issued_at"`
	ExpiresAt  time.Time  `gorm:"column:expires_at"`
	ConsumedAt *time.Time `gorm:"column:consumed_at"`
	CreatedBy  int64      `gorm:"column:created_by"`
	SentCount  int        `gorm:"column:sent_count"`
	LastSentAt *time.Time `gorm:"column:last_sent_at"`
	CheckCount int        `gorm:"column:check_count"`
}

func (Invitation) TableName() string { return "invitations" }

// OTPCode 是一次性验证码。只存 HMAC，不存明文。
type OTPCode struct {
	ID         int64      `gorm:"column:id;primaryKey;autoIncrement"`
	Email      string     `gorm:"column:email"`
	Purpose    string     `gorm:"column:purpose"`
	CodeHMAC   []byte     `gorm:"column:code_hmac"`
	IssuedAt   time.Time  `gorm:"column:issued_at"`
	ExpiresAt  time.Time  `gorm:"column:expires_at"`
	Attempts   int        `gorm:"column:attempts"`
	ConsumedAt *time.Time `gorm:"column:consumed_at"`
	RequestIP  *string    `gorm:"column:request_ip"`
}

func (OTPCode) TableName() string { return "otp_codes" }

// Session 是服务端会话。只存 sha256(token)。
//
// 设备信息（device_*）在签发时从 UA 解析后落库（**原始 UA 永不落库**，只留
// ua_hash）：device_id 是不含版本号的设备指纹的加盐哈希，用于把"同一个客户端
// 反复登录"归并成一台设备；其余四列是给人看的展示字段。
type Session struct {
	TokenHash         []byte     `gorm:"column:token_hash;primaryKey"`
	UserID            int64      `gorm:"column:user_id"`
	UID               string     `gorm:"column:uid"`
	Scope             string     `gorm:"column:scope"`
	IssuedAt          time.Time  `gorm:"column:issued_at"`
	IdleExpiresAt     time.Time  `gorm:"column:idle_expires_at"`
	AbsoluteExpiresAt time.Time  `gorm:"column:absolute_expires_at"`
	LastSeenAt        time.Time  `gorm:"column:last_seen_at"`
	IP                *string    `gorm:"column:ip"`
	UAHash            *string    `gorm:"column:ua_hash"`
	DeviceID          *string    `gorm:"column:device_id"`
	DeviceType        *string    `gorm:"column:device_type"`
	DeviceModel       *string    `gorm:"column:device_model"`
	DeviceOS          *string    `gorm:"column:device_os"`
	DeviceBrowser     *string    `gorm:"column:device_browser"`
	RevokedAt         *time.Time `gorm:"column:revoked_at"`
	RevokeReason      *string    `gorm:"column:revoke_reason"`
}

func (Session) TableName() string { return "sessions" }

// MailOutbox 是持久化发件队列（设计 §7.3）。
type MailOutbox struct {
	ID            int64      `gorm:"column:id;primaryKey;autoIncrement"`
	DedupeKey     string     `gorm:"column:dedupe_key"`
	ToEmail       string     `gorm:"column:to_email"`
	Template      string     `gorm:"column:template"`
	Payload       *string    `gorm:"column:payload"`
	Priority      int        `gorm:"column:priority"`
	Status        string     `gorm:"column:status"`
	Attempts      int        `gorm:"column:attempts"`
	NextAttemptAt time.Time  `gorm:"column:next_attempt_at"`
	LastError     *string    `gorm:"column:last_error"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	SentAt        *time.Time `gorm:"column:sent_at"`
}

func (MailOutbox) TableName() string { return "mail_outbox" }

// MailLog 记录每一次发信请求（含被限流拒绝的），是配额统计的数据源。
type MailLog struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement"`
	EmailHash string    `gorm:"column:email_hash"`
	Purpose   string    `gorm:"column:purpose"`
	IP        *string   `gorm:"column:ip"`
	Accepted  bool      `gorm:"column:accepted"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (MailLog) TableName() string { return "mail_log" }

// AdminAudit 是管理动作审计。detail 内**禁止**写入 token / 验证码。
type AdminAudit struct {
	ID         int64     `gorm:"column:id;primaryKey;autoIncrement"`
	ActorID    int64     `gorm:"column:actor_id"`
	ActorEmail string    `gorm:"column:actor_email"`
	Action     string    `gorm:"column:action"`
	Target     *string   `gorm:"column:target"`
	Detail     *string   `gorm:"column:detail"`
	IP         *string   `gorm:"column:ip"`
	UA         *string   `gorm:"column:ua"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

func (AdminAudit) TableName() string { return "admin_audit" }

// NormalizeEmail 归一化邮箱：去空白 + 转小写。
// 这是 email 唯一性与查表的前置条件（设计 §4.2 I1 相关）。
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
