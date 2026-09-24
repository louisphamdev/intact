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

func runContractReset(args []string) error {
	resetFlags := flag.NewFlagSet("contract-reset", flag.ContinueOnError)
	dbPath := resetFlags.String("db", "intact.db", "path to the database file")
	keyID := resetFlags.String("key", "", "key id to reset")
	reducerVer := resetFlags.Int("reducer", 0, "reducer version to reset")
	if err := resetFlags.Parse(args); err != nil {
		return err
	}
	if *keyID != "" && *reducerVer > 0 {
		return errors.New("cannot specify both --key and --reducer")
	}
	if *keyID == "" && *reducerVer <= 0 {
		return errors.New("must specify either --key or --reducer")
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	if err := s.ResetContract(*keyID, *reducerVer); err != nil {
		return fmt.Errorf("contract-reset: %w", err)
	}
	fmt.Printf("contract-reset complete (key=%q reducer=%d)\n", *keyID, *reducerVer)
	return nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "contract-reset" {
		if err := runContractReset(os.Args[2:]); err != nil {
			log.Fatalf("contract-reset: %v", err)
		}
		return
	}

	addr := flag.String("addr", "127.0.0.1:20130", "listen address")
	dbPath := flag.String("db", "intact.db", "path to the database file")
	enroll := flag.Bool("enroll", false, "print a new TOTP secret and otpauth URI, then exit")
	showTOTP := flag.Bool("show-totp", false, "print this install's TOTP secret and how to add it to an authenticator app, then exit")
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

	noAuth := *insecure || os.Getenv("INTACT_INSECURE_NO_AUTH") == "1"
	var authCfg *auth.Config
	if !noAuth || os.Getenv("INTACT_TOTP_SECRET") != "" || *showTOTP {
		secret, fresh, err := totpSecret(s, os.Getenv("INTACT_TOTP_SECRET"))
		if err != nil {
			log.Fatal(err)
		}
		if *showTOTP {
			fmt.Print(enrollmentGuide(secret))
			return
		}
		if authCfg, err = auth.FromSecret(secret); err != nil {
			log.Fatal(err)
		}
		if fresh {
			log.Print("\n" + enrollmentGuide(secret))
		}
	}
	if err := authDecision(authCfg, noAuth); err != nil {
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

const totpSetting = "totp_secret"

// totpSecret returns the sign-in secret: the environment's, else the one this
// install created on its first start, else a new one that is saved (fresh).
func totpSecret(s *store.Store, env string) (secret string, fresh bool, err error) {
	if env != "" {
		return env, false, nil
	}
	if saved, _ := s.GetSetting(totpSetting); saved != "" {
		return saved, false, nil
	}
	secret = auth.GenerateTOTPSecret()
	if err := s.SetSetting(totpSetting, secret); err != nil {
		return "", false, fmt.Errorf("save the TOTP secret: %w", err)
	}
	return secret, true, nil
}

// enrollmentGuide tells the owner how to add the secret to an authenticator app.
func enrollmentGuide(secret string) string {
	uri := fmt.Sprintf("otpauth://totp/intact?secret=%s&issuer=intact", secret)
	return "Dashboard sign-in: add this install's TOTP secret to an authenticator app.\n" +
		"  Setup key: " + secret + "\n" +
		"  Or open this URI on the phone: " + uri + "\n" +
		"  1. In the authenticator app (Google Authenticator, 1Password, Aegis), add an account.\n" +
		"  2. Choose \"Enter a setup key\". Type the name intact and the setup key above. Keep \"Time based\".\n" +
		"  3. Open the dashboard and type the 6-digit code that the app shows.\n" +
		"Run `intact -db <file> -show-totp` to print this again. Keep the key secret.\n"
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
