package agsecret

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

const (
	want  = "GOCSPX-right0000000000000000000000"
	other = "GOCSPX-wrong0000000000000000000000"
)

// The app's bundle holds two Google clients; the secret is the literal right
// after the Antigravity client id, not the first GOCSPX- in the file.
var script = []byte(`ee({"x/oauthClient.js"(){"use strict";kfe="` + ClientID + `",_fe="` + want +
	`",z_e="884354919052-x.apps.googleusercontent.com",H_e="` + other + `"}});`)

func TestFromScriptTakesTheSecretOfTheAntigravityClient(t *testing.T) {
	if got := FromScript(script); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := FromScript([]byte(`a="884354919052-x.apps.googleusercontent.com",b="` + other + `"`)); got != "" {
		t.Fatalf("no Antigravity client id, got %q", got)
	}
}

func TestFromInstalledAppReadsTheFirstAppFound(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "app", "out", "main.js")
	os.MkdirAll(filepath.Dir(app), 0o755)
	os.WriteFile(app, script, 0o644)
	if got := FromInstalledApp([]string{filepath.Join(dir, "missing.js"), app}); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := FromInstalledApp([]string{filepath.Join(dir, "missing.js")}); got != "" {
		t.Fatalf("no app, got %q", got)
	}
}

// deb builds a Debian package like the official one: an ar archive whose
// data.tar.xz holds the app.
func deb(t *testing.T, mainJS []byte) []byte {
	t.Helper()
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	files := []struct {
		name string
		body []byte
	}{
		{"./usr/share/antigravity/resources/app/package.json", []byte(`{}`)},
		{"./usr/share/antigravity/resources/app/out/main.js", mainJS},
	}
	for _, f := range files {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		tw.Write(f.body)
	}
	tw.Close()
	var xb bytes.Buffer
	xw, err := xz.NewWriter(&xb)
	if err != nil {
		t.Fatal(err)
	}
	xw.Write(tb.Bytes())
	xw.Close()

	var ab bytes.Buffer
	ab.WriteString("!<arch>\n")
	member := func(name string, body []byte) {
		fmt.Fprintf(&ab, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, 0, 0, 0, "100644", len(body))
		ab.Write(body)
		if len(body)%2 == 1 {
			ab.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("control.tar.xz", []byte("odd"))
	member("data.tar.xz", xb.Bytes())
	return ab.Bytes()
}

func TestFromDebStreamsToTheAppScript(t *testing.T) {
	got, err := FromDeb(bytes.NewReader(deb(t, script)))
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
	if _, err := FromDeb(bytes.NewReader(deb(t, []byte("no client here")))); err == nil {
		t.Fatal("a package without the client id gave no error")
	}
	if _, err := FromDeb(strings.NewReader("not an ar archive")); err == nil {
		t.Fatal("a non-package gave no error")
	}
}

func TestFromRepositoryPicksTheNewestPackage(t *testing.T) {
	pkg := deb(t, script)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dists/antigravity-debian/main/binary-amd64/Packages":
			fmt.Fprint(w, "Package: antigravity\nVersion: 1.9.0-1\nFilename: pool/old.deb\n\n"+
				"Package: antigravity\nVersion: 1.23.2-1776332190\nFilename: pool/new.deb\n\n"+
				"Package: antigravity\nVersion: 1.22.9-1\nFilename: pool/mid.deb\n")
		case "/pool/new.deb":
			w.Write(pkg)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	got, err := FromRepository(context.Background(), srv.Client(), srv.URL)
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
}

func TestNewerComparesEachNumber(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"1.23.2-1", "1.9.0-1", true},
		{"1.9.0-1", "1.23.2-1", false},
		{"1.23.2-2", "1.23.2-1", true},
		{"1.23.2-1", "1.23.2-1", false},
	} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
