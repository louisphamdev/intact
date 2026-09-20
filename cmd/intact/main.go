// Command intact serves provider credentials and forwards requests unchanged.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/louisphamdev/intact/internal/httpapi"
	"github.com/louisphamdev/intact/internal/store"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:20130", "listen address")
	dbPath := flag.String("db", "intact.db", "path to the database file")
	flag.Parse()

	s, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer s.Close()

	log.Printf("intact listening on http://%s", *addr)
	if err := http.ListenAndServe(*addr, httpapi.New(s, nil)); err != nil {
		log.Fatal(err)
	}
}
