package mime

import (
	"context"
	"encoding/base64"
	"html"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/sync/errgroup"
)

const (
	// maxRemoteAssets caps how many distinct URLs one message may send us
	// after. A newsletter runs to a few dozen; a thousand is a message
	// playing a different game.
	maxRemoteAssets = 200
	// remoteConcurrency is how many are in flight at once. Six keeps a
	// forty-image newsletter under a couple of seconds without opening a
	// message looking like a crawl to the host on the other end.
	remoteConcurrency = 6
)

// remoteResult counts what the fetch pass managed, so the header block can
// say it plainly rather than leave the reader guessing why a picture is a
// grey box.
type remoteResult struct{ ok, failed int }

func (r remoteResult) tried() bool { return r.ok+r.failed > 0 }

var (
	// Pictures arrive as <img src>, and <source> inside a <picture>.
	reMediaTag = regexp.MustCompile(`(?is)<(?:img|source)\b[^>]*>`)
	reSrcAttr  = regexp.MustCompile(`(?is)\bsrc\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	// srcset would win over the src beside it, and it holds a list of URLs
	// with descriptors rather than one URL. Dropping it makes the browser
	// fall back to the src we did fold in.
	reSrcsetAttr = regexp.MustCompile(`(?is)\s(?:data-)?srcset\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	// background= is HTML4, and mail is full of HTML4.
	reBgAttr = regexp.MustCompile(`(?is)(^|\s)background\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	// CSS urls are looked for only inside the two places CSS lives, so that
	// a "url(...)" written in the prose of the message is left as prose.
	reStyleTag  = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
	reStyleAttr = regexp.MustCompile(`(?is)\bstyle\s*=\s*("[^"]*"|'[^']*')`)
	reCSSURL    = regexp.MustCompile(`(?is)url\(\s*("[^"]*"|'[^']*'|[^)]*?)\s*\)`)
)

// inlineRemote folds the pictures the message hosts elsewhere into the page
// as data: URIs, so that the page itself still loads nothing.
//
// It walks the markup twice with the same traversal: once to collect what is
// pointed at, then -- after the fetches -- once to substitute. A URL nothing
// answers keeps its original reference, and the browser shows a broken image.
func inlineRemote(ctx context.Context, h string, fetch FetchFunc, bud *int) (string, remoteResult) {
	var urls []string
	seen := map[string]bool{}
	rewriteRemote(h, func(u string) (string, bool) {
		if !seen[u] && len(urls) < maxRemoteAssets {
			seen[u] = true
			urls = append(urls, u)
		}
		return "", false
	})
	if len(urls) == 0 {
		return h, remoteResult{}
	}

	type got struct {
		data  []byte
		ctype string
		err   error
	}
	out := make([]got, len(urls))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(remoteConcurrency)
	for i, u := range urls {
		g.Go(func() error {
			d, ct, err := fetch(gctx, u)
			out[i] = got{d, ct, err}
			// One picture failing is not the page failing, so nothing is
			// returned that would cancel the rest.
			return nil
		})
	}
	_ = g.Wait()

	// Spending the budget in the order the message asks for things keeps the
	// result the same run to run, and spends it on what is near the top.
	uris := make(map[string]string, len(urls))
	var res remoteResult
	for i, u := range urls {
		r := out[i]
		if r.err != nil || len(r.data) == 0 || len(r.data) > *bud {
			res.failed++
			continue
		}
		*bud -= len(r.data)
		ctype := r.ctype
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		uris[u] = "data:" + ctype + ";base64," + base64.StdEncoding.EncodeToString(r.data)
		res.ok++
	}

	h = rewriteRemote(h, func(u string) (string, bool) {
		v, ok := uris[u]
		return v, ok
	})
	return h, res
}

// rewriteRemote calls visit for every http(s) URL the markup points at and
// substitutes what it returns. It is the one traversal both passes use, so
// what was collected and what is replaced cannot drift apart.
func rewriteRemote(h string, visit func(string) (string, bool)) string {
	// attr rewrites one attribute value, quotes and all.
	attr := func(quoted string) string {
		q, raw := splitQuoted(quoted)
		u := html.UnescapeString(strings.TrimSpace(raw))
		if !isRemoteURL(u) {
			return quoted
		}
		v, ok := visit(u)
		if !ok {
			return quoted
		}
		// A data: URI carries "=" and ";", which an unquoted attribute value
		// may not, so an unquoted one comes back quoted.
		if q == "" {
			q = `"`
		}
		return q + v + q
	}
	// tail replaces the value at the end of a matched attribute, leaving the
	// name and the "=" between them untouched.
	tail := func(match, value string) string {
		return match[:len(match)-len(value)] + attr(value)
	}

	h = reMediaTag.ReplaceAllStringFunc(h, func(tag string) string {
		tag = reSrcAttr.ReplaceAllStringFunc(tag, func(a string) string {
			return tail(a, reSrcAttr.FindStringSubmatch(a)[1])
		})
		return reSrcsetAttr.ReplaceAllString(tag, "")
	})
	h = reBgAttr.ReplaceAllStringFunc(h, func(a string) string {
		return tail(a, reBgAttr.FindStringSubmatch(a)[2])
	})

	css := func(block string) string {
		return reCSSURL.ReplaceAllStringFunc(block, func(m string) string {
			g := reCSSURL.FindStringSubmatch(m)
			_, raw := splitQuoted(strings.TrimSpace(g[1]))
			u := html.UnescapeString(strings.TrimSpace(raw))
			if !isRemoteURL(u) {
				return m
			}
			v, ok := visit(u)
			if !ok {
				return m
			}
			// Unquoted: base64 holds nothing that ends a CSS url() token,
			// and it sidesteps the quote already wrapping a style attribute.
			return "url(" + v + ")"
		})
	}
	h = reStyleTag.ReplaceAllStringFunc(h, css)
	h = reStyleAttr.ReplaceAllStringFunc(h, css)
	return h
}

// splitQuoted takes an attribute value apart into the quote character that
// wrapped it, if any, and what was inside.
func splitQuoted(v string) (quote, raw string) {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[:1], v[1 : len(v)-1]
	}
	return "", v
}

// isRemoteURL answers whether a reference is one that would send the browser
// to the network. It is checked before url.Parse so that the data: URIs the
// cid: pass has already written -- megabytes of base64 -- are dismissed on
// their first five bytes.
func isRemoteURL(s string) bool {
	if len(s) < len("http://a") {
		return false
	}
	if !strings.EqualFold(s[:5], "http:") && !strings.EqualFold(s[:6], "https:") {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Host != ""
}
