// Copyright 2020 Paul Greenberg greenpau@outlook.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package jwt

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/caddyauth"
	"go.uber.org/zap"

	"time"
)

// Plugin Errors
const (
	ErrProvisonFailed strError = "authorization provider provisioning error"
)

// ProviderPool is the global authorization provider pool.
// It provides access to all instances of JWT plugin.
var ProviderPool *AuthProviderPool

func init() {
	ProviderPool = &AuthProviderPool{}
	caddy.RegisterModule(AuthProvider{})
}

// AuthProvider authorizes access to endpoints based on
// the presense and content of JWT token.
type AuthProvider struct {
	Name                       string                 `json:"-"`
	Provisioned                bool                   `json:"-"`
	ProvisionFailed            bool                   `json:"-"`
	Context                    string                 `json:"context,omitempty"`
	PrimaryInstance            bool                   `json:"primary,omitempty"`
	AuthURLPath                string                 `json:"auth_url_path,omitempty"`
	AuthRedirectQueryDisabled  bool                   `json:"disable_auth_redirect_query,omitempty"`
	AuthRedirectQueryParameter string                 `json:"auth_redirect_query_param,omitempty"`
	AccessList                 []*AccessListEntry     `json:"access_list,omitempty"`
	TrustedTokens              []*CommonTokenConfig   `json:"trusted_tokens,omitempty"`
	TokenValidator             *TokenValidator        `json:"-"`
	TokenValidatorOptions      *TokenValidatorOptions `json:"token_validate_options,omitempty"`
	AllowedTokenTypes          []string               `json:"token_types,omitempty"`
	AllowedTokenSources        []string               `json:"token_sources,omitempty"`
	PassClaims                 bool                   `json:"pass_claims,omitempty"`
	StripToken                 bool                   `json:"strip_token,omitempty"`
	ForbiddenURL               string                 `json:"forbidden_url,omitempty"`

	ValidateMethodPath          bool `json:"validate_method_path,omitempty"`
	ValidateAccessListPathClaim bool `json:"validate_acl_path_claim,omitempty"`

	PassClaimsWithHeaders bool `json:"pass_claims_with_headers,omitempty"`

	logger    *zap.Logger
	startedAt time.Time
}

// CaddyModule returns the Caddy module information.
func (AuthProvider) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.authentication.providers.jwt",
		New: func() caddy.Module { return new(AuthProvider) },
	}
}

// Provision provisions JWT authorization provider
func (m *AuthProvider) Provision(ctx caddy.Context) error {
	m.logger = ctx.Logger(m)
	m.startedAt = time.Now().UTC()
	if err := ProviderPool.Register(m); err != nil {
		return fmt.Errorf(
			"authentication provider registration error, instance %s, error: %s",
			m.Name, err,
		)
	}
	if m.PrimaryInstance {
		m.logger.Info(
			"provisioned plugin instance",
			zap.String("instance_name", m.Name),
			zap.Time("started_at", m.startedAt),
		)
	}
	return nil
}

// Validate implements caddy.Validator.
func (m *AuthProvider) Validate() error {
	m.logger.Info(
		"validated plugin instance",
		zap.String("instance_name", m.Name),
	)
	return nil
}

