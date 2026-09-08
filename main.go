package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type Config struct {
	Addr          string
	TrustedProxy  string
	DataDir       string
	BaseURL       string
	SecureCookies bool
	Bootstrap     string
	EchoMail      bool
}

type App struct {
	cfg            Config
	trustedProxies []*net.IPNet
	store          *Store
	tmpl           map[string]*template.Template
	mailer         *Mailer
	links          *linkStore
	loginRL        *limiter
	audit          *AuditLog
}

func main() {
	var cfg Config
	var mail Mailer
	flag.StringVar(&cfg.Addr, "addr", envOr("CD_ADDR", "127.0.0.1:8080"), "listen address (use 0.0.0.0:8080 to serve the LAN)")
	flag.StringVar(&cfg.TrustedProxy, "trusted-proxies", envOr("CD_TRUSTED_PROXIES", ""), "comma-separated IPs/CIDRs of reverse proxies whose X-Forwarded-For may be trusted")
	flag.StringVar(&cfg.DataDir, "data", envOr("CD_DATA", "./data"), "data directory")
	flag.StringVar(&cfg.BaseURL, "base-url", envOr("CD_BASE_URL", ""), "base URL used in emailed links, e.g. http://directory.lan:8080")
	flag.BoolVar(&cfg.SecureCookies, "secure-cookies", envOr("CD_SECURE_COOKIES", "") != "", "set the Secure flag on cookies (enable when behind HTTPS)")
	flag.StringVar(&cfg.Bootstrap, "bootstrap-admin", envOr("CD_BOOTSTRAP_ADMIN", ""), "email address to create as an admin on startup if absent")
	flag.BoolVar(&cfg.EchoMail, "echo-mail", envOr("CD_ECHO_MAIL", "") != "", "also print outgoing mail to the log")
	flag.StringVar(&mail.Host, "smtp-host", envOr("CD_SMTP_HOST", ""), "SMTP relay host (leave empty to write mail to data/outbox)")
	flag.IntVar(&mail.Port, "smtp-port", envInt("CD_SMTP_PORT", 25), "SMTP port")
	flag.StringVar(&mail.User, "smtp-user", envOr("CD_SMTP_USER", ""), "SMTP username")
	flag.StringVar(&mail.Pass, "smtp-pass", envOr("CD_SMTP_PASS", ""), "SMTP password")
	flag.StringVar(&mail.From, "smtp-from", envOr("CD_SMTP_FROM", "club-directory@localhost"), "envelope and header From address")
	flag.BoolVar(&mail.StartTLS, "smtp-starttls", envOr("CD_SMTP_STARTTLS", "") != "", "use STARTTLS if the relay offers it")
	flag.BoolVar(&mail.Insecure, "smtp-insecure", envOr("CD_SMTP_INSECURE", "") != "", "skip TLS certificate verification (self-signed internal relays)")
	flag.Parse()

	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		log.Fatalf("opening data directory: %v", err)
	}
	mail.Outbox = store.OutboxDir()
	mail.Echo = cfg.EchoMail || mail.Host == ""

	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://" + strings.Replace(cfg.Addr, "0.0.0.0", "localhost", 1)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")

	proxies, err := parseProxies(cfg.TrustedProxy)
	if err != nil {
		log.Fatal(err)
	}
	app := &App{
		cfg:            cfg,
		trustedProxies: proxies,
		store:          store,
		mailer:         &mail,
		links:          newLinkStore(),
		loginRL:        newLimiter(5, 15*time.Minute),
		audit:          newAuditLog(store.Dir()),
	}
	if err := app.loadTemplates(); err != nil {
		log.Fatalf("loading templates: %v", err)
	}
	if err := app.bootstrap(); err != nil {
		log.Fatalf("bootstrap: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	if strings.HasPrefix(cfg.BaseURL, "https://") && !cfg.SecureCookies {
		log.Print("WARNING: base URL is https but -secure-cookies is off; cookies will be sent over plain HTTP too")
	}
	go func() {
		log.Printf("club directory listening on http://%s (links use %s)", cfg.Addr, cfg.BaseURL)
		log.Printf("data directory: %s | roster: %d members | mail: %s",
			cfg.DataDir, store.Count(), mailDesc(&mail))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	store.Close()
	log.Print("stopped")
}

func mailDesc(m *Mailer) string {
	if !m.Configured() {
		return "no relay configured, writing to data/outbox"
	}
	return fmt.Sprintf("relaying via %s:%d", m.Host, m.Port)
}

// bootstrap makes sure there is always a way in.
func (a *App) bootstrap() error {
	if a.cfg.Bootstrap != "" {
		if _, err := a.store.ByEmail(a.cfg.Bootstrap); err == ErrNotFound {
			m := &Member{
				Email: a.cfg.Bootstrap, Role: RoleAdmin, Status: StatusActive,
				Name: "", CallSign: "",
			}
			if _, err := a.store.Create(m); err != nil {
				return err
			}
			log.Printf("created admin %s — sign in at %s/login", a.cfg.Bootstrap, a.cfg.BaseURL)
			a.audit.Write("system", "admin.bootstrap", a.cfg.Bootstrap, "", "")
		}
	}
	if a.store.Count() == 0 {
		log.Print("roster is empty: start with -bootstrap-admin you@example.com to create the first admin")
	} else if a.store.AdminCount() == 0 {
		log.Print("WARNING: no active admin on the roster; rerun with -bootstrap-admin to regain access")
	}
	return nil
}

func (a *App) loadTemplates() error {
	funcs := template.FuncMap{
		"date": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Format("2 Jan 2006")
		},
		"datetime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Format("2 Jan 2006, 15:04")
		},
		"maybeDate": func(t *time.Time) string {
			if t == nil {
				return "never"
			}
			return t.Format("2 Jan 2006")
		},
		"initials":   initials,
		"lower":      strings.ToLower,
		"statuses":   func() []Status { return allStatuses },
		"staffRoles": func() []StaffRole { return allStaffRoles },
		"eq3":        func(a, b string) bool { return a == b },
	}
	pages, err := fs.Glob(webFS, "web/templates/pages/*.html")
	if err != nil {
		return err
	}
	a.tmpl = map[string]*template.Template{}
	for _, p := range pages {
		name := strings.TrimSuffix(p[strings.LastIndex(p, "/")+1:], ".html")
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(webFS,
			"web/templates/layout.html", "web/templates/partials/*.html", p)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		a.tmpl[name] = t
	}
	return nil
}

