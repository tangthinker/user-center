package auth

import (
	"fmt"
	"log"
	"testing"
)

func TestMemoryAuth_Sign(t *testing.T) {

	auth := NewMemoryAuth()

	token, _ := auth.Sign("tangthinker")
	fmt.Println("token: ", token)

	uid, err := auth.Verify(token)
	if err != nil {
		log.Fatalln(err)
	}
	fmt.Println("uid: ", uid)
}
