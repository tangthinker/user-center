package mail

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/store"
	"gorm.io/gorm"
)

// OutboxConfig 是队列与重试策略。
type OutboxConfig struct {
	// MaxAttempts 是单封邮件的最大投递次数。
	MaxAttempts int
	// Backoff 是第 1、2、3… 次失败后的等待时长；超出长度的部分沿用最后一个值。
	Backoff []time.Duration
	// BatchSize 是单轮 ProcessOnce 最多处理的邮件数。
	BatchSize int
}

// DefaultOutboxConfig 返回设计文档 §7.3 的重试策略：1m → 5m → 15m → 1h → 6h。
func DefaultOutboxConfig() OutboxConfig {
	return OutboxConfig{
		MaxAttempts: 5,
		Backoff: []time.Duration{
			time.Minute,
			5 * time.Minute,
			15 * time.Minute,
			time.Hour,
			6 * time.Hour,
		},
		BatchSize: 10,
	}
}

func (c OutboxConfig) withDefaults() OutboxConfig {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	if len(c.Backoff) == 0 {
		c.Backoff = DefaultOutboxConfig().Backoff
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 10
	}
	return c
}

// Outbox 是持久化发件队列。
//
// 单实例前提下它就是队列本身：`mail_outbox` 表由数据库保证持久性，
// 进程重启不丢信；抢占用条件更新完成，重复投递不会发生。
type Outbox struct {
	db       *gorm.DB
	mailer   Mailer
	renderer Renderer
	cfg      OutboxConfig
	hooks    Hooks
	now      func() time.Time
}

// NewOutbox 构造队列。
func NewOutbox(db *gorm.DB, mailer Mailer, renderer Renderer, cfg OutboxConfig, hooks Hooks) (*Outbox, error) {
	if db == nil {
		return nil, errors.New("mail: nil db")
	}
	if mailer == nil {
		return nil, errors.New("mail: Mailer is required")
	}
	if renderer == nil {
		return nil, errors.New("mail: Renderer is required")
	}
	return &Outbox{
		db:       db,
		mailer:   mailer,
		renderer: renderer,
		cfg:      cfg.withDefaults(),
		hooks:    hooks,
		now:      func() time.Time { return time.Now().UTC() },
	}, nil
}

// WithTx 返回绑定到同一事务的副本。
//
// 编排层应把"入队"放在业务事务内（outbox 模式）：事务提交则邮件必定在队列里，
// 回滚则不会有幽灵邮件——这比"提交后再入队"更不容易丢信。
func (o *Outbox) WithTx(tx *gorm.DB) *Outbox {
	c := *o
	c.db = tx
	return &c
}

// WithClock 注入时钟（测试用）。
func (o *Outbox) WithClock(now func() time.Time) *Outbox {
	c := *o
	c.now = now
	return &c
}

// EnqueueParams 是一次入队请求。
type EnqueueParams struct {
	// DedupeKey 是幂等键（唯一索引）；重复入队返回 ErrDuplicate。
	DedupeKey string
	To        string
	Template  string
	Payload   any
}

// Enqueue 把一封邮件放入队列。
func (o *Outbox) Enqueue(ctx context.Context, p EnqueueParams) error {
	if p.DedupeKey == "" {
		return errors.New("mail: DedupeKey is required")
	}
	if p.Template == "" {
		return errors.New("mail: Template is required")
	}
	if err := ValidateAddress(p.To); err != nil {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}

	var payload *string
	if p.Payload != nil {
		raw, err := EncodePayload(p.Payload)
		if err != nil {
			return err
		}
		s := string(raw)
		payload = &s
	}

	now := o.now()
	rec := &domain.MailOutbox{
		DedupeKey:     p.DedupeKey,
		ToEmail:       p.To,
		Template:      p.Template,
		Payload:       payload,
		Priority:      100,
		Status:        domain.MailPending,
		Attempts:      0,
		NextAttemptAt: now,
		CreatedAt:     now,
	}
	if err := o.db.WithContext(ctx).Create(rec).Error; err != nil {
		if store.IsUniqueViolation(err) {
			return fmt.Errorf("%w: %s", ErrDuplicate, p.DedupeKey)
		}
		return fmt.Errorf("mail: enqueue: %w", err)
	}
	return nil
}

