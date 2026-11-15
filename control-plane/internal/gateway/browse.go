package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// Browsing: an agent uses a page as the person would, click by click, in the
// gateway's Chrome, signed in as them. Each person and agent has a tab of
// its own in a browser context of its own, kept between calls for a few
// minutes, so a page can be filled in over several calls. Like Shoot, it
// reaches only the installation's own names.

// BrowseIdle is how long a browsing tab is kept without a call.
const BrowseIdle = 5 * time.Minute

type tab struct {
	mu      sync.Mutex // one action at a time
	token   atomic.Value
	ctx     context.Context
	cancel  func()
	last    time.Time
	blocked atomic.Int32
}

type browsing struct {
	mu   sync.Mutex
	tabs map[string]*tab
}

// Action is one step of browsing.
type Action struct {
	URL    string // open this page
	Click  int    // click the element with this number
	Type   int    // type Text into the element with this number
	Text   string
	Submit bool // then press Enter
}

// Page is what the agent sees of a page.
type Page struct {
	Title    string    `json:"title"`
	URL      string    `json:"url"`
	Text     string    `json:"text"`
	Elements []Element `json:"elements"`
}

// Element is a thing on a page to click or type into, numbered.
type Element struct {
	N     int    `json:"n"`
	Tag   string `json:"tag"`
	Type  string `json:"type,omitempty"`
	Label string `json:"label"`
	Value string `json:"value,omitempty"`
}

// snapshot numbers the visible links, buttons and fields of the page and
// returns them with its visible text.
const snapshot = `(() => {
  const seen = (e) => { const r = e.getBoundingClientRect(); const s = getComputedStyle(e);
    return r.width > 0 && r.height > 0 && s.visibility !== 'hidden' && s.display !== 'none'; };
  document.querySelectorAll('[data-hc]').forEach(e => e.removeAttribute('data-hc'));
  const out = []; let n = 0;
  for (const e of document.querySelectorAll('a[href], button, input, select, textarea, [role=button], [role=link], [contenteditable=true]')) {
    if (!seen(e) || e.disabled || (e.type === 'hidden')) continue;
    n++; e.setAttribute('data-hc', String(n));
    let label = (e.getAttribute('aria-label') || e.innerText || e.value || e.placeholder || e.name || e.title || '').trim().replace(/\s+/g, ' ');
    if (!label && e.id) { const l = document.querySelector('label[for="' + e.id + '"]'); if (l) label = l.innerText.trim(); }
    const el = {n, tag: e.tagName.toLowerCase(), label: label.slice(0, 80)};
    if (e.type && e.tagName !== 'BUTTON') el.type = e.type;
    if ((e.tagName === 'INPUT' || e.tagName === 'TEXTAREA') && e.type !== 'password') el.value = String(e.value || '').slice(0, 80);
    out.push(el);
    if (n >= 150) break;
  }
  return JSON.stringify({title: document.title, url: location.href,
    text: (document.body ? document.body.innerText : '').replace(/\n{3,}/g, '\n\n').slice(0, 6000), elements: out});
})()`

func (b *Browser) tab(ctx context.Context, key, token string) (*tab, error) {
	b.browsing.mu.Lock()
	defer b.browsing.mu.Unlock()
	if b.browsing.tabs == nil {
		b.browsing.tabs = map[string]*tab{}
		go b.reap()
	}
	if t := b.browsing.tabs[key]; t != nil && t.ctx.Err() == nil {
		t.token.Store(token)
		return t, nil
	}
	devtools, err := b.devtools(ctx)
	if err != nil {
		return nil, err
	}
	actx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), devtools)
	tctx, cancelTab := chromedp.NewContext(actx, chromedp.WithNewBrowserContext())
	t := &tab{ctx: tctx, cancel: func() { cancelTab(); cancelAlloc() }, last: time.Now()}
	t.token.Store(token)
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
				t.blocked.Add(1)
				fetch.FailRequest(e.RequestID, network.ErrorReasonBlockedByClient).Do(exec)
				return
			}
			b.continueAs(e, t.token.Load().(string)).Do(exec)
		}()
	})
	if err := chromedp.Run(tctx, fetch.Enable(), chromedp.EmulateViewport(1280, 900)); err != nil {
		t.cancel()
		return nil, fmt.Errorf("browser: %w", err)
	}
	b.browsing.tabs[key] = t
	return t, nil
}

