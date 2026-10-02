// Package webui 内嵌本库自带的静态界面。
//
// 设计约束（docs/auth-redesign.md §8.3）：
//   - 零外部依赖：无 CDN、无第三方字体/统计脚本（无供应链风险）；
//   - 无内联脚本：JS/CSS 独立文件，便于启用严格 CSP；
//   - 不使用 cookie：管理界面把会话 token 放在内存里，随 Authorization 头发送，
//     从根本上消除 CSRF 面。
package webui

import (
	"embed"
	"io/fs"
)

//go:embed invite.html invite.js style.css admin/index.html admin/app.js
var files embed.FS

// InvitePage 返回邀请落地页模板（含 {{...}} 占位符，由 httpapi 注入）。
func InvitePage() ([]byte, error) { return files.ReadFile("invite.html") }

// InviteJS 返回邀请落地页脚本。
func InviteJS() ([]byte, error) { return files.ReadFile("invite.js") }

// AdminIndex 返回管理界面首页模板（含 {{...}} 占位符）。
func AdminIndex() ([]byte, error) { return files.ReadFile("admin/index.html") }

// AdminJS 返回管理界面脚本。
func AdminJS() ([]byte, error) { return files.ReadFile("admin/app.js") }

// StyleCSS 返回共享样式（无外部资源）。
func StyleCSS() ([]byte, error) { return files.ReadFile("style.css") }

// FS 暴露内嵌文件系统（只读），供宿主需要时自行挂载。
func FS() fs.FS { return files }
