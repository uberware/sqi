// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// GET /auth/providers — what the login page needs to render itself.
//
// Public by necessity: the login page is unauthenticated by definition, so an
// authenticated discovery endpoint would be circular. What it exposes is that
// SSO is configured and what its button says — both already visible to anyone
// who loads the page. It exposes neither the issuer nor the client secret.
//
// This endpoint exists because /auth/me distinguishes only anonymous from
// authenticated; before C2 the web had no channel at all for asking what login
// methods a deployment offers.

import "net/http"

// defaultSSOButtonLabel is what the button says when the operator configured
// no button_label. A blank label would render an unlabeled, unclickable-looking
// control, so SSO would appear broken rather than merely unstyled.
const defaultSSOButtonLabel = "Sign in with SSO"

type ssoProvider struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	LoginURL string `json:"login_url"`
}

type authProvidersResponse struct {
	// Password reports whether username/password login is offered. Always true
	// today; present so the web never has to infer it, and so a future
	// SSO-only deployment is a config change rather than a web change.
	Password bool `json:"password"`
	// SSO is never nil: an empty JSON array is what the web iterates over, and
	// a null would force every consumer to guard for it.
	SSO []ssoProvider `json:"sso"`
}

func (h *authHandler) authProviders(w http.ResponseWriter, _ *http.Request) {
	out := authProvidersResponse{Password: true, SSO: []ssoProvider{}}
	if h.oidcProvider != nil {
		label := h.oidcCfg.ButtonLabel
		if label == "" {
			label = defaultSSOButtonLabel
		}
		out.SSO = append(out.SSO, ssoProvider{
			ID:       "oidc",
			Label:    label,
			LoginURL: "/api/v1/auth/oidc/login",
		})
	}
	writeJSON(w, http.StatusOK, out)
}
