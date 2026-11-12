package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// Browser takes screenshots with the control plane's headless Chrome, as a
// person. Chrome sends every name to Traefik; this lets through only
// requests to the installation's own names, each with the person's Keycloak
// token, which the sign-in in front of the apps accepts. Each screenshot
// gets a fresh browser context, so nothing of one person stays for the next.
type Browser struct {
	URL    string // Chrome's DevTools, e.g. http://control-plane_browser:9222
	Domain string
}

// Shot is a screenshot and what the page was.
type Shot struct {
	PNG     []byte
	Title   string
	URL     string // where the page ended up
	Blocked int    // requests to other hosts that were refused
}

const maxShot = 10 << 20

// Allowed tells whether the browser may load u: https at the installation's
// own names only.
func (b *Browser) Allowed(u string) bool {
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "https" || p.User != nil {
		return false
	}
	host := strings.ToLower(p.Hostname())
	return host == b.Domain || strings.HasSuffix(host, "."+b.Domain)
}

// devtools is Chrome's DevTools URL with the host as an IP: Chrome refuses
// requests whose Host header is a name other than localhost.
func (b *Browser) devtools(ctx context.Context) (string, error) {
	u, err := url.Parse(b.URL)
	if err != nil {
		return "", err
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil || len(addrs) == 0 {
		return "", fmt.Errorf("browser: %s: %v", u.Hostname(), err)
	}
	u.Host = net.JoinHostPort(addrs[0].IP.String(), u.Port())
	return u.String(), nil
}

// Shoot loads page as the person whose token this is and takes a
// screenshot: of the window, or of the whole page.
func (b *Browser) Shoot(ctx context.Context, token, page string, width, height int, fullPage bool) (Shot, error) {
	if !b.Allowed(page) {
		return Shot{}, fmt.Errorf("%w: only pages at https://<name>.%s can be shot", ErrInvalid, b.Domain)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	devtools, err := b.devtools(ctx)
	if err != nil {
		return Shot{}, err
	}
	actx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, devtools)
	defer cancelAlloc()
	tctx, cancelTab := chromedp.NewContext(actx, chromedp.WithNewBrowserContext())
	defer cancelTab()

	var blocked atomic.Int32
	chromedp.ListenTarget(tctx, func(ev any) {
		e, ok := ev.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		go func() {
			c := chromedp.FromContext(tctx)
			if c == nil || c.Target == nil {
				return
			}
			exec := cdp.WithExecutor(tctx, c.Target)
			if !b.Allowed(e.Request.URL) {
				blocked.Add(1)
				fetch.FailRequest(e.RequestID, network.ErrorReasonBlockedByClient).Do(exec)
				return
			}
			headers := []*fetch.HeaderEntry{{Name: "Authorization", Value: "Bearer " + token}}
			for k, v := range e.Request.Headers {
				if !strings.EqualFold(k, "Authorization") {
					headers = append(headers, &fetch.HeaderEntry{Name: k, Value: fmt.Sprint(v)})
				}
			}
			fetch.ContinueRequest(e.RequestID).WithHeaders(headers).Do(exec)
		}()
	})

	var shot Shot
	capture := chromedp.CaptureScreenshot(&shot.PNG)
	if fullPage {
		capture = chromedp.FullScreenshot(&shot.PNG, 100)
	}
	err = chromedp.Run(tctx,
		fetch.Enable(),
		chromedp.EmulateViewport(int64(width), int64(height)),
		chromedp.Navigate(page),
		// Let scripts draw what they load after the load event.
		chromedp.Sleep(time.Second),
		chromedp.Title(&shot.Title),
		chromedp.Location(&shot.URL),
		capture,
	)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Shot{}, fmt.Errorf("browser: %s did not load within a minute", page)
		}
		return Shot{}, fmt.Errorf("browser: %w", err)
	}
	shot.Blocked = int(blocked.Load())
	// A page that sent the browser to sign in was not shot as the person.
	if p, err := url.Parse(shot.URL); err == nil {
		switch strings.ToLower(p.Hostname()) {
		case "keycloak." + b.Domain, "sso." + b.Domain:
			return Shot{}, fmt.Errorf("%s asked to sign in instead of showing the page (ended at %s)", page, shot.URL)
		}
	}
	if len(shot.PNG) > maxShot {
		return Shot{}, fmt.Errorf("%w: the screenshot is larger than %d MB; try without full_page", ErrInvalid, maxShot>>20)
	}
	return shot, nil
}
