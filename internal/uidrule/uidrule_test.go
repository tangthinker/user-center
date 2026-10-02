package uidrule_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/tangthinker/user-center/v2/internal/uidrule"
)

func TestValidateAcceptsLegalUIDs(t *testing.T) {
	legal := []string{
		"abc",                         // 最短
		"Alice",                       // 大小写混合
		"alice",                       // 与 Alice 视为不同用户
		"a_b-c",                       // 允许下划线与连字符
		"abc123",                      // 允许数字（非首位）
		"a" + strings.Repeat("b", 31), // 恰好 32
		"Thinker007",
	}
	for _, uid := range legal {
		if err := uidrule.Validate(uid); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", uid, err)
		}
	}
}

func TestValidateRejectsIllegalUIDs(t *testing.T) {
	cases := []struct {
		uid  string
		want error
	}{
		{"", uidrule.ErrEmpty},
		{"ab", uidrule.ErrTooShort},
		{"a" + strings.Repeat("b", 32), uidrule.ErrTooLong}, // 33
		{"1abc", uidrule.ErrFirstChar},                      // 数字打头
		{"9", uidrule.ErrTooShort},                          // 先触发长度
		{"_abc", uidrule.ErrFirstChar},
		{"-abc", uidrule.ErrFirstChar},
		{"ab c", uidrule.ErrSpace},
		{"ab\tc", uidrule.ErrSpace},
		{"abc@def", uidrule.ErrCharset},
		{"abc.def", uidrule.ErrCharset},
		{"abc/def", uidrule.ErrCharset}, // 防未来被当作路径片段使用
		{"用户名abc", uidrule.ErrFirstChar},
		{"abc用户名", uidrule.ErrCharset},
		{"ab😀", uidrule.ErrCharset},
	}
	for _, c := range cases {
		err := uidrule.Validate(c.uid)
		if !errors.Is(err, c.want) {
			t.Errorf("Validate(%q) = %v, want %v", c.uid, err, c.want)
		}
	}
}

// 保留字必须大小写不敏感匹配——这是"大小写敏感"策略的必要配套
func TestReservedWordsAreCaseInsensitive(t *testing.T) {
	for _, uid := range []string{"admin", "Admin", "ADMIN", "aDmIn", "root", "ROOT", "noreply", "NoReply"} {
		if err := uidrule.Validate(uid); !errors.Is(err, uidrule.ErrReserved) {
			t.Errorf("Validate(%q) = %v, want ErrReserved", uid, err)
		}
	}
}

// 默认表不得包含宿主/作者专有名词：本库被多个宿主引用，
// 把某一个宿主或作者的名字写进默认保留字，等于替别人的部署做决定。
func TestDefaultReservedListHasNoHostOrAuthorNames(t *testing.T) {
	for _, uid := range []string{"tangthinker", "TangThinker", "cloudcore", "cloud", "storage", "file", "files"} {
		if uidrule.IsReserved(uid) {
			t.Errorf("%q 不应出现在默认保留字里（宿主如需保护自己的名字，用 Config.ReservedUIDs）", uid)
		}
		if err := uidrule.Validate(uid); err != nil {
			t.Errorf("Validate(%q) = %v，默认表下应当可用", uid, err)
		}
	}
}

// 宿主可以追加自己的保留字（大小写不敏感）。
func TestIsReservedExtra(t *testing.T) {
	extra := []string{"TangThinker", "  cloud-core  ", ""}
	for _, uid := range []string{"tangthinker", "TANGTHINKER", "cloud-core", "Cloud-Core"} {
		if !uidrule.IsReservedExtra(uid, extra) {
			t.Errorf("IsReservedExtra(%q) = false，应当命中", uid)
		}
	}
	for _, uid := range []string{"alice", ""} {
		if uidrule.IsReservedExtra(uid, extra) {
			t.Errorf("IsReservedExtra(%q) = true，不应命中", uid)
		}
	}
	if uidrule.IsReservedExtra("alice", nil) {
		t.Error("空名单不应命中任何值")
	}
}

func TestIsReservedAndIsValid(t *testing.T) {
	if !uidrule.IsReserved("SuPpOrT") {
		t.Error("IsReserved should be case-insensitive")
	}
	if uidrule.IsReserved("myuser") {
		t.Error("myuser should not be reserved")
	}
	if !uidrule.IsValid("myuser") {
		t.Error("IsValid(myuser) = false")
	}
	if uidrule.IsValid("1user") {
		t.Error("IsValid(1user) = true")
	}
}

// 边界：长度按 rune 计数
func TestLengthBoundariesUseRunes(t *testing.T) {
	if err := uidrule.Validate("ab"); err == nil {
		t.Error("2 chars must be rejected")
	}
	if err := uidrule.Validate("abc"); err != nil {
		t.Errorf("3 chars must be accepted, got %v", err)
	}
}
