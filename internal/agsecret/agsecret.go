// Package agsecret finds the OAuth client secret of the Antigravity app. intact
// ships no copy: it reads the secret from an installed app, or from the official
// Linux package that Google publishes. Google refuses the sign-in without it.
package agsecret

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/ulikunitz/xz"
)

// ClientID is the Google OAuth client of the Antigravity app.
const ClientID = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"

// RepositoryURL is Google's APT repository for the Antigravity app.
const RepositoryURL = "https://us-central1-apt.pkg.dev/projects/antigravity-auto-updater-dev"

// The bundle holds a second Google client with its own GOCSPX- secret, so take
// only the literal that follows the Antigravity client id.
var secretAfterID = regexp.MustCompile(`"` + regexp.QuoteMeta(ClientID) +
	`",[A-Za-z_$][\w$]*="(GOCSPX-[A-Za-z0-9_-]{20,})"`)

const (
	appScript    = "resources/app/out/main.js"
	maxScript    = 64 << 20
	maxPackages  = 8 << 20
	maxPackageSz = 1 << 30
)

// FromScript returns the secret in the app's main script, or "".
func FromScript(b []byte) string {
	if m := secretAfterID.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// InstalledAppPaths are the places where the app's main script is after a
// default install on this system.
func InstalledAppPaths() []string {
	home, _ := os.UserHomeDir()
	var dirs []string
	switch runtime.GOOS {
	case "darwin":
		dirs = []string{"/Applications/Antigravity.app/Contents/Resources",
			filepath.Join(home, "Applications/Antigravity.app/Contents/Resources")}
	case "windows":
		dirs = []string{filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Antigravity", "resources"),
			filepath.Join(os.Getenv("ProgramFiles"), "Antigravity", "resources")}
	default:
		dirs = []string{"/usr/share/antigravity/resources", "/opt/antigravity/resources",
			filepath.Join(home, ".local/share/antigravity/resources")}
	}
	paths := make([]string, len(dirs))
	for i, d := range dirs {
		paths[i] = filepath.Join(d, "app", "out", "main.js")
	}
	return paths
}

// FromInstalledApp returns the secret from the first of paths that holds it, or "".
func FromInstalledApp(paths []string) string {
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(f, maxScript))
		f.Close()
		if s := FromScript(b); s != "" {
			return s
		}
	}
	return ""
}

// FromRepository downloads the newest app package from the APT repository at
// base and reads the secret from it as it streams. Nothing is written to disk.
func FromRepository(ctx context.Context, client *http.Client, base string) (string, error) {
	base = strings.TrimSuffix(base, "/")
	idx, err := get(ctx, client, base+"/dists/antigravity-debian/main/binary-amd64/Packages")
	if err != nil {
		return "", err
	}
	raw, err := io.ReadAll(io.LimitReader(idx, maxPackages))
	idx.Close()
	if err != nil {
		return "", fmt.Errorf("read the package index: %w", err)
	}
	file := newestPackage(raw)
	if file == "" {
		return "", errors.New("the package index lists no antigravity package")
	}
	body, err := get(ctx, client, base+"/"+file)
	if err != nil {
		return "", err
	}
	defer body.Close()
	return FromDeb(io.LimitReader(body, maxPackageSz))
}

func get(ctx context.Context, client *http.Client, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// newestPackage returns the Filename of the newest antigravity entry in an APT
// Packages index.
func newestPackage(index []byte) string {
	var best, bestVer string
	for _, stanza := range strings.Split(string(index), "\n\n") {
		var name, ver, file string
		for _, line := range strings.Split(stanza, "\n") {
			k, v, _ := strings.Cut(line, ": ")
			switch k {
			case "Package":
				name = v
			case "Version":
				ver = v
			case "Filename":
				file = v
			}
		}
		if name == "antigravity" && file != "" && (best == "" || newer(ver, bestVer)) {
			best, bestVer = file, ver
		}
	}
	return best
}

// newer reports whether version a is newer than b, number by number.
func newer(a, b string) bool {
	split := func(s string) []int {
		var n []int
		for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r < '0' || r > '9' }) {
			v, _ := strconv.Atoi(f)
			n = append(n, v)
		}
		return n
	}
	x, y := split(a), split(b)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return len(x) > len(y)
}

// FromDeb reads the secret from a Debian package: the ar member data.tar.xz,
// then the app's main script inside it.
func FromDeb(r io.Reader) (string, error) {
	br := bufio.NewReader(r)
	magic := make([]byte, 8)
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != "!<arch>\n" {
		return "", errors.New("the download is not a Debian package")
	}
	hdr := make([]byte, 60)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			return "", errors.New("the package has no data.tar.xz")
		}
		name := strings.TrimSuffix(strings.TrimSpace(string(hdr[:16])), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(hdr[48:58])), 10, 64)
		if err != nil || size < 0 {
			return "", errors.New("the package has a bad member header")
		}
		if name == "data.tar.xz" {
			return fromDataTar(io.LimitReader(br, size))
		}
		// Members are padded to an even length.
		if _, err := io.CopyN(io.Discard, br, size+size%2); err != nil {
			return "", errors.New("the package is truncated")
		}
	}
}

func fromDataTar(r io.Reader) (string, error) {
	xr, err := xz.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("data.tar.xz: %w", err)
	}
	tr := tar.NewReader(xr)
	for {
		h, err := tr.Next()
		if err != nil {
			return "", fmt.Errorf("the package has no %s with the Antigravity client", appScript)
		}
		if !strings.HasSuffix(h.Name, "/"+appScript) {
			continue
		}
		var b bytes.Buffer
		if _, err := io.Copy(&b, io.LimitReader(tr, maxScript)); err != nil {
			return "", fmt.Errorf("read %s: %w", h.Name, err)
		}
		if s := FromScript(b.Bytes()); s != "" {
			return s, nil
		}
	}
}
