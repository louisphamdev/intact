package servertools

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// FetchConfig holds the limits a request sets on the web fetch tool.
type FetchConfig struct {
	MaxUses   int // 0 is no limit
	Allowed   []string
	Blocked   []string
	MaxTokens int // 0 is the default
}

// Fetched is a page the tool retrieved.
type Fetched struct {
	URL, Title, Text string
	At               time.Time
}

// FetchError carries the error code Anthropic's web fetch tool reports.
type FetchError struct{ Code string }

func (e *FetchError) Error() string { return e.Code }

const (
	defaultFetchTokens = 100000
	maxFetchURL        = 250
	maxFetchBody       = 4 << 20
)

// Fetch retrieves a page for the web fetch tool. The URL is chosen by the model,
// so it may name only a public address: one that resolves to this machine or to
// a private network is refused, at the connection and again at each redirect.
func Fetch(ctx context.Context, raw string, cfg FetchConfig) (*Fetched, error) {
	u, err := checkURL(raw, cfg)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{
		Timeout: 25 * time.Second,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second, Control: dialControl}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			DisableKeepAlives:   true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if _, err := checkURL(req.URL.String(), cfg); err != nil {
				return err
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &FetchError{"invalid_tool_input"}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; intact-web-fetch)")
	req.Header.Set("Accept", "text/html,text/plain,application/xhtml+xml,application/json;q=0.9,*/*;q=0.5")
	resp, err := hc.Do(req)
	if err != nil {
		var fe *FetchError
		if errors.As(err, &fe) {
			return nil, fe
		}
		if strings.Contains(err.Error(), errPrivate.Error()) {
			return nil, &FetchError{"url_not_allowed"}
		}
		return nil, &FetchError{"url_not_accessible"}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &FetchError{"too_many_requests"}
	}
	if resp.StatusCode >= 400 {
		return nil, &FetchError{"url_not_accessible"}
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if !textual(mt) {
		return nil, &FetchError{"unsupported_content_type"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBody))
	if err != nil {
		return nil, &FetchError{"url_not_accessible"}
	}
	text, title := string(body), ""
	if mt == "text/html" || mt == "application/xhtml+xml" || mt == "" {
		text, title = htmlToText(text)
	}
	limit := cfg.MaxTokens
	if limit <= 0 {
		limit = defaultFetchTokens
	}
	if r := []rune(text); len(r) > limit*4 {
		text = string(r[:limit*4])
	}
	return &Fetched{URL: resp.Request.URL.String(), Title: title, Text: text, At: time.Now().UTC()}, nil
}

func textual(mt string) bool {
	switch {
	case mt == "", strings.HasPrefix(mt, "text/"):
		return true
	case mt == "application/json", mt == "application/xml", mt == "application/xhtml+xml",
		mt == "application/javascript", strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"):
		return true
	}
	return false
}

// checkURL applies the rules that hold before any connection: length, scheme,
// credentials in the URL, and the request's domain lists.
func checkURL(raw string, cfg FetchConfig) (*url.URL, error) {
	if len(raw) > maxFetchURL {
		return nil, &FetchError{"url_too_long"}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, &FetchError{"invalid_tool_input"}
	}
	if u.User != nil {
		return nil, &FetchError{"url_not_allowed"}
	}
	host := strings.ToLower(u.Hostname())
	if len(cfg.Allowed) > 0 && !inDomains(host, cfg.Allowed) {
		return nil, &FetchError{"url_not_allowed"}
	}
	if inDomains(host, cfg.Blocked) {
		return nil, &FetchError{"url_not_allowed"}
	}
	return u, nil
}

// inDomains reports whether host is one of the domains or a subdomain of one.
func inDomains(host string, domains []string) bool {
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if i := strings.IndexByte(d, '/'); i >= 0 {
			d = d[:i]
		}
		if d != "" && (host == d || strings.HasSuffix(host, "."+d)) {
			return true
		}
	}
	return false
}

var errPrivate = errors.New("address is not public")

// dialControl is publicOnly; a test that fetches from a server on this machine
// swaps it.
var dialControl = publicOnly

// publicOnly is the dialer's control: it runs on the address a name resolved to,
// just before the connection, so a name that points inside is refused as well as
// an address written out.
func publicOnly(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errPrivate
	}
	ip := net.ParseIP(host)
	if ip == nil || !publicIP(ip) {
		return errPrivate
	}
	return nil
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func publicIP(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsUnspecified(), ip.IsMulticast(),
		ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast():
		return false
	case cgnat.Contains(ip):
		return false
	}
	if ip4 := ip.To4(); ip4 != nil && (ip4[0] == 0 || ip4[0] >= 240) {
		return false
	}
	return true
}

var (
	reDrop    = regexp.MustCompile(`(?is)<(script|style|noscript|template|svg|head)\b.*?</(script|style|noscript|template|svg|head)\s*>|<!--.*?-->`)
	reTitle   = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reBreak   = regexp.MustCompile(`(?i)</?(p|div|br|li|ul|ol|tr|table|section|article|header|footer|h[1-6]|pre|blockquote)\b[^>]*>`)
	reTag     = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces  = regexp.MustCompile(`[ \t\r\f\v]+`)
	reNewline = regexp.MustCompile(`\n\s*\n+`)
)

// htmlToText reduces a page to the text a reader sees, and its title. It is not
// an HTML parser: scripts, styles and tags go, block tags become line breaks.
func htmlToText(page string) (text, title string) {
	if m := reTitle.FindStringSubmatch(page); m != nil {
		title = strings.TrimSpace(html.UnescapeString(reTag.ReplaceAllString(m[1], "")))
	}
	page = reDrop.ReplaceAllString(page, " ")
	page = reBreak.ReplaceAllString(page, "\n")
	page = reTag.ReplaceAllString(page, "")
	page = html.UnescapeString(page)
	page = reSpaces.ReplaceAllString(page, " ")
	lines := strings.Split(page, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	page = reNewline.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(page), title
}

func (f *Fetched) modelText() string {
	head := "URL: " + f.URL
	if f.Title != "" {
		head = "Title: " + f.Title + "\n" + head
	}
	return fmt.Sprintf("%s\n\n%s", head, f.Text)
}
