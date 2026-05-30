package console_test

// Helpers that need to import gin (to register handlers on the
// fixture's RouterGroup). Kept separate from middleware_chain_test.go
// so the test file proper can focus on assertions.

import (
	"errors"

	"github.com/gin-gonic/gin"
)

// registerTestRoutes mounts two helper routes on the fixture's authed
// group: /console/_test_render_error and /console/_test_panic. Both
// inherit the full middleware chain (Recover, RequestID, AccessLog,
// SecurityHeaders, WithGinContext, Session, CSRF, Auth, RequireAuth).
//
// Each registration is guarded so calling registerTestRoutes more
// than once on the same fixture is a no-op (the second call would
// panic on duplicate route otherwise).
func registerTestRoutes(f *authFixture) {
	if f.testRoutesRegistered {
		return
	}
	f.testRoutesRegistered = true

	g := f.Console.AuthedGroup()
	c := f.Console

	g.GET("/_test_render_error", func(gc *gin.Context) {
		c.RenderError(gc, "test verb", errors.New("simulated failure for TestChain_RequestIDFlowsToErrorPage"))
	})

	g.GET("/_test_panic", func(gc *gin.Context) {
		panic("deliberate test panic for TestChain_PanicInHandlerProducesFriendly500")
	})
}
