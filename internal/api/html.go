package api

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/kubecron/kubecron/internal/storage"
)

// fmtDuration formats a millisecond duration in a human-readable way.
func fmtDuration(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	case ms < 3_600_000:
		return fmt.Sprintf("%dm %ds", ms/60_000, (ms%60_000)/1000)
	default:
		return fmt.Sprintf("%dh %dm", ms/3_600_000, (ms%3_600_000)/60_000)
	}
}

// esc safely escapes a string for HTML output.
func esc(s string) string { return html.EscapeString(s) }

// navState is what the nav needs beyond the page title: who is signed in, and
// which clusters exist so the picker can offer them.
type navState struct {
	UserEmail string
	Clusters  []storage.Cluster
	// ActiveCluster is the cluster the current page belongs to, or "" on the
	// fleet overview.
	ActiveCluster string
}

// clusterNav renders the nav's cluster control.
//
// With several clusters it is a picker, so switching does not require going
// back to the overview first. With exactly one there is no choice to make, so
// it degrades to a label naming the cluster — a one-option dropdown is a
// control that cannot do anything. With none it renders nothing at all.
func clusterNav(n navState) string {
	switch len(n.Clusters) {
	case 0:
		return ""
	case 1:
		c := n.Clusters[0]
		cls := "nav-cluster"
		if n.ActiveCluster == c.ID {
			cls += " active"
		}
		return fmt.Sprintf(`<a class="%s" href="/clusters/%s" title="Cluster">%s</a>`,
			cls, esc(c.ID), esc(c.Name))
	}

	var b strings.Builder
	b.WriteString(`<select class="nav-cluster-select" aria-label="Cluster" onchange="location.href=this.value">`)
	overviewSelected := ""
	if n.ActiveCluster == "" {
		overviewSelected = ` selected`
	}
	fmt.Fprintf(&b, `<option value="/"%s>All clusters</option>`, overviewSelected)
	for _, c := range n.Clusters {
		selected := ""
		if c.ID == n.ActiveCluster {
			selected = ` selected`
		}
		fmt.Fprintf(&b, `<option value="/clusters/%s"%s>%s</option>`, esc(c.ID), selected, esc(c.Name))
	}
	b.WriteString(`</select>`)
	return b.String()
}

// htmlHead returns the HTML <head> block + open <body> with nav and toast.
// The signed-in user and a logout link appear when OIDC is enabled.
func htmlHead(title string, n navState) string {
	navRight := ""
	if n.UserEmail != "" {
		navRight = `<div style="margin-left:auto;display:flex;align-items:center;gap:10px;">` +
			`<span style="font-family:var(--font-mono);font-size:0.8rem;color:var(--muted);">` + esc(n.UserEmail) + `</span>` +
			`<button hx-post="/auth/logout" style="font-family:var(--font-mono);font-size:0.8rem;color:var(--muted);background:none;cursor:pointer;border:1px solid var(--border);padding:2px 10px;border-radius:4px;">logout</button>` +
			`</div>`
	}
	// The Overview link only appears when there are several clusters to
	// aggregate. On a single-cluster install "/" redirects to that cluster, so
	// the link would lead straight back to the page the reader is already on.
	overviewLink := ""
	if len(n.Clusters) > 1 {
		cls := "nav-link"
		if n.ActiveCluster == "" {
			cls += " active"
		}
		overviewLink = `<a class="` + cls + `" href="/">Overview</a>`
	}
	_ = title
	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>KubeCron</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;600&family=Syne:wght@400;600;800&display=swap" rel="stylesheet">
<link rel="icon" type="image/svg+xml" href="/static/favicon.svg">
<link rel="icon" type="image/png" sizes="32x32" href="/static/favicon.png">
<link rel="stylesheet" href="/static/app.css">
<script src="https://unpkg.com/htmx.org@2.0.4" defer></script>
</head>
<body>
<nav>
  <a class="logo" href="/"><span style="font-family:var(--font-mono);">[KubeCron]</span></a>
  ` + overviewLink + `
  ` + clusterNav(n) + `
  ` + navRight + `
</nav>
<div id="toast"></div>
<script>
function showToast(msg, ok) {
  const t = document.getElementById('toast');
  t.textContent = msg;
  t.style.borderColor = ok ? 'var(--accent)' : 'var(--red)';
  t.style.color       = ok ? 'var(--accent)' : 'var(--red)';
  t.classList.add('show');
  setTimeout(() => t.classList.remove('show'), 3000);
}
document.body.addEventListener('htmx:afterRequest', function(e) {
  if (!e.detail.successful) showToast('Error ' + e.detail.xhr.status, false);
});
document.addEventListener('htmx:configRequest', function(e) {
  var match = document.cookie.match(/(?:^|;\s*)csrf_token=([^;]+)/);
  if (match) e.detail.headers['X-CSRF-Token'] = match[1];
});
</script>
`
}

// htmlHeadSidebar starts a page with a 2-column layout (sidebar + main).
// sidebarHTML is injected raw into the aside; call htmlFoot to close.
func htmlHeadSidebar(title, sidebarHTML string, n navState) string {
	return htmlHead(title, n) + `<div class="page-layout"><aside class="sidebar">` + sidebarHTML + `</aside><div class="page-main">`
}

// htmlFootSidebar closes a sidebar-layout page.
const htmlFootSidebar = `</div></div>` + htmlFoot

const htmlFoot = `</body></html>`

// statusBadge returns a coloured HTML badge for a run status.
func statusBadge(status string) string {
	cls := map[string]string{
		"succeeded": "badge-success",
		"failed":    "badge-failed",
		"running":   "badge-running",
	}
	c, ok := cls[status]
	if !ok {
		c = "badge-muted"
	}
	return fmt.Sprintf(`<span class="badge %s">%s</span>`, c, esc(status))
}

// breadcrumb builds a "/" separated breadcrumb; each element is raw HTML.
func breadcrumb(parts ...string) string {
	var b strings.Builder
	b.WriteString(`<div class="breadcrumb">`)
	for i, p := range parts {
		if i > 0 {
			b.WriteString(`<span class="breadcrumb-sep">/</span>`)
		}
		b.WriteString(p)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// countdownSpan renders a <span> with data-ts for client-side countdown JS.
func countdownSpan(t time.Time) string {
	return fmt.Sprintf(
		`<span class="countdown" data-ts="%d">%s</span>`,
		t.Unix(), t.Format("02/01 15:04"),
	)
}

// countdownJS is the inline script that powers all countdown timers on a page.
const countdownJS = `<script>
(function() {
  function update() {
    document.querySelectorAll('.countdown[data-ts]').forEach(function(el) {
      var diff = parseInt(el.dataset.ts) - Math.floor(Date.now() / 1000);
      if (diff <= 0) { el.textContent = 'now'; return; }
      var w = Math.floor(diff/604800), d = Math.floor((diff%604800)/86400);
      var h = Math.floor((diff%86400)/3600), m = Math.floor((diff%3600)/60), s = diff%60;
      if (w > 0)      el.textContent = d > 0 ? w+'w '+d+'j' : w+'w';
      else if (d > 0) el.textContent = d+'j '+h+'h';
      else if (h > 0) el.textContent = h+'h '+m+'m';
      else if (m > 0) el.textContent = m+'m '+s+'s';
      else            el.textContent = s+'s';
    });
  }
  update();
  setInterval(update, 1000);
})();
</script>`