// Processed 是单轮处理的统计。
type Processed struct {
	Sent    int
	Retried int
	Failed  int
}

// ProcessOnce 处理一批待发邮件后返回。
func (o *Outbox) ProcessOnce(ctx context.Context) (Processed, error) {
	var out Processed
	for i := 0; i < o.cfg.BatchSize; i++ {
		rec, ok, err := o.claim(ctx)
		if err != nil {
			return out, err
		}
		if !ok {
			break
		}

		sendErr := o.deliver(ctx, rec)
		if sendErr == nil {
			if err := o.markSent(ctx, rec); err != nil {
				return out, err
			}
			if o.hooks.OnSent != nil {
				o.hooks.OnSent(rec.ToEmail, rec.Template)
			}
			out.Sent++
			continue
		}

		// 永久性失败（模板损坏、地址非法）不浪费重试次数，直接终态。
		final := rec.Attempts >= o.cfg.MaxAttempts || errors.Is(sendErr, ErrPermanent)
		if err := o.markFailure(ctx, rec, sendErr, final); err != nil {
			return out, err
		}
		if o.hooks.OnFailed != nil {
			o.hooks.OnFailed(rec.ToEmail, rec.Template, sendErr, rec.Attempts, final)
		}
		if final {
			out.Failed++
		} else {
			out.Retried++
		}
	}
	return out, nil
}

// Run 周期性地处理队列，直到 ctx 被取消。返回 nil（取消不是错误）。
func (o *Outbox) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// 立即处理一次，避免第一封邮件要等到第一个 tick
	if _, err := o.ProcessOnce(ctx); err != nil && o.hooks.OnWorkerError != nil {
		o.hooks.OnWorkerError(err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := o.ProcessOnce(ctx); err != nil && o.hooks.OnWorkerError != nil {
				o.hooks.OnWorkerError(err)
			}
		}
	}
}

// Stats 是队列概况，供宿主的上报/告警使用（L5）。
type Stats struct {
	Pending         int64
	Sending         int64
	Sent            int64
	Failed          int64
	OldestPendingAt *time.Time
}

