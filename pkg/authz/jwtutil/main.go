package main

import (
	"flag"
	"fmt"
	"log"

	jwt "github.com/golang-jwt/jwt/v4"
)

var flags struct {
}

func main() {
	flag.Parse()

	tok := jwt.NewWithClaims(
		jwt.SigningMethodHS256,
		&jwt.StandardClaims{
			Subject: "auser",
		},
	)

	out, err := tok.SignedString([]byte("AllYourToks"))
	if err != nil {
		log.Fatalf("Error signing: %v", err)
	}
	fmt.Println(out)
}
