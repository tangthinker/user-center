package secure_test

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/tangthinker/user-center/v2/internal/secure"
)

func TestNewTokenIsUniqueAndHashed(t *testing.T) {
	seen := make(map[string]struct{}, 500)
	for i := 0; i < 500; i++ {
		plain, hash, err := secure.NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if plain == "" {
			t.Fatal("empty token")
		}
		if _, dup := seen[plain]; dup {
			t.Fatal("duplicate token generated")
		}
		seen[plain] = struct{}{}

		if !bytes.Equal(hash, secure.HashToken(plain)) {
			t.Fatal("returned hash does not match HashToken(plain)")
		}
		if bytes.Contains([]byte(plain), hash) {
			t.Fatal("plain token must not contain its hash")
		}
	}
}

func TestHashTokenIsDeterministic(t *testing.T) {
	a := secure.HashToken("abc")
	b := secure.HashToken("abc")
	c := secure.HashToken("abd")
	if !bytes.Equal(a, b) {
		t.Fatal("hash is not deterministic")
	}
	if bytes.Equal(a, c) {
		t.Fatal("different inputs produced the same hash")
	}
	if len(a) != 32 {
		t.Fatalf("sha256 length = %d, want 32", len(a))
	}
}

func TestNewNumericCode(t *testing.T) {
	distinct := make(map[string]struct{})
	for i := 0; i < 300; i++ {
		code, err := secure.NewNumericCode(6)
		if err != nil {
			t.Fatalf("NewNumericCode: %v", err)
		}
		if len(code) != 6 {
			t.Fatalf("code = %q, want 6 digits (含前导零)", code)
		}
		for _, r := range code {
			if r < '0' || r > '9' {
				t.Fatalf("code %q contains non-digit %q", code, r)
			}
		}
		distinct[code] = struct{}{}
	}
	// 300 次采样若几乎全相同，说明随机源有问题
	if len(distinct) < 100 {
		t.Fatalf("only %d distinct codes out of 300; random source looks broken", len(distinct))
	}
}

func TestNewNumericCodeRejectsBadLength(t *testing.T) {
	for _, n := range []int{0, -1, 13, 100} {
		if _, err := secure.NewNumericCode(n); err == nil {
			t.Fatalf("expected error for digits=%d", n)
		}
	}
}

func TestCodeHMACBindsPurposeAndEmail(t *testing.T) {
	key := []byte("test-key")
	base := secure.CodeHMAC(key, "login", "a@example.com", "123456")

	if !bytes.Equal(base, secure.CodeHMAC(key, "login", "a@example.com", "123456")) {
		t.Fatal("HMAC is not deterministic")
	}
	// 用途隔离：login 的 HMAC 不能等于 admin_login 的
	if bytes.Equal(base, secure.CodeHMAC(key, "admin_login", "a@example.com", "123456")) {
		t.Fatal("HMAC must differ by purpose")
	}
	// 邮箱绑定
	if bytes.Equal(base, secure.CodeHMAC(key, "login", "b@example.com", "123456")) {
		t.Fatal("HMAC must differ by email")
	}
	// 码本身
	if bytes.Equal(base, secure.CodeHMAC(key, "login", "a@example.com", "123457")) {
		t.Fatal("HMAC must differ by code")
	}
	// 密钥
	if bytes.Equal(base, secure.CodeHMAC([]byte("other"), "login", "a@example.com", "123456")) {
		t.Fatal("HMAC must differ by key")
	}
}

func TestEqual(t *testing.T) {
	if !secure.Equal([]byte("abc"), []byte("abc")) {
		t.Fatal("Equal should be true for identical slices")
	}
	if secure.Equal([]byte("abc"), []byte("abd")) {
		t.Fatal("Equal should be false for different slices")
	}
	if secure.Equal([]byte("abc"), []byte("ab")) {
		t.Fatal("Equal should be false for different lengths")
	}
	if !secure.EqualString("tok", "tok") || secure.EqualString("tok", "toK") {
		t.Fatal("EqualString mismatch")
	}
}

func TestHashEmail(t *testing.T) {
	a := secure.HashEmail("User@Example.com ")
	b := secure.HashEmail("user@example.com")
	if a != b {
		t.Fatal("HashEmail must be case/space insensitive")
	}
	if strings.Contains(a, "@") {
		t.Fatal("HashEmail must not leak the original address")
	}
	if secure.HashEmail("x@example.com") == a {
		t.Fatal("different emails must produce different hashes")
	}
}

// 令牌生成必须并发安全（crypto/rand 是安全的，这里验证无数据竞争与重复）
func TestNewTokenConcurrent(t *testing.T) {
	const n = 200
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = make(map[string]struct{}, n)
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			plain, _, err := secure.NewToken()
			if err != nil {
				t.Errorf("NewToken: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if _, dup := seen[plain]; dup {
				t.Error("duplicate token under concurrency")
			}
			seen[plain] = struct{}{}
		}()
	}
	wg.Wait()
}
