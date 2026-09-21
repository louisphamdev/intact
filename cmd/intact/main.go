// Command intact serves provider credentials and forwards requests unchanged.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"

	"github.com/louisphamdev/intact/internal/auth"
	"github.com/louisphamdev/intact/internal/httpapi"
	"github.com/louisphamdev/intact/internal/store"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:20130", "listen address")
	dbPath := flag.String("db", "intact.db", "path to the database file")
	enroll := flag.Bool("enroll", false, "print a new TOTP secret and otpauth URI, then exit")
	flag.Parse()

	if *enroll {
		secret := auth.GenerateTOTPSecret()
		uri := fmt.Sprintf("otpauth://totp/%s?secret=%s&issuer=intact",
			url.QueryEscape("intact"), secret)
		fmt.Println("INTACT_TOTP_SECRET=" + secret)
		fmt.Println("Add this to your authenticator app:")
		fmt.Println("  " + uri)
		return
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer s.Close()

	authCfg := auth.FromEnv()
	if authCfg == nil {
		log.Printf("WARNING: no INTACT_TOTP_SECRET set, the server is not authenticated")
	}
	// A public listener with no auth would expose every stored credential, so
	// refuse it. Loopback stays open for local use.
	if authCfg == nil && !isLoopback(*addr) {
		log.Fatalf("refusing to listen on %s without INTACT_TOTP_SECRET: that would expose credentials", *addr)
	}

	log.Printf("intact listening on http://%s", *addr)
	if err := http.ListenAndServe(*addr, httpapi.NewWithAuth(s, nil, authCfg)); err != nil {
		log.Fatal(err)
	}
}

// isLoopback reports whether the listen address is bound to localhost only.
func isLoopback(addr string) bool {
	host := addr
	if i := lastColon(addr); i >= 0 {
		host = addr[:i]
	}
	switch host {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	// An empty host or 0.0.0.0 means every interface, which is not loopback.
	return false
}

func lastColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}
