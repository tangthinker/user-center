// Package mail 提供邮件发送抽象、内置 SMTP 实现、持久化发件队列与模板。
//
// 形态约束（docs/auth-redesign.md §0.1）：
//   - L4：邮件配置全部来自宿主注入的 Config，本包不读任何环境变量；
//   - L5：发送结果通过 Hooks 回调交给宿主的日志/告警体系，本包不自己告警；
//   - 队列是**持久化**的（`mail_outbox` 表），进程重启不丢信。
package mail

import (
	"context"
	"errors"
)

// Message 是一封待发送的邮件。
//
// Subject 与 To 中的 CR/LF 会被拒绝（见 sanitizeHeader），
// 防止头部注入。
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Mailer 是发送抽象。
//
// 宿主可以用它接入任意 provider（阿里企业邮箱 SMTP、其他 SMTP、
// 或自有的邮件网关 SDK）；本包提供 SMTP 默认实现。
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// Hooks 让宿主决定"发送结果去哪里"（L5）。所有回调都可为 nil。
type Hooks struct {
	// OnSent 在投递成功后调用。
	OnSent func(to, template string)
	// OnFailed 在投递失败后调用；final 为 true 表示已达最大重试次数。
	OnFailed func(to, template string, err error, attempts int, final bool)
	// OnWorkerError 在队列处理本身出错时调用（例如数据库故障）。
	OnWorkerError func(err error)
}

// ErrDuplicate 表示同 dedupe_key 的邮件已在队列中（幂等命中，不是错误状态）。
var ErrDuplicate = errors.New("mail: duplicate dedupe key")

// ErrPermanent 表示不可重试的失败（例如模板渲染失败、地址非法）。
var ErrPermanent = errors.New("mail: permanent failure")