// Authenticate authorizes access based on the presense and content of JWT token.
func (m AuthProvider) Authenticate(w http.ResponseWriter, r *http.Request) (caddyauth.User, bool, error) {
	if m.ProvisionFailed {
		w.WriteHeader(500)
		w.Write([]byte(`Internal Server Error`))
		return caddyauth.User{}, false, ErrProvisonFailed
	}

	if !m.Provisioned {
		provisionedInstance, err := ProviderPool.Provision(m.Name)
		if err != nil {
			m.logger.Error(
				"authorization provider provisioning error",
				zap.String("instance_name", m.Name),
				zap.String("error", err.Error()),
			)
			w.WriteHeader(500)
			w.Write([]byte(`Internal Server Error`))
			return caddyauth.User{}, false, err
		}
		m = *provisionedInstance
	}

	var opts *TokenValidatorOptions
	if m.ValidateMethodPath {
		opts = m.TokenValidatorOptions.Clone()
		opts.Metadata["method"] = r.Method
		opts.Metadata["path"] = r.URL.Path
	} else {
		opts = m.TokenValidatorOptions
	}

	userClaims, validUser, err := m.TokenValidator.Authorize(r, opts)
	if err != nil {
		m.logger.Debug(
			"token validation error",
			zap.String("error", err.Error()),
		)
		if strings.Contains(err.Error(), "user role is valid, but not allowed by") {
			if m.ForbiddenURL != "" {
				w.Header().Set("Location", m.ForbiddenURL)
				w.WriteHeader(303)
			} else {
				w.WriteHeader(403)
			}
			w.Write([]byte(`Forbidden`))
			return caddyauth.User{}, false, err
		}
		for k := range m.TokenValidator.Cookies {
			w.Header().Add("Set-Cookie", k+"=delete; path=/; expires=Thu, 01 Jan 1970 00:00:00 GMT")
		}
		addRedirectLocationHeader(w, r, m.AuthURLPath, m.AuthRedirectQueryDisabled, m.AuthRedirectQueryParameter)
		w.WriteHeader(302)
		w.Write([]byte(`Unauthorized`))
		return caddyauth.User{}, false, err
	}
	if !validUser {
		m.logger.Debug(
			"token validation error",
			zap.String("error", "user invalid"),
		)
		for k := range m.TokenValidator.Cookies {
			w.Header().Add("Set-Cookie", k+"=delete; path=/; expires=Thu, 01 Jan 1970 00:00:00 GMT")
		}
		addRedirectLocationHeader(w, r, m.AuthURLPath, m.AuthRedirectQueryDisabled, m.AuthRedirectQueryParameter)
		w.WriteHeader(302)
		w.Write([]byte(`Unauthorized User`))
		return caddyauth.User{}, false, nil
	}

	if userClaims == nil {
		m.logger.Debug(
			"token validation error",
			zap.String("error", "nil claims"),
		)
		for k := range m.TokenValidator.Cookies {
			w.Header().Add("Set-Cookie", k+"=delete; path=/; expires=Thu, 01 Jan 1970 00:00:00 GMT")
		}
		addRedirectLocationHeader(w, r, m.AuthURLPath, m.AuthRedirectQueryDisabled, m.AuthRedirectQueryParameter)
		w.WriteHeader(302)
		w.Write([]byte(`User Unauthorized`))
		return caddyauth.User{}, false, nil
	}

	userIdentity := caddyauth.User{
		ID: userClaims.Email,
		Metadata: map[string]string{
			"roles": strings.Join(userClaims.Roles, " "),
		},
	}

	if userClaims.Name != "" {
		userIdentity.Metadata["name"] = userClaims.Name
		if m.PassClaimsWithHeaders {
			r.Header.Set("X-Token-User-Name", userClaims.Name)
		}
	}

	if userClaims.Email != "" {
		userIdentity.Metadata["email"] = userClaims.Email
		if m.PassClaimsWithHeaders {
			r.Header.Set("X-Token-User-Email", userClaims.Email)
		}
	}

	if m.PassClaimsWithHeaders {
		if len(userClaims.Roles) > 0 {
			r.Header.Set("X-Token-User-Roles", strings.Join(userClaims.Roles, " "))
		}
		if userClaims.Subject != "" {
			r.Header.Set("X-Token-Subject", userClaims.Subject)
		}
	}

	return userIdentity, true, nil
}

// Interface guards
var (
	_ caddy.Provisioner       = (*AuthProvider)(nil)
	_ caddy.Validator         = (*AuthProvider)(nil)
	_ caddyauth.Authenticator = (*AuthProvider)(nil)
)