func initials(name string) string {
	f := strings.Fields(name)
	if len(f) == 0 {
		return "?"
	}
	if len(f) == 1 {
		return strings.ToUpper(f[0][:1])
	}
	return strings.ToUpper(f[0][:1] + f[len(f)-1][:1])
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(webFS, "web/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/",
		cacheControl(http.FileServer(http.FS(static)))))

	// Public / directory
	mux.HandleFunc("GET /{$}", a.handleDirectory)
	mux.HandleFunc("GET /m/{id}", a.handleMember)
	mux.HandleFunc("GET /photo/{file}", a.handlePhoto)

	// Authentication
	mux.HandleFunc("GET /login", a.handleLoginForm)
	mux.HandleFunc("POST /login", a.handleLoginRequest)
	mux.HandleFunc("GET /login/sent", a.handleLinkSent)
	mux.HandleFunc("GET /auth/{token}", a.handleMagicLink)
	mux.HandleFunc("GET /login/code", a.handleMFAForm)
	mux.HandleFunc("POST /login/code", a.handleMFASubmit)
	mux.HandleFunc("POST /logout", a.handleLogout)

	// Member self-service
	mux.HandleFunc("GET /account", a.requireMember(a.handleAccount))
	mux.HandleFunc("POST /account", a.requireMember(a.handleAccountSave))
	mux.HandleFunc("POST /account/sharing", a.requireMember(a.handleSharingSave))
	mux.HandleFunc("POST /account/photo", a.requireMember(a.handlePhotoUpload))
	mux.HandleFunc("POST /account/photo/remove", a.requireMember(a.handlePhotoRemove))
	mux.HandleFunc("GET /account/security", a.requireMember(a.handleSecurity))
	mux.HandleFunc("POST /account/security/start", a.requireMember(a.handleMFAStart))
	mux.HandleFunc("POST /account/security/enable", a.requireMember(a.handleMFAEnable))
	mux.HandleFunc("POST /account/security/disable", a.requireMember(a.handleMFADisable))
	mux.HandleFunc("POST /account/security/codes", a.requireMember(a.handleNewBackupCodes))
	mux.HandleFunc("POST /account/sessions/revoke", a.requireMember(a.handleRevokeSessions))

	// Admin
	mux.HandleFunc("GET /admin", a.requireAdmin(a.handleAdminRoster))
	mux.HandleFunc("GET /admin/new", a.requireAdmin(a.handleAdminNewForm))
	mux.HandleFunc("POST /admin/new", a.requireAdmin(a.handleAdminCreate))
	mux.HandleFunc("GET /admin/member/{id}", a.requireAdmin(a.handleAdminEditForm))
	mux.HandleFunc("POST /admin/member/{id}", a.requireAdmin(a.handleAdminSave))
	mux.HandleFunc("POST /admin/member/{id}/status", a.requireAdmin(a.handleAdminStatus))
	mux.HandleFunc("POST /admin/member/{id}/invite", a.requireAdmin(a.handleAdminInvite))
	mux.HandleFunc("POST /admin/member/{id}/delete", a.requireAdmin(a.handleAdminDelete))
	mux.HandleFunc("GET /admin/settings", a.requireAdmin(a.handleSettingsForm))
	mux.HandleFunc("POST /admin/settings", a.requireAdmin(a.handleSettingsSave))
	mux.HandleFunc("GET /admin/log", a.requireAdmin(a.handleAuditView))
	mux.HandleFunc("GET /admin/export.csv", a.requireAdmin(a.handleExport))

	return securityHeaders(mux)
}

func cacheControl(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

// securityHeaders keeps the app self-contained: the CSP forbids loading
// anything from another origin, which is what "no public internet" should
// mean in the browser as well as on the network.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
				"form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		// Nothing rendered here should sit in a shared browser cache: pages
		// carry enrolment secrets, backup codes and other members' details.
		if !strings.HasPrefix(r.URL.Path, "/static/") && !strings.HasPrefix(r.URL.Path, "/photo/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		h.ServeHTTP(w, r)
	})
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}
