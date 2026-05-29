package middleware

import (
	"encoding/gob"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// One-shot post-mint token reveal. The token plaintext is stashed in
// the session (encrypted via gorilla/sessions CookieStore) and consumed
// exactly once on the next render, then cleared. Keeps the value off
// any URL or visible-history surface.

const (
	sessionKeyOneShotToken     = "one_shot_token"
	sessionKeyOneShotTokenName = "one_shot_token_name"
)

func init() {
	gob.Register(oneShotTokenBlob{})
}

type oneShotTokenBlob struct {
	Plain string
	Name  string
}

// SetOneShotToken stashes the just-minted plaintext for one render.
func SetOneShotToken(c *gin.Context, plaintext, name string) error {
	sess := sessions.Default(c)
	sess.Set(sessionKeyOneShotToken, oneShotTokenBlob{Plain: plaintext, Name: name})
	return sess.Save()
}

// PopOneShotToken returns and clears the stashed plaintext (or "" if
// none). Caller usually pairs it with PopOneShotTokenName.
func PopOneShotToken(c *gin.Context) string {
	sess := sessions.Default(c)
	raw := sess.Get(sessionKeyOneShotToken)
	if raw == nil {
		return ""
	}
	b, ok := raw.(oneShotTokenBlob)
	if !ok {
		sess.Delete(sessionKeyOneShotToken)
		_ = sess.Save()
		return ""
	}
	sess.Delete(sessionKeyOneShotToken)
	_ = sess.Save()
	// Stash the name in a separate key so it survives the second pop
	// from PopOneShotTokenName when the template asks for both.
	sess.Set(sessionKeyOneShotTokenName, b.Name)
	_ = sess.Save()
	return b.Plain
}

// PopOneShotTokenName returns and clears the just-minted token's name.
// Must be called after PopOneShotToken.
func PopOneShotTokenName(c *gin.Context) string {
	sess := sessions.Default(c)
	raw := sess.Get(sessionKeyOneShotTokenName)
	if raw == nil {
		return ""
	}
	s, _ := raw.(string)
	sess.Delete(sessionKeyOneShotTokenName)
	_ = sess.Save()
	return s
}
