package auth

import (
	"fmt"
	"log"
	"testing"
)

func TestMemoryAuth_Sign(t *testing.T) {

	auth := NewMemoryAuth()

	token, _ := auth.Sign("tangthinker")
	token2, _ := auth.Sign("tangthinker")
	fmt.Println("token: ", token)
	fmt.Println("token2: ", token2)

	uid, err := auth.Verify(token)
	if err != nil {
		log.Fatalln(err)
	}
	fmt.Println("uid: ", uid)

	uid, err = auth.Verify(token2)
	if err != nil {
		log.Fatalln(err)
	}
	fmt.Println("uid: ", uid)
}