// Stats 读取队列概况。
func (o *Outbox) Stats(ctx context.Context) (Stats, error) {
	var out Stats
	rows, err := o.db.WithContext(ctx).Raw(
		`SELECT status, COUNT(*) FROM mail_outbox GROUP BY status`).Rows()
	if err != nil {
		return out, fmt.Errorf("mail: stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return out, err
		}
		switch status {
		case domain.MailPending:
			out.Pending = count
		case domain.MailSending:
			out.Sending = count
		case domain.MailSent:
			out.Sent = count
		case domain.MailFailed:
			out.Failed = count
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	// 用列本身（而不是 MIN(...) 表达式）取出最早的一条：SQLite 驱动只对
	// 声明了类型的列做时间转换，聚合表达式会退化为字符串。
	var oldest time.Time
	err = o.db.WithContext(ctx).Raw(
		`SELECT created_at FROM mail_outbox WHERE status = ? ORDER BY created_at ASC, id ASC LIMIT 1`,
		domain.MailPending,
	).Row().Scan(&oldest)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 没有待发邮件
	case err != nil:
		return out, fmt.Errorf("mail: stats oldest: %w", err)
	default:
		t := oldest.UTC()
		out.OldestPendingAt = &t
	}
	return out, nil
}

// CleanupSent 删除保留期之外的已发送邮件，返回删除行数。
func (o *Outbox) CleanupSent(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		retention = 30 * 24 * time.Hour
	}
	res := o.db.WithContext(ctx).
		Where("status = ? AND sent_at IS NOT NULL AND sent_at < ?", domain.MailSent, o.now().Add(-retention)).
		Delete(&domain.MailOutbox{})
	if res.Error != nil {
		return 0, fmt.Errorf("mail: cleanup: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// claim 原子地取出一封待发邮件并把状态置为 sending。
func (o *Outbox) claim(ctx context.Context) (*domain.MailOutbox, bool, error) {
	now := o.now()

	var id int64
	err := o.db.WithContext(ctx).Raw(
		`SELECT id FROM mail_outbox
		 WHERE status = ? AND next_attempt_at <= ?
		 ORDER BY priority ASC, id ASC LIMIT 1`,
		domain.MailPending, now,
	).Row().Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("mail: claim select: %w", err)
	}
	if id == 0 {
		return nil, false, nil
	}

	res := o.db.WithContext(ctx).Exec(
		`UPDATE mail_outbox SET status = ?, attempts = attempts + 1
		 WHERE id = ? AND status = ?`,
		domain.MailSending, id, domain.MailPending,
	)
	if res.Error != nil {
		return nil, false, fmt.Errorf("mail: claim update: %w", res.Error)
	}
	if res.RowsAffected != 1 {
		// 被其它 worker 抢走了，本轮跳过
		return nil, false, nil
	}

	var rec domain.MailOutbox
	if err := o.db.WithContext(ctx).Where("id = ?", id).First(&rec).Error; err != nil {
		return nil, false, fmt.Errorf("mail: claim load: %w", err)
	}
	return &rec, true, nil
}

func (o *Outbox) deliver(ctx context.Context, rec *domain.MailOutbox) error {
	var payload []byte
	if rec.Payload != nil {
		payload = []byte(*rec.Payload)
	}
	msg, err := o.renderer.Build(rec.Template, payload)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	msg.To = rec.ToEmail
	return o.mailer.Send(ctx, msg)
}

func (o *Outbox) markSent(ctx context.Context, rec *domain.MailOutbox) error {
	now := o.now()
	// 发送成功后清空 payload：邀请链接与验证码的明文只应存在于"待发"期间，
	// 不能在库里长期留存（设计 §4.2 I3 的延伸）。
	res := o.db.WithContext(ctx).Exec(
		`UPDATE mail_outbox SET status = ?, sent_at = ?, last_error = NULL, payload = NULL WHERE id = ?`,
		domain.MailSent, now, rec.ID,
	)
	if res.Error != nil {
		return fmt.Errorf("mail: mark sent: %w", res.Error)
	}
	return nil
}

func (o *Outbox) markFailure(ctx context.Context, rec *domain.MailOutbox, sendErr error, final bool) error {
	now := o.now()
	msg := sendErr.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}

	if final {
		// 终态失败同样清空 payload：敏感明文不该因为一封发不出去的邮件而长期留存。
		res := o.db.WithContext(ctx).Exec(
			`UPDATE mail_outbox SET status = ?, last_error = ?, payload = NULL WHERE id = ?`,
			domain.MailFailed, msg, rec.ID,
		)
		if res.Error != nil {
			return fmt.Errorf("mail: mark failed: %w", res.Error)
		}
		return nil
	}

	res := o.db.WithContext(ctx).Exec(
		`UPDATE mail_outbox SET status = ?, next_attempt_at = ?, last_error = ? WHERE id = ?`,
		domain.MailPending, now.Add(o.backoffFor(rec.Attempts)), msg, rec.ID,
	)
	if res.Error != nil {
		return fmt.Errorf("mail: mark retry: %w", res.Error)
	}
	return nil
}

// backoffFor 返回第 attempts 次失败后的等待时长。
func (o *Outbox) backoffFor(attempts int) time.Duration {
	if attempts <= 0 {
		attempts = 1
	}
	idx := attempts - 1
	if idx >= len(o.cfg.Backoff) {
		idx = len(o.cfg.Backoff) - 1
	}
	return o.cfg.Backoff[idx]
}
