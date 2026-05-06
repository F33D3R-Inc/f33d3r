package observ

import (
	"regexp"
	"strings"
)

// Path normalisation collapses high-cardinality URL paths into a small fixed
// set of templates so Prometheus labels don't explode. T0–T1 strategy: regex
// pattern match. At T2+ we'll switch to Go 1.22 ServeMux pattern templates,
// at which point this file becomes obsolete.
//
// Cardinality budget at T0: ~80 unique (method, path) tuples for Nantar.
// Anything that would push past this should be added below as a normaliser.

var (
	rePostID    = regexp.MustCompile(`^/post/[^/]+$`)
	reUserHandle = regexp.MustCompile(`^/u/[^/]+(/.*)?$`)
	reMediaPath = regexp.MustCompile(`^/media/[^/]+/[^/]+$`)
	reCDN       = regexp.MustCompile(`^/cdn/.+$`)
	reStatic    = regexp.MustCompile(`^/static/.+$`)
	reAPIPost   = regexp.MustCompile(`^/api/post/[^/]+$`)
	reFeedItem  = regexp.MustCompile(`^/feed/item/[^/]+$`)
	reKYC       = regexp.MustCompile(`^/kyc(/.*)?$`)
	reAdmin     = regexp.MustCompile(`^/admin(/.*)?$`)
	reSettings  = regexp.MustCompile(`^/settings(/.*)?$`)
	reShop      = regexp.MustCompile(`^/shop/[^/]+(/.*)?$`)
	reTrack     = regexp.MustCompile(`^/api/track/[^/]+$`)
	reVovin     = regexp.MustCompile(`^/vovin/.+$`)
	reAinSoph   = regexp.MustCompile(`^/ainsoph/.+$`)
	reVerity    = regexp.MustCompile(`^/verity/.+$`)
	reLedger    = regexp.MustCompile(`^/ledger/.+$`)
	reThessalon = regexp.MustCompile(`^/thessalon/.+$`)
	rePoll      = regexp.MustCompile(`^/api/poll/[^/]+$`)
	reUUID      = regexp.MustCompile(`/[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
)

// normalisePath maps a concrete request path to a low-cardinality template
// suitable for Prometheus labels.
func normalisePath(p string) string {
	switch {
	case rePostID.MatchString(p):
		return "/post/:id"
	case reUserHandle.MatchString(p):
		return "/u/:handle"
	case reMediaPath.MatchString(p):
		return "/media/:post_id/:filename"
	case reCDN.MatchString(p):
		return "/cdn/*"
	case reStatic.MatchString(p):
		return "/static/*"
	case reAPIPost.MatchString(p):
		return "/api/post/:action"
	case reFeedItem.MatchString(p):
		return "/feed/item/:action"
	case reShop.MatchString(p):
		return "/shop/:handle"
	case reTrack.MatchString(p):
		return "/api/track/:action"
	case rePoll.MatchString(p):
		return "/api/poll/:action"
	case reKYC.MatchString(p):
		return "/kyc/*"
	case reAdmin.MatchString(p):
		return "/admin/*"
	case reSettings.MatchString(p):
		return "/settings/*"
	case reVovin.MatchString(p):
		return "/vovin/*"
	case reAinSoph.MatchString(p):
		return "/ainsoph/*"
	case reVerity.MatchString(p):
		return "/verity/*"
	case reLedger.MatchString(p):
		return "/ledger/*"
	case reThessalon.MatchString(p):
		return "/thessalon/*"
	}
	// Final safety net: any UUID embedded in an unmatched path becomes :uuid.
	if reUUID.MatchString(p) {
		return reUUID.ReplaceAllString(p, "/:uuid")
	}
	// Strip trailing slash to coalesce / and "" if any.
	return strings.TrimRight(p, "/")
}
