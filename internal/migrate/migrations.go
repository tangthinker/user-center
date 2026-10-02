package migrate

// migrations 是本库的全部 schema 版本，必须按 Version 升序排列。
//
// 变更纪律（docs/auth-redesign.md §12.2）：
//   - 已发布版本的 Statements **不得修改**（已发布 tag 是不可变契约）；
//   - 新增变更必须追加新版本，且**只做加法**（新列可空或有默认值、新表独立）；
//   - 重命名、改类型、删列属于破坏性变更，必须升主版本。
var migrations = []Migration{
	{
		Version: 1,
		Name:    "init",
		Statements: []string{
			// ---------- 用户 ----------
			// 无软删除：email 唯一索引不会出现"被删用户永久占住邮箱"的陷阱。
			`CREATE TABLE IF NOT EXISTS users (
				id            INTEGER PRIMARY KEY AUTOINCREMENT,
				uid           TEXT     NULL,
				email         TEXT     NOT NULL,
				status        TEXT     NOT NULL,
				is_admin      INTEGER  NOT NULL DEFAULT 0,
				activated_at  DATETIME NULL,
				last_login_at DATETIME NULL,
				disabled_at   DATETIME NULL,
				created_by    INTEGER  NULL,
				created_at    DATETIME NOT NULL,
				updated_at    DATETIME NOT NULL
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS ux_users_email ON users(email)`,
			// 部分唯一索引：允许任意多个 uid IS NULL（待激活），
			// 但非 NULL 的 uid 必须唯一，且**大小写敏感**（SQLite 默认 BINARY collation）。
			`CREATE UNIQUE INDEX IF NOT EXISTS ux_users_uid ON users(uid) WHERE uid IS NOT NULL`,

			// ---------- 邀请 ----------
			// check_count 用于 invite/check-uid 的每 token 次数限制（原子递增）。
			`CREATE TABLE IF NOT EXISTS invitations (
				token_hash   BLOB PRIMARY KEY,
				user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				email        TEXT    NOT NULL,
				purpose      TEXT    NOT NULL,
				issued_at    DATETIME NOT NULL,
				expires_at   DATETIME NOT NULL,
				consumed_at  DATETIME NULL,
				created_by   INTEGER NOT NULL,
				sent_count   INTEGER NOT NULL DEFAULT 0,
				last_sent_at DATETIME NULL,
				check_count  INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS ix_invitations_user ON invitations(user_id)`,
			`CREATE INDEX IF NOT EXISTS ix_invitations_expires ON invitations(expires_at)`,

			// ---------- 一次性验证码 ----------
			// 部分唯一索引保证同一 (email, purpose) 任意时刻至多一个未消费的码（设计 §4.2 I4）。
			`CREATE TABLE IF NOT EXISTS otp_codes (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				email       TEXT    NOT NULL,
				purpose     TEXT    NOT NULL,
				code_hmac   BLOB    NOT NULL,
				issued_at   DATETIME NOT NULL,
				expires_at  DATETIME NOT NULL,
				attempts    INTEGER NOT NULL DEFAULT 0,
				consumed_at DATETIME NULL,
				request_ip  TEXT    NULL
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS ux_otp_active ON otp_codes(email, purpose) WHERE consumed_at IS NULL`,

			// ---------- 会话 ----------
			`CREATE TABLE IF NOT EXISTS sessions (
				token_hash          BLOB PRIMARY KEY,
				user_id             INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				uid                 TEXT    NOT NULL,
				scope               TEXT    NOT NULL,
				issued_at           DATETIME NOT NULL,
				idle_expires_at     DATETIME NOT NULL,
				absolute_expires_at DATETIME NOT NULL,
				last_seen_at        DATETIME NOT NULL,
				ip                  TEXT    NULL,
				ua_hash             TEXT    NULL,
				revoked_at          DATETIME NULL,
				revoke_reason       TEXT    NULL
			)`,
			`CREATE INDEX IF NOT EXISTS ix_sessions_user ON sessions(user_id)`,
			`CREATE INDEX IF NOT EXISTS ix_sessions_abs ON sessions(absolute_expires_at)`,

			// ---------- 邮件发件箱（持久化队列） ----------
			// priority 列本期不参与调度策略，为将来可能的优先级队列预留（设计 §7.2）。
			`CREATE TABLE IF NOT EXISTS mail_outbox (
				id              INTEGER PRIMARY KEY AUTOINCREMENT,
				dedupe_key      TEXT    NOT NULL,
				to_email        TEXT    NOT NULL,
				template        TEXT    NOT NULL,
				payload         TEXT    NULL,
				priority        INTEGER NOT NULL DEFAULT 100,
				status          TEXT    NOT NULL,
				attempts        INTEGER NOT NULL DEFAULT 0,
				next_attempt_at DATETIME NOT NULL,
				last_error      TEXT    NULL,
				created_at      DATETIME NOT NULL,
				sent_at         DATETIME NULL
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS ux_outbox_dedupe ON mail_outbox(dedupe_key)`,
			`CREATE INDEX IF NOT EXISTS ix_outbox_pending ON mail_outbox(status, next_attempt_at)`,

			// ---------- 发信请求日志（配额统计与限流判据） ----------
			`CREATE TABLE IF NOT EXISTS mail_log (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				email_hash TEXT    NOT NULL,
				purpose    TEXT    NOT NULL,
				ip         TEXT    NULL,
				accepted   INTEGER NOT NULL,
				created_at DATETIME NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS ix_mail_log_created ON mail_log(created_at)`,
			`CREATE INDEX IF NOT EXISTS ix_mail_log_email ON mail_log(email_hash, purpose, created_at)`,

			// ---------- 管理审计 ----------
			`CREATE TABLE IF NOT EXISTS admin_audit (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				actor_id    INTEGER NOT NULL,
				actor_email TEXT    NOT NULL,
				action      TEXT    NOT NULL,
				target      TEXT    NULL,
				detail      TEXT    NULL,
				ip          TEXT    NULL,
				ua          TEXT    NULL,
				created_at  DATETIME NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS ix_audit_created ON admin_audit(created_at)`,
		},
	},
}
