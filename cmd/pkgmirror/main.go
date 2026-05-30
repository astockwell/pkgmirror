// Command pkgmirror runs the pkgmirror HTTP service.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/astockwell/pkgmirror/assets"
	"github.com/astockwell/pkgmirror/internal/audit"
	"github.com/astockwell/pkgmirror/internal/auth"
	"github.com/astockwell/pkgmirror/internal/bootstrap"
	"github.com/astockwell/pkgmirror/internal/config"
	"github.com/astockwell/pkgmirror/internal/console"
	consolemw "github.com/astockwell/pkgmirror/internal/console/middleware"
	pkgdb "github.com/astockwell/pkgmirror/internal/db"
	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/packages/container"
	"github.com/astockwell/pkgmirror/internal/policy"
	"github.com/astockwell/pkgmirror/internal/policy/cooldown"
	"github.com/astockwell/pkgmirror/internal/policy/license"
	"github.com/astockwell/pkgmirror/internal/server"
	"github.com/astockwell/pkgmirror/internal/storage"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/tokens"
	"github.com/astockwell/pkgmirror/internal/upstream"
	"github.com/astockwell/pkgmirror/internal/upstreamstore"
	"github.com/astockwell/pkgmirror/internal/users"

	"github.com/gin-gonic/gin"
)

