package auth

import (
	"fmt"
	"log"
	"sync"
	"testing"
	"time"
)

func TestMemoryAuth_Sign(t *testing.T) {

	auth := NewMemoryAuth()

	token, _ := auth.Sign("tangthinker")
	time.Sleep(1 * time.Second)
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

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		auth2 := NewMemoryAuth()
		defer wg.Done()
		uid, err = auth2.Verify("")
		if err != nil {
			fmt.Println(err)
		}
	}()

	wg.Add(1)
	go func() {
		auth3 := NewMemoryAuth()
		defer wg.Done()
		uid, err = auth3.Verify(token2)
		if err != nil {
			fmt.Println(err)
		}
	}()

	wg.Add(1)
	go func() {
		auth4 := NewMemoryAuth()
		defer wg.Done()
		uid, err = auth4.Verify(token)
		if err != nil {
			fmt.Println(err)
		}
	}()

	wg.Wait()
}
