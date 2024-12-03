package idgen

import (
	"crypto/md5"
	"fmt"
	"math/rand"
	"strconv"
	"time"
)

type Generator interface {
	Generate() string
}

type CommonGenerator struct {
}

func NewCommonGenerator() Generator {
	return &CommonGenerator{}
}

func (g *CommonGenerator) Generate() string {

	prefix := "Thinker"

	now := time.Now().UnixNano()

	hash := md5.New()

	hash.Write([]byte(strconv.FormatInt(now, 10)))

	info := fmt.Sprintf("%x", hash.Sum(nil))

	for i := 0; i < 10; i++ {
		idx := rand.Intn(len(info))
		if info[idx] >= 'a' && info[idx] <= 'z' {
			info = info[:idx] + string(info[idx]-32) + info[idx+1:]
		}
	}

	return prefix + info
}
