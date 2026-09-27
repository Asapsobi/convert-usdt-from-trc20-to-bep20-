// Command hashpassword prints the bcrypt hash of the password on stdin,
// for an OC_OPERATORS entry ("username:hash:Display Name"):
//
//	printf '%s' 'the password' | go run ./cmd/hashpassword
//
// scripts/dev.sh setup uses it to create the local operator login.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fail(err)
	}
	password := strings.TrimRight(string(raw), "\r\n")
	if password == "" {
		fail(fmt.Errorf("no password on stdin"))
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		fail(err)
	}
	fmt.Println(string(hash))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "hashpassword:", err)
	os.Exit(1)
}
