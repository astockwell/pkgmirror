// Command pkgmirror runs the pkgmirror HTTP service.
package main

import (
	"context"
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
	pkgdb "github.com/astockwell/pkgmirror/internal/db"
	"github.com/astockwell/pkgmirror/internal/models"
	pkgsvc "github.com/astockwell/pkgmirror/internal/packages"
	"github.com/astockwell/pkgmirror/internal/policy"
	"github.com/astockwell/pkgmirror/internal/policy/cooldown"
	"github.com/astockwell/pkgmirror/internal/policy/license"
	"github.com/astockwell/pkgmirror/internal/server"
	"github.com/astockwell/pkgmirror/internal/storage"
	"github.com/astockwell/pkgmirror/internal/tenants"
	"github.com/astockwell/pkgmirror/internal/tokens"
	"github.com/astockwell/pkgmirror/internal/users"
)

func main() {
	cfg := config.Load()

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
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

	blobs, err := storage.NewFS(cfg.BlobDir)
	if err != nil {
		log.Fatalf("open blob storage: %v", err)
	}
	svc := pkgsvc.NewService(pkgModels, blobs)

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

	r, err := server.New(server.Deps{
		Service:       svc,
		Models:        pkgModels,
		Tenants:       tenantStore,
		Authenticator: authn,
		Engine:        engine,
		Templates:     assets.Templates(),
	})
	if err != nil {
		log.Fatalf("build server: %v", err)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("pkgmirror listening on %s (db=%s, blobs=%s, default-tenant=%s)",
			cfg.Addr, cfg.DBPath, cfg.BlobDir, res.DefaultTenant.Name)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
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
