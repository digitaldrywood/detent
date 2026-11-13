package hubserver

import (
	"context"
	"github.com/digitaldrywood/detent/internal/hubserver/sprites"
	"github.com/digitaldrywood/detent/internal/tracker"
	"net/http"
)

var errSpriteNameTaken = sprites.ErrNameTaken
var errSpriteNamesExhausted = sprites.ErrNamesExhausted
var errSpritesValidation = sprites.ErrValidation
var errSpritesTokenRejected = sprites.ErrTokenRejected
var errSpritesBilling = sprites.ErrBilling

const spriteSandboxUnavailableReason = sprites.SandboxUnavailableReason

type spritePoolSettings = sprites.PoolSettings
type spriteScaleInput = sprites.ScaleInput
type spriteScaleDecision = sprites.ScaleDecision

func decideSpriteScale(input spriteScaleInput) spriteScaleDecision { return sprites.DecideScale(input) }
func validateSpritePoolAccess(tier *string) error {
	return fromFeatureError(sprites.ValidatePoolAccess(tier))
}
func validateSpritePoolSettings(settings *spritePoolSettings) error {
	return fromFeatureError(sprites.ValidatePoolSettings(settings))
}
func spritesResponseError(status int) error { return sprites.ResponseError(status) }
func validSpritesSlug(slug string) bool     { return sprites.ValidSlug(slug) }
func validateSpritesToken(ctx context.Context, client *http.Client, token []byte) (string, error) {
	return sprites.ValidateToken(ctx, client, token)
}
func spriteShellQuote(value string) string { return sprites.ShellQuote(value) }
func spritePoolBootstrapScript(org tracker.OrganizationID, hubURL, setup, command, version string) string {
	return sprites.BootstrapScript(org, hubURL, setup, command, version)
}
func spriteBootstrapDiagnostic(line string) string { return sprites.BootstrapDiagnostic(line) }

type spriteNameGenerator struct {
	intN      func(int) int
	generator *sprites.NameGenerator
}

func (g *spriteNameGenerator) next(taken func(string) (bool, error)) (string, error) {
	if g.generator == nil {
		g.generator = sprites.NewNameGenerator(g.intN)
	}
	return g.generator.Next(taken)
}