// reap closes tabs nobody used for BrowseIdle.
func (b *Browser) reap() {
	for range time.Tick(time.Minute) {
		b.browsing.mu.Lock()
		for k, t := range b.browsing.tabs {
			if t.mu.TryLock() {
				if time.Since(t.last) > BrowseIdle || t.ctx.Err() != nil {
					t.cancel()
					delete(b.browsing.tabs, k)
				}
				t.mu.Unlock()
			}
		}
		b.browsing.mu.Unlock()
	}
}

// Browse does one action in the tab of key, as the person whose token it
// is, and returns the page as it is then.
func (b *Browser) Browse(ctx context.Context, key, token string, a Action) (Page, error) {
	if a.URL != "" && !b.Allowed(a.URL) {
		return Page{}, fmt.Errorf("%w: only pages at https://<name>.%s can be opened", ErrInvalid, b.Domain)
	}
	t, err := b.tab(ctx, key, token)
	if err != nil {
		return Page{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = time.Now()
	actx, cancel := context.WithTimeout(t.ctx, 45*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel) // the caller gave up
	defer stop()
	sel := func(n int) string { return fmt.Sprintf(`[data-hc="%d"]`, n) }
	var steps []chromedp.Action
	switch {
	case a.URL != "":
		steps = append(steps, chromedp.Navigate(a.URL))
	case a.Click > 0:
		steps = append(steps, chromedp.Click(sel(a.Click), chromedp.ByQuery, chromedp.AtLeast(0)))
	case a.Type > 0:
		steps = append(steps,
			chromedp.SetValue(sel(a.Type), "", chromedp.ByQuery),
			chromedp.SendKeys(sel(a.Type), a.Text, chromedp.ByQuery))
		if a.Submit {
			steps = append(steps, chromedp.SendKeys(sel(a.Type), kb.Enter, chromedp.ByQuery))
		}
	}
	if a.Click > 0 || a.Type > 0 {
		// The numbers are those of the last look at the page.
		var exists bool
		n := a.Click + a.Type
		if err := chromedp.Run(actx, chromedp.Evaluate(fmt.Sprintf(`!!document.querySelector('[data-hc="%d"]')`, n), &exists)); err != nil || !exists {
			return Page{}, fmt.Errorf("%w: there is no element %d on the page now; read the page again for the current numbers", ErrInvalid, n)
		}
	}
	steps = append(steps, chromedp.Sleep(time.Second)) // let the page react
	var raw string
	steps = append(steps, chromedp.Evaluate(snapshot, &raw))
	if err := chromedp.Run(actx, steps...); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Page{}, errors.New("browser: the page did not answer within 45 seconds")
		}
		return Page{}, fmt.Errorf("browser: %w", err)
	}
	var p Page
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return Page{}, fmt.Errorf("browser: %w", err)
	}
	if u, err := url.Parse(p.URL); err == nil {
		switch strings.ToLower(u.Hostname()) {
		case "keycloak." + b.Domain, "sso." + b.Domain:
			return p, fmt.Errorf("the page asked to sign in (at %s): the person's sign-in did not reach it", p.URL)
		}
	}
	return p, nil
}

// String is the page as the agent reads it.
func (p Page) String() string {
	var s strings.Builder
	fmt.Fprintf(&s, "Page: %s\nAt: %s\n\n%s\n\nElements (use their numbers to click or type):\n", p.Title, p.URL, strings.TrimSpace(p.Text))
	for _, e := range p.Elements {
		fmt.Fprintf(&s, "[%d] %s", e.N, e.Tag)
		if e.Type != "" {
			fmt.Fprintf(&s, " (%s)", e.Type)
		}
		fmt.Fprintf(&s, " %q", e.Label)
		if e.Value != "" {
			fmt.Fprintf(&s, " value=%q", e.Value)
		}
		s.WriteString("\n")
	}
	return s.String()
}
