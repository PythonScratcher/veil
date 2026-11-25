package main

import (
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"
)

// UpdateCfg brands the built-in decoy: a large enterprise software company website.
// (The JSON key stays "update_site" so existing configs keep working.)
type UpdateCfg struct {
	Product string `json:"product,omitempty"` // company name, default "Corvane Software"
	Vendor  string `json:"vendor,omitempty"`  // short name, default "Corvane"
}

type sitePage struct{ title, lead, body string }

// updateSite builds the default decoy: a corporate software-company website with many static pages.
func (s *server) updateSite() http.Handler {
	company, short := "Corvane Software", "Corvane"
	if u := s.cfg.Update; u != nil {
		if u.Product != "" {
			company = u.Product
		}
		if u.Vendor != "" {
			short = u.Vendor
		}
	}
	esc := html.EscapeString
	nav := [][2]string{{"/products", "Products"}, {"/solutions", "Solutions"}, {"/pricing", "Pricing"}, {"/security", "Security"}, {"/about", "Company"}}

	shell := func(w http.ResponseWriter, cur, title, body string) {
		var n strings.Builder
		for _, l := range nav {
			cls := ""
			if l[0] == cur {
				cls = ` class="on"`
			}
			fmt.Fprintf(&n, `<a href="%s"%s>%s</a>`, l[0], cls, l[1])
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>%s</title>
<meta name="viewport" content="width=device-width,initial-scale=1"><meta name="description" content="%s builds cloud software for enterprise teams."><link rel="icon" href="data:,">
<style>
*{box-sizing:border-box}body{margin:0;font:16px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;color:#18212f;background:#fff}
a{color:#1d5fd6}.w{max-width:70em;margin:0 auto;padding:0 1.5em}
header{position:sticky;top:0;background:rgba(255,255,255,.96);border-bottom:1px solid #e6e9ef;z-index:5}
header .w{display:flex;align-items:center;height:68px}.logo{font-weight:800;font-size:1.35em;color:#0b1b34;text-decoration:none;letter-spacing:-.01em;margin-right:2.4em}
.logo i{display:inline-block;width:.8em;height:.8em;background:linear-gradient(135deg,#1d5fd6,#7b3fe4);border-radius:3px;margin-right:.35em}
nav a{margin-right:1.6em;color:#3a475a;text-decoration:none;font-size:.95em}nav a.on,nav a:hover{color:#1d5fd6}
.cta{margin-left:auto;display:flex;gap:1em;align-items:center}.cta a{text-decoration:none;font-size:.92em}
.btn{display:inline-block;padding:.7em 1.4em;background:#1d5fd6;color:#fff!important;border-radius:6px;font-weight:600;text-decoration:none}
.btn.alt{background:transparent;border:1px solid rgba(255,255,255,.5)}.btn.sm{padding:.45em 1em}
.hero{background:linear-gradient(135deg,#0b1b34 0%%,#1b2f5e 60%%,#3b2a85 100%%);color:#fff;padding:6em 0 5.5em}
.hero h1{font-size:3em;line-height:1.1;margin:0 0 .4em;max-width:15em;letter-spacing:-.02em}.hero p{font-size:1.2em;max-width:36em;color:#c9d4ea}.hero .btn{margin:1em 1em 0 0}
.page-hero{background:#f4f6fa;padding:3.5em 0 2.5em}.page-hero h1{margin:0 0 .3em;font-size:2.2em;color:#0b1b34}.page-hero p{margin:0;max-width:40em;color:#4a566a;font-size:1.1em}
.logos{padding:2em 0;border-bottom:1px solid #e6e9ef;color:#8b95a5;text-align:center;font-weight:700;letter-spacing:.08em;font-size:.95em}.logos span{margin:0 1.4em;display:inline-block;line-height:2}
section{padding:3.5em 0}section.alt{background:#f7f8fb}h2{font-size:1.9em;margin:0 0 .5em;color:#0b1b34;letter-spacing:-.01em}.sub{color:#5a6577;max-width:40em;margin:0 0 2em}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(16em,1fr));gap:1.4em}.card{background:#fff;border:1px solid #e6e9ef;border-radius:10px;padding:1.5em}
.card h3{margin:.2em 0 .4em;color:#0b1b34;font-size:1.1em}.card p{margin:0;color:#4a566a;font-size:.95em}.tag{font-size:.75em;font-weight:700;color:#7b3fe4;letter-spacing:.08em}
.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(10em,1fr));gap:1.5em;text-align:center}.stats b{display:block;font-size:2.4em;color:#1d5fd6;letter-spacing:-.02em}.stats span{color:#5a6577;font-size:.92em}
.price .card{text-align:left}.price b{font-size:2em;color:#0b1b34}.price ul{padding-left:1.1em;color:#4a566a;font-size:.93em}
.prose{max-width:46em}.prose p{color:#3a475a}
footer{background:#0b1b34;color:#a9b5cc;padding:3.5em 0 2em;font-size:.9em}footer .cols{display:grid;grid-template-columns:2fr repeat(4,1fr);gap:2em}
footer h4{color:#fff;margin:0 0 .8em;font-size:.95em}footer a{display:block;color:#a9b5cc;text-decoration:none;margin:.35em 0}footer a:hover{color:#fff}
.legal{border-top:1px solid #243452;margin-top:2.5em;padding-top:1.3em;display:flex;justify-content:space-between;flex-wrap:wrap;gap:1em}
@media(max-width:760px){.hero h1{font-size:2.1em}nav{display:none}footer .cols{grid-template-columns:1fr 1fr}}
</style></head><body>
<header><div class="w"><a class="logo" href="/"><i></i>%s</a><nav>%s</nav><div class="cta"><a href="/contact">Contact sales</a><a class="btn sm" href="/contact">Request a demo</a></div></div></header>
%s
<footer><div class="w"><div class="cols"><div><b style="color:#fff;font-size:1.2em">%s</b><p>Cloud software that helps enterprise teams build, run and secure the applications their business depends on.</p></div>
<div><h4>Products</h4><a href="/products">Platform</a><a href="/products">Analytics</a><a href="/products">Automation</a><a href="/products">Identity</a></div>
<div><h4>Solutions</h4><a href="/solutions">Financial services</a><a href="/solutions">Healthcare</a><a href="/solutions">Retail</a><a href="/solutions">Public sector</a></div>
<div><h4>Resources</h4><a href="/security">Trust center</a><a href="/status">System status</a><a href="/pricing">Pricing</a><a href="/newsroom">Newsroom</a></div>
<div><h4>Company</h4><a href="/about">About us</a><a href="/careers">Careers</a><a href="/contact">Contact</a><a href="/newsroom">Press</a></div></div>
<div class="legal"><span>&copy; %d %s All rights reserved.</span><span><a style="display:inline;margin-right:1.2em" href="/privacy">Privacy</a><a style="display:inline;margin-right:1.2em" href="/terms">Terms</a><a style="display:inline" href="/security">Security</a></span></div></div></footer>
</body></html>`, esc(title), esc(short), esc(short), n.String(), body, esc(short), time.Now().Year(), esc(company))
	}
	inner := func(h, lead, body string) string {
		return `<div class="page-hero"><div class="w"><h1>` + h + `</h1><p>` + lead + `</p></div></div><section><div class="w">` + body + `</div></section>`
	}
	card := func(tag, h, p string) string {
		t := ""
		if tag != "" {
			t = `<div class="tag">` + tag + `</div>`
		}
		return `<div class="card">` + t + `<h3>` + h + `</h3><p>` + p + `</p></div>`
	}

	pages := map[string]sitePage{
		"/products": {"Products", "One connected platform for building, running and securing modern applications.", `<div class="grid">` +
			card("PLATFORM", short+" Cloud", "Managed application hosting with autoscaling, global edge delivery and 99.99% uptime SLA.") +
			card("ANALYTICS", short+" Insight", "Real-time dashboards and warehouse-native analytics on all of your operational data.") +
			card("AUTOMATION", short+" Flow", "Low-code workflow automation that connects more than 600 business applications.") +
			card("IDENTITY", short+" Identity", "Single sign-on, adaptive multi-factor authentication and lifecycle management.") +
			card("OBSERVABILITY", short+" Pulse", "Unified logs, metrics and traces with AI-assisted incident detection.") +
			card("DEVELOPER", short+" API", "Well-documented REST and streaming APIs, SDKs for eight languages and a generous sandbox.") + `</div>`},
		"/solutions": {"Solutions", "Industry solutions built with the teams that run the world's most regulated businesses.", `<div class="grid">` +
			card("", "Financial services", "Real-time risk reporting, audit trails and data residency controls for banks and insurers.") +
			card("", "Healthcare", "HIPAA-ready workflows that connect clinical, scheduling and billing systems.") +
			card("", "Retail &amp; e-commerce", "Inventory, fulfilment and customer analytics that scale through peak season.") +
			card("", "Public sector", "FedRAMP-aligned hosting and accessible citizen-facing services.") +
			card("", "Manufacturing", "Connected-factory telemetry and predictive maintenance at scale.") +
			card("", "Technology", "Embed our APIs and ship faster with a platform engineers actually enjoy.") + `</div>`},
		"/pricing": {"Pricing", "Simple plans that scale with your organization. Annual billing saves 20%.", `<div class="grid price">` +
			`<div class="card"><div class="tag">TEAM</div><h3>Team</h3><b>$29</b> per user / month<ul><li>Up to 50 users</li><li>Core platform and analytics</li><li>Email and chat support</li></ul></div>` +
			`<div class="card"><div class="tag">BUSINESS</div><h3>Business</h3><b>$79</b> per user / month<ul><li>Unlimited users</li><li>SSO and advanced permissions</li><li>24/7 priority support</li></ul></div>` +
			`<div class="card"><div class="tag">ENTERPRISE</div><h3>Enterprise</h3><b>Custom</b><ul><li>Dedicated environment</li><li>Custom SLAs and data residency</li><li>Named technical account manager</li></ul><p><a class="btn sm" href="/contact">Contact sales</a></p></div></div>`},
		"/security": {"Security &amp; trust", "Security is built into every layer of our products and our company.", `<div class="grid">` +
			card("", "Certifications", "SOC 2 Type II, ISO 27001, ISO 27018 and annual independent penetration testing.") +
			card("", "Data protection", "Encryption in transit and at rest, customer-managed keys and regional data residency.") +
			card("", "Responsible disclosure", "We welcome reports from researchers. Contact our security team through the details on our contact page.") + `</div>`},
		"/about": {"About " + short, "We started in 2011 with a simple idea: enterprise software should be a pleasure to use.", `<div class="prose"><p>` + esc(company) + ` is a software company. Our platform serves over 30,000 organizations, from fast-growing start-ups to global enterprises.</p><p>We are guided by three principles: build for the long term, earn trust every day, and make our customers successful. We are publicly committed to reaching net-zero emissions across our operations by 2035.</p></div>`},
		"/careers": {"Careers", "Join thousands of colleagues building software that matters.", `<div class="grid">` +
			card("ENGINEERING", "Senior Backend Engineer", "Distributed systems in Go and Rust. Remote-friendly across Europe and North America.") +
			card("ENGINEERING", "Site Reliability Engineer", "Run the infrastructure behind a 99.99% platform.") +
			card("PRODUCT", "Group Product Manager, Identity", "Own the roadmap for our fastest-growing product line.") +
			card("SALES", "Enterprise Account Executive", "Partner with Fortune 500 customers on their transformation.") + `</div><p style="margin-top:2em;color:#5a6577">Applications are handled through our recruiting team. Please see the contact page for details.</p>`},
		"/contact": {"Contact us", "Talk to our team about your requirements.", `<div class="grid">` +
			card("", "Sales", "Speak to an account executive about plans, demos and procurement. Monday to Friday, 08:00&ndash;18:00.") +
			card("", "Support", "Customers can open and track cases from the support portal, 24 hours a day.") +
			card("", "Press &amp; analysts", "Media enquiries are handled by our communications team.") + `</div>`},
		"/newsroom": {"Newsroom", "The latest from " + esc(short) + ".", `<div class="prose"><h3>` + esc(short) + ` named a Leader in cloud application platforms</h3><p>October 2026 &mdash; Independent analysts recognised our platform for completeness of vision and ability to execute.</p><h3>` + esc(short) + ` Cloud now available in three new regions</h3><p>September 2026 &mdash; Customers can now host workloads in Frankfurt, S&atilde;o Paulo and Singapore.</p><h3>Annual customer conference returns this spring</h3><p>August 2026 &mdash; Registration opens next month.</p></div>`},
		"/status": {"System status", "Current availability of " + esc(short) + " services.", `<div class="grid">` +
			card("OPERATIONAL", "Platform", "All regions operating normally.") + card("OPERATIONAL", "API", "All endpoints operating normally.") + card("OPERATIONAL", "Identity", "All systems operational.") + `</div>`},
		"/privacy": {"Privacy policy", "How we collect, use and protect personal information.", `<div class="prose"><p>We process personal data only as needed to provide our services, secure our systems and meet legal obligations. We do not sell personal data. Customers can request access, correction or deletion at any time through their account administrator or by contacting our privacy team.</p></div>`},
		"/terms":   {"Terms of service", "The terms that govern use of our websites and services.", `<div class="prose"><p>Use of our products is governed by the master subscription agreement signed with your organization. Use of this website is subject to these terms and to applicable law.</p></div>`},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		shell(w, "/", company+" | Enterprise cloud software", fmt.Sprintf(`<div class="hero"><div class="w"><h1>The platform enterprise teams build on</h1>
<p>%s helps the world's largest organizations build, run and secure the applications their business depends on &mdash; on one connected platform.</p>
<a class="btn" href="/contact">Request a demo</a><a class="btn alt" href="/products">Explore products</a></div></div>
<div class="logos"><div class="w"><span>ARCLIGHT</span><span>NORTHBRIDGE</span><span>KESTREL BANK</span><span>MONTAGUE</span><span>OAKHAVEN</span><span>LUMINAIRE</span></div></div>
<section><div class="w"><h2>Everything your teams need, in one place</h2><p class="sub">From first line of code to global scale, our products work together so your teams can move faster with less risk.</p><div class="grid">%s%s%s</div></div></section>
<section class="alt"><div class="w"><div class="stats"><div><b>30,000+</b><span>customers worldwide</span></div><div><b>99.99%%</b><span>platform uptime</span></div><div><b>22</b><span>global regions</span></div><div><b>600+</b><span>integrations</span></div></div></div></section>
<section><div class="w"><h2>Trusted where it matters most</h2><p class="sub">Independently audited, globally compliant and built for the most demanding security reviews.</p><a class="btn" href="/security">Visit the trust center</a></div></section>`,
			esc(company), card("PLATFORM", short+" Cloud", "Managed hosting with autoscaling and global edge delivery."), card("ANALYTICS", short+" Insight", "Real-time analytics on all of your operational data."), card("AUTOMATION", short+" Flow", "Low-code automation across 600+ business applications.")))
	})
	for path, pg := range pages {
		path, pg := path, pg
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			shell(w, path, strings.NewReplacer("&amp;", "&").Replace(pg.title)+" | "+short, inner(pg.title, pg.lead, pg.body))
		})
	}
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { notFoundPage(w) })
	return nginxish(mux)
}

func notFoundPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(404)
	w.Write([]byte(notFound))
}

const notFound = `<html>
<head><title>404 Not Found</title></head>
<body>
<center><h1>404 Not Found</h1></center>
<hr><center>nginx</center>
</body>
</html>
`
