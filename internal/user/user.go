// Package user 是用户仓储：读写 users 表的最小集合。
//
// 它只做数据访问与少量原子语义（如"是否首次登录"），
// 业务流程编排在 internal/app 中完成。
package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tangthinker/user-center/v2/internal/domain"
	"gorm.io/gorm"
)

// ErrNotFound 表示用户不存在。
var ErrNotFound = errors.New("user: not found")

// Service 是用户仓储。
type Service struct {
	db  *gorm.DB
	now func() time.Time
}

// New 构造用户仓储。
func New(db *gorm.DB) *Service {
	return &Service{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// WithTx 返回绑定到同一事务的副本。
func (s *Service) WithTx(tx *gorm.DB) *Service {
	c := *s
	c.db = tx
	return &c
}

// WithClock 注入时钟（测试用）。
func (s *Service) WithClock(now func() time.Time) *Service {
	c := *s
	c.now = now
	return &c
}

// CreateInvited 创建一个"待激活"用户（status=invited，uid 为 NULL）。
func (s *Service) CreateInvited(ctx context.Context, email string, createdBy *int64) (*domain.User, error) {
	now := s.now()
	u := &domain.User{
		Email:     domain.NormalizeEmail(email),
		Status:    domain.UserStatusInvited,
		CreatedBy: createdBy,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(u).Error; err != nil {
		return nil, fmt.Errorf("user: create: %w", err)
	}
	return u, nil
}

// CreateActive 直接创建一个已激活用户（bootstrap 管理员用）。
func (s *Service) CreateActive(ctx context.Context, email string, isAdmin bool) (*domain.User, error) {
	now := s.now()
	u := &domain.User{
		Email:       domain.NormalizeEmail(email),
		Status:      domain.UserStatusActive,
		IsAdmin:     isAdmin,
		ActivatedAt: &now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.db.WithContext(ctx).Create(u).Error; err != nil {
		return nil, fmt.Errorf("user: create active: %w", err)
	}
	return u, nil
}

// GetByEmail 按（归一化后的）邮箱查询；不存在返回 ErrNotFound。
func (s *Service) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	return s.get(ctx, "email = ?", domain.NormalizeEmail(email))
}

// GetByUID 按 uid 查询；不存在返回 ErrNotFound。
func (s *Service) GetByUID(ctx context.Context, uid string) (*domain.User, error) {
	return s.get(ctx, "uid = ?", uid)
}

// GetByID 按主键查询；不存在返回 ErrNotFound。
func (s *Service) GetByID(ctx context.Context, id int64) (*domain.User, error) {
	return s.get(ctx, "id = ?", id)
}

func (s *Service) get(ctx context.Context, where string, arg any) (*domain.User, error) {
	var u domain.User
	err := s.db.WithContext(ctx).Where(where, arg).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("user: query: %w", err)
	}
	return &u, nil
}

// SetStatus 更新用户状态（并维护 disabled_at）。
func (s *Service) SetStatus(ctx context.Context, id int64, status string) error {
	now := s.now()
	updates := map[string]any{"status": status, "updated_at": now}
	switch status {
	case domain.UserStatusDisabled:
		updates["disabled_at"] = now
	case domain.UserStatusActive:
		updates["disabled_at"] = nil
	}
	res := s.db.WithContext(ctx).Model(&domain.User{}).Where("id = ?", id).Updates(updates)
	if res.Error != nil {
		return fmt.Errorf("user: set status: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAdmin 设置/取消管理员标记。
func (s *Service) SetAdmin(ctx context.Context, id int64, isAdmin bool) error {
	res := s.db.WithContext(ctx).Model(&domain.User{}).Where("id = ?", id).
		Updates(map[string]any{"is_admin": isAdmin, "updated_at": s.now()})
	if res.Error != nil {
		return fmt.Errorf("user: set admin: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// ChangeEmail 修改登录邮箱。
func (s *Service) ChangeEmail(ctx context.Context, id int64, email string) error {
	res := s.db.WithContext(ctx).Model(&domain.User{}).Where("id = ?", id).
		Updates(map[string]any{"email": domain.NormalizeEmail(email), "updated_at": s.now()})
	if res.Error != nil {
		return fmt.Errorf("user: change email: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete 删除用户（外键级联会一并删除其邀请与会话）。
func (s *Service) Delete(ctx context.Context, id int64) error {
	res := s.db.WithContext(ctx).Where("id = ?", id).Delete(&domain.User{})
	if res.Error != nil {
		return fmt.Errorf("user: delete: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// List 按创建时间倒序返回用户。
func (s *Service) List(ctx context.Context, limit, offset int) ([]domain.User, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var out []domain.User
	err := s.db.WithContext(ctx).Order("id DESC").Limit(limit).Offset(offset).Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("user: list: %w", err)
	}
	return out, nil
}

// CountByStatus 统计各状态用户数。
func (s *Service) CountByStatus(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.WithContext(ctx).Raw(
		`SELECT status, COUNT(*) FROM users GROUP BY status`).Rows()
	if err != nil {
		return nil, fmt.Errorf("user: count by status: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]int64)
	for rows.Next() {
		var (
			status string
			count  int64
		)
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		out[status] = count
	}
	return out, rows.Err()
}

// CountAdmins 返回管理员数量。
func (s *Service) CountAdmins(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&domain.User{}).Where("is_admin = ?", true).Count(&n).Error
	if err != nil {
		return 0, fmt.Errorf("user: count admins: %w", err)
	}
	return n, nil
}

// MarkLogin 记录登录时间，并返回这是否是**首次**登录。
//
// 用条件更新判定"首次"，因此并发或重试都不会重复触发首次登录的副作用
// （例如欢迎邮件只发一封）。
func (s *Service) MarkLogin(ctx context.Context, id int64) (firstLogin bool, err error) {
	now := s.now()

	res := s.db.WithContext(ctx).Exec(
		`UPDATE users SET last_login_at = ?, updated_at = ? WHERE id = ? AND last_login_at IS NULL`,
		now, now, id,
	)
	if res.Error != nil {
		return false, fmt.Errorf("user: mark first login: %w", res.Error)
	}
	if res.RowsAffected == 1 {
		return true, nil
	}

	res = s.db.WithContext(ctx).Exec(
		`UPDATE users SET last_login_at = ?, updated_at = ? WHERE id = ?`,
		now, now, id,
	)
	if res.Error != nil {
		return false, fmt.Errorf("user: mark login: %w", res.Error)
	}
	return false, nil
}