func main() {
	cfg := config.Load()

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	if err := os.MkdirAll(cfg.TmpDir, 0o755); err != nil {
		log.Fatalf("create tmp dir: %v", err)
	}

	// Sweep any leftover pkgmirror staging files in TmpDir. A previous
	// process crashing mid-upload leaves pkgmirror-upload-* (HashedBuffer)
	// and pkgmirror-oci-upload-* (OCI tracker) files behind; nothing in
	// the new process has a reference to them. SweepOrphans only deletes
	// files matching our own prefixes, so anything else operators
	// stashed in TmpDir is untouched.
	if removed, err := container.SweepOrphans(cfg.TmpDir); err != nil {
		log.Printf("warn: orphan sweep: %v", err)
	} else if removed > 0 {
		log.Printf("swept %d orphaned staging files from %s", removed, cfg.TmpDir)
	}

	dbConn, err := pkgdb.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer dbConn.Close()

	pkgModels := models.New(dbConn)
	tenantStore := tenants.New(dbConn)
	userStore := users.New(dbConn)
	tokenStore := tokens.New(dbConn)

	bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	res, err := bootstrap.Ensure(bootCtx, tenantStore, userStore, tokenStore, bootstrap.Options{
		DefaultTenantName:       cfg.DefaultTenantName,
		DefaultTenantVisibility: cfg.DefaultTenantVisibility,
		AdminToken:              cfg.AdminToken,
	})
	bootCancel()
	if err != nil {
		log.Fatalf("bootstrap: %v", err)
	}
	if res.GeneratedAdminToken != "" {
		log.Printf("======================================================================")
		log.Printf("  pkgmirror generated an initial admin token.")
		log.Printf("  This is shown ONCE — save it now; it cannot be recovered later.")
		log.Printf("")
		log.Printf("    %s", res.GeneratedAdminToken)
		log.Printf("")
		log.Printf("  Use it as the password for HTTP Basic auth, or as a Bearer token.")
		log.Printf("======================================================================")
	}

	blobs, err := storage.NewLocalStorage(context.Background(), cfg.BlobDir)
	if err != nil {
		log.Fatalf("open blob storage: %v", err)
	}
	svc := pkgsvc.NewService(pkgModels, blobs)
	svc.TmpDir = cfg.TmpDir

	authn := &auth.TokenAuthenticator{
		Tokens:  tokenStore,
		Users:   userStore,
		Tenants: tenantStore,
	}

	// Supply-chain policy engine. Step 1-4 of the plan: a rule-driven
	// engine with no evaluators yet registered, plus the audit decorator.
	// Real evaluators (cooldown, license allowlist) land in steps 5+.
	auditLogger := audit.New(dbConn, 4096)
	defer auditLogger.Close()
	pruneCtx, prunecancel := context.WithCancel(context.Background())
	defer prunecancel()
	audit.StartPruner(pruneCtx, dbConn)

	ruleStore := policy.NewRuleStore(dbConn)
	if cfg.PolicyFile != "" {
		syncCtx, syncCancel := context.WithTimeout(context.Background(), 30*time.Second)
		n, err := policy.SyncYAMLFile(syncCtx, cfg.PolicyFile, ruleStore, tenantStore)
		syncCancel()
		if err != nil {
			log.Fatalf("policy: load %s: %v", cfg.PolicyFile, err)
		}
		log.Printf("policy: loaded %d rule(s) from %s", n, cfg.PolicyFile)
	}
	core := policy.NewChainEngine( /* evaluators added in step 5+ */
		cooldown.New(),
		license.New(),
	)
	if err := core.PullFromStore(context.Background(), ruleStore); err != nil {
		log.Fatalf("policy: initial rule pull: %v", err)
	}
	refreshCtx, refreshCancel := context.WithCancel(context.Background())
	defer refreshCancel()
	policy.StartRuleRefresher(refreshCtx, core, ruleStore, 30*time.Second)

	engine := audit.WrapEngine(core, auditLogger, tenantStore)

	// JIT upstream pull-through fetcher. Default ON per
	// plans/upstream-pull-through.md S11; operators opt out per-tenant
	// via the tenant_upstreams table (web console UI lands in PR E) or
	// globally with PKGMIRROR_UPSTREAM_DEFAULT_MODE=off. The fetcher is
	// nil-safe: per-format handlers (PR F+) treat a nil Upstream as
	// "pull-through disabled" and behave exactly as they did before.
	upstreamCfg := upstream.LoadConfigFromEnv()
	upstreamFetcher := upstream.New(upstreamCfg, upstreamstore.New(dbConn))
	log.Printf("upstream: default_mode=%s allowlist_extra=%v allow_private_ips=%v allow_plaintext=%v fetch_timeout=%s max_bytes=%d rpm_per_tenant=%d metadata_cache_max_bytes=%d",
		upstreamCfg.DefaultMode, upstreamCfg.AllowedHostsExtra, upstreamCfg.AllowPrivateIPs, upstreamCfg.AllowPlaintext,
		upstreamCfg.FetchTimeout, upstreamCfg.MaxBytesPerFetch, upstreamCfg.FetchRPMPerTenant, upstreamCfg.MetadataCacheMaxBytes)

	r, err := server.New(server.Deps{
		Service:       svc,
		Models:        pkgModels,
		Tenants:       tenantStore,
		Authenticator: authn,
		Engine:        engine,
		Rules:         ruleStore,
		Audit:         auditLogger,
		Templates:     assets.Templates(),
		Upstream:      upstreamFetcher,
	})
	if err != nil {
		log.Fatalf("build server: %v", err)
	}

	// Wire the web console (PR 1 ships only the foundation: /console/_ping
	// + the layout + static assets + the security/session/CSRF middleware
	// chain). Disabled with PKGMIRROR_CONSOLE_ENABLED=false.
	consoleCfg, err := console.LoadConfigFromEnv()
	if err != nil {
		log.Fatalf("load console config: %v", err)
	}
	consoleCfg.UsingTLS = cfg.TLSEnabled()
	if consoleCfg.Enabled {
		devMode := gin.Mode() != gin.ReleaseMode || consoleCfg.DevDir != ""
		generated, err := consoleCfg.EnsureKeys(devMode, secureRandom)
		if err != nil {
			log.Fatalf("console keys: %v", err)
		}
		if generated {
			log.Printf("console: generated ephemeral session/CSRF keys (dev-mode or PKGMIRROR_CONSOLE_ALLOW_EPHEMERAL_KEYS=true). Sessions invalidate on restart.")
		}
		// PR 2b: select the console authenticator from config. Today
		// only password mode is implemented; proxy-header lands in PR 2c.
		// Call Validate up front so consoleCfg.AuthMode picks up its
		// default ("password") before the log line below + the
		// authenticator switch below.
		if err := consoleCfg.Validate(); err != nil {
			log.Fatalf("console config: %v", err)
		}
		var consoleAuthn auth.Authenticator
		switch consoleCfg.AuthMode {
		case "password":
			consoleAuthn = consolemw.NewSessionAuthenticator(userStore, tenantStore)
		case "proxy-header":
			consoleAuthn = consolemw.NewProxyHeaderAuthenticator(consolemw.ProxyHeaderOptions{
				Users:          userStore,
				Tenants:        tenantStore,
				UserHeader:     consoleCfg.UserHeader,
				EmailHeader:    consoleCfg.EmailHeader,
				GroupsHeader:   consoleCfg.GroupsHeader,
				TrustedProxies: consoleCfg.TrustedProxies,
				BootstrapAdmin: consoleCfg.BootstrapAdmin,
			})
			if consoleCfg.BootstrapAdmin != "" {
				log.Printf("console: bootstrap admin watch active for %q (convergent; safe to leave set after promotion)", consoleCfg.BootstrapAdmin)
			}
		}
		c, err := console.New(console.Deps{
			Config:        consoleCfg,
			Users:         userStore,
			Tenants:       tenantStore,
			Models:        pkgModels,
			Tokens:        tokenStore,
			Audit:         auditLogger,
			Rules:         ruleStore,
			Authenticator: consoleAuthn,
			AppVersion:    "dev",
		})
		if err != nil {
			log.Fatalf("console: %v", err)
		}
		if err := c.Register(r); err != nil {
			log.Fatalf("console register: %v", err)
		}
		log.Printf("console: mounted at /console (auth_mode=%s)", consoleCfg.AuthMode)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Validate TLS config up front so a misconfiguration fails at
	// boot rather than at first request.
	if (cfg.TLSCertFile != "") != (cfg.TLSKeyFile != "") {
		log.Fatalf("PKGMIRROR_TLS_CERT and PKGMIRROR_TLS_KEY must both be set or both empty")
	}

	go func() {
		scheme := "http"
		if cfg.TLSEnabled() {
			scheme = "https"
		}
		log.Printf("pkgmirror listening on %s://%s (db=%s, blobs=%s, default-tenant=%s)",
			scheme, cfg.Addr, cfg.DBPath, cfg.BlobDir, res.DefaultTenant.Name)
		var serveErr error
		if cfg.TLSEnabled() {
			// http.Server.ListenAndServeTLS reads the cert + key
			// on every call (not once at boot), so a cert rotated
			// in-place is picked up on the next graceful restart.
			// For hot-reloads we'd need a getCertificate callback;
			// not needed for the typical reverse-proxy or
			// short-lived-pod deployment.
			serveErr = httpSrv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			serveErr = httpSrv.ListenAndServe()
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Fatalf("server error: %v", serveErr)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown: %v", err)
	}
	log.Printf("pkgmirror stopped")
}

// secureRandom is the random source passed to console.Config.EnsureKeys.
// crypto/rand is the only acceptable source for session/CSRF keys; we
// expose it as a function so EnsureKeys stays test-friendly.
func secureRandom(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}
