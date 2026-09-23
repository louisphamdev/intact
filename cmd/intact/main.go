// Command intact serves provider credentials and forwards requests unchanged.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/louisphamdev/intact/internal/auth"
	"github.com/louisphamdev/intact/internal/httpapi"
	"github.com/louisphamdev/intact/internal/store"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:20130", "listen address")
	dbPath := flag.String("db", "intact.db", "path to the database file")
	enroll := flag.Bool("enroll", false, "print a new TOTP secret and otpauth URI, then exit")
	insecure := flag.Bool("insecure-no-auth", false, "start with no sign-in gate; every caller reaches every stored credential")
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

	authCfg, err := auth.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	if err := authDecision(authCfg, *insecure || os.Getenv("INTACT_INSECURE_NO_AUTH") == "1"); err != nil {
		log.Fatal(err)
	}
	if authCfg == nil {
		log.Printf("WARNING: no INTACT_TOTP_SECRET set, the server is not authenticated")
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: httpapi.NewWithAuth(s, nil, authCfg),
		// Header-read only. A read or write deadline would cut a long stream.
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("intact listening on http://%s", *addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// authDecision refuses to start an ungated server. The reference deployment
// binds loopback behind a tunnel, so a loopback bind proves nothing about who
// reaches the port.
func authDecision(authCfg *auth.Config, insecureNoAuth bool) error {
	if authCfg == nil && !insecureNoAuth {
		return errors.New("INTACT_TOTP_SECRET is not set: every caller would reach every stored credential. " +
			"Set it, or pass -insecure-no-auth (or INTACT_INSECURE_NO_AUTH=1) to start with no gate")
	}
	return nil
}
