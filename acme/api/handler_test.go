package api

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-chi/chi"
	"github.com/pkg/errors"
	"github.com/smallstep/assert"
	"github.com/smallstep/certificates/acme"
	"go.step.sm/crypto/jose"
	"go.step.sm/crypto/pemutil"
)

func TestHandler_GetNonce(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"GET", 204},
		{"HEAD", 200},
	}

	// Request with chi context
	req := httptest.NewRequest("GET", "http://ca.smallstep.com/nonce", nil)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handler{}
			w := httptest.NewRecorder()
			req.Method = tt.name
			h.GetNonce(w, req)
			res := w.Result()

			if res.StatusCode != tt.statusCode {
				t.Errorf("Handler.GetNonce StatusCode = %d, wants %d", res.StatusCode, tt.statusCode)
			}
		})
	}
}

func TestHandler_GetDirectory(t *testing.T) {
	linker := NewLinker("ca.smallstep.com", "acme")

	prov := newProv()
	provName := url.PathEscape(prov.GetName())
	baseURL := &url.URL{Scheme: "https", Host: "test.ca.smallstep.com"}
	ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
	ctx = context.WithValue(ctx, baseURLContextKey, baseURL)

	expDir := Directory{
		NewNonce:   fmt.Sprintf("%s/acme/%s/new-nonce", baseURL.String(), provName),
		NewAccount: fmt.Sprintf("%s/acme/%s/new-account", baseURL.String(), provName),
		NewOrder:   fmt.Sprintf("%s/acme/%s/new-order", baseURL.String(), provName),
		RevokeCert: fmt.Sprintf("%s/acme/%s/revoke-cert", baseURL.String(), provName),
		KeyChange:  fmt.Sprintf("%s/acme/%s/key-change", baseURL.String(), provName),
	}

	type test struct {
		statusCode int
		err        *acme.Error
	}
	var tests = map[string]func(t *testing.T) test{
		"ok": func(t *testing.T) test {
			return test{
				statusCode: 200,
			}
		},
	}
	for name, run := range tests {
		tc := run(t)
		t.Run(name, func(t *testing.T) {
			h := &Handler{linker: linker}
			req := httptest.NewRequest("GET", "/foo/bar", nil)
			req = req.WithContext(ctx)
			w := httptest.NewRecorder()
			h.GetDirectory(w, req)
			res := w.Result()

			assert.Equals(t, res.StatusCode, tc.statusCode)

			body, err := ioutil.ReadAll(res.Body)
			res.Body.Close()
			assert.FatalError(t, err)

			if res.StatusCode >= 400 && assert.NotNil(t, tc.err) {
				var ae acme.Error
				assert.FatalError(t, json.Unmarshal(bytes.TrimSpace(body), &ae))

				assert.Equals(t, ae.Type, tc.err.Type)
				assert.Equals(t, ae.Detail, tc.err.Detail)
				assert.Equals(t, ae.Identifier, tc.err.Identifier)
				assert.Equals(t, ae.Subproblems, tc.err.Subproblems)
				assert.Equals(t, res.Header["Content-Type"], []string{"application/problem+json"})
			} else {
				var dir Directory
				json.Unmarshal(bytes.TrimSpace(body), &dir)
				assert.Equals(t, dir, expDir)
				assert.Equals(t, res.Header["Content-Type"], []string{"application/json"})
			}
		})
	}
}

func TestHandler_GetAuthorization(t *testing.T) {
	expiry := time.Now().UTC().Add(6 * time.Hour)
	az := acme.Authorization{
		ID:        "authzID",
		AccountID: "accID",
		Identifier: acme.Identifier{
			Type:  "dns",
			Value: "example.com",
		},
		Status:    "pending",
		ExpiresAt: expiry,
		Wildcard:  false,
		Challenges: []*acme.Challenge{
			{
				Type:    "http-01",
				Status:  "pending",
				Token:   "tok2",
				URL:     "https://ca.smallstep.com/acme/challenge/chHTTPID",
				ID:      "chHTTP01ID",
				AuthzID: "authzID",
			},
			{
				Type:    "dns-01",
				Status:  "pending",
				Token:   "tok2",
				URL:     "https://ca.smallstep.com/acme/challenge/chDNSID",
				ID:      "chDNSID",
				AuthzID: "authzID",
			},
		},
	}
	prov := newProv()
	provName := url.PathEscape(prov.GetName())
	baseURL := &url.URL{Scheme: "https", Host: "test.ca.smallstep.com"}

	// Request with chi context
	chiCtx := chi.NewRouteContext()
	chiCtx.URLParams.Add("authzID", az.ID)
	url := fmt.Sprintf("%s/acme/%s/authz/%s",
		baseURL.String(), provName, az.ID)

	type test struct {
		db         acme.DB
		ctx        context.Context
		statusCode int
		err        *acme.Error
	}
	var tests = map[string]func(t *testing.T) test{
		"fail/no-account": func(t *testing.T) test {
			return test{
				db:         &acme.MockDB{},
				ctx:        context.Background(),
				statusCode: 400,
				err:        acme.NewError(acme.ErrorAccountDoesNotExistType, "account does not exist"),
			}
		},
		"fail/nil-account": func(t *testing.T) test {
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, nil)
			return test{
				db:         &acme.MockDB{},
				ctx:        ctx,
				statusCode: 400,
				err:        acme.NewError(acme.ErrorAccountDoesNotExistType, "account does not exist"),
			}
		},
		"fail/db.GetAuthorization-error": func(t *testing.T) test {
			acc := &acme.Account{ID: "accID"}
			ctx := context.WithValue(context.Background(), accContextKey, acc)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			return test{
				db: &acme.MockDB{
					MockError: acme.NewErrorISE("force"),
				},
				ctx:        ctx,
				statusCode: 500,
				err:        acme.NewErrorISE("force"),
			}
		},
		"fail/account-id-mismatch": func(t *testing.T) test {
			acc := &acme.Account{ID: "accID"}
			ctx := context.WithValue(context.Background(), accContextKey, acc)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			return test{
				db: &acme.MockDB{
					MockGetAuthorization: func(ctx context.Context, id string) (*acme.Authorization, error) {
						assert.Equals(t, id, az.ID)
						return &acme.Authorization{
							AccountID: "foo",
						}, nil
					},
				},
				ctx:        ctx,
				statusCode: 401,
				err:        acme.NewError(acme.ErrorUnauthorizedType, "account id mismatch"),
			}
		},
		"fail/db.UpdateAuthorization-error": func(t *testing.T) test {
			acc := &acme.Account{ID: "accID"}
			ctx := context.WithValue(context.Background(), accContextKey, acc)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			return test{
				db: &acme.MockDB{
					MockGetAuthorization: func(ctx context.Context, id string) (*acme.Authorization, error) {
						assert.Equals(t, id, az.ID)
						return &acme.Authorization{
							AccountID: "accID",
							Status:    acme.StatusPending,
							ExpiresAt: time.Now().Add(-1 * time.Hour),
						}, nil
					},
					MockUpdateAuthorization: func(ctx context.Context, az *acme.Authorization) error {
						assert.Equals(t, az.Status, acme.StatusInvalid)
						return acme.NewErrorISE("force")
					},
				},
				ctx:        ctx,
				statusCode: 500,
				err:        acme.NewErrorISE("force"),
			}
		},
		"ok": func(t *testing.T) test {
			acc := &acme.Account{ID: "accID"}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			ctx = context.WithValue(ctx, baseURLContextKey, baseURL)
			return test{
				db: &acme.MockDB{
					MockGetAuthorization: func(ctx context.Context, id string) (*acme.Authorization, error) {
						assert.Equals(t, id, az.ID)
						return &az, nil
					},
				},
				ctx:        ctx,
				statusCode: 200,
			}
		},
	}
	for name, run := range tests {
		tc := run(t)
		t.Run(name, func(t *testing.T) {
			h := &Handler{db: tc.db, linker: NewLinker("dns", "acme")}
			req := httptest.NewRequest("GET", "/foo/bar", nil)
			req = req.WithContext(tc.ctx)
			w := httptest.NewRecorder()
			h.GetAuthorization(w, req)
			res := w.Result()

			assert.Equals(t, res.StatusCode, tc.statusCode)

			body, err := ioutil.ReadAll(res.Body)
			res.Body.Close()
			assert.FatalError(t, err)

			if res.StatusCode >= 400 && assert.NotNil(t, tc.err) {
				var ae acme.Error
				assert.FatalError(t, json.Unmarshal(bytes.TrimSpace(body), &ae))

				assert.Equals(t, ae.Type, tc.err.Type)
				assert.Equals(t, ae.Detail, tc.err.Detail)
				assert.Equals(t, ae.Identifier, tc.err.Identifier)
				assert.Equals(t, ae.Subproblems, tc.err.Subproblems)
				assert.Equals(t, res.Header["Content-Type"], []string{"application/problem+json"})
			} else {
				//var gotAz acme.Authz
				//assert.FatalError(t, json.Unmarshal(bytes.TrimSpace(body), &gotAz))
				expB, err := json.Marshal(az)
				assert.FatalError(t, err)
				assert.Equals(t, bytes.TrimSpace(body), expB)
				assert.Equals(t, res.Header["Location"], []string{url})
				assert.Equals(t, res.Header["Content-Type"], []string{"application/json"})
			}
		})
	}
}

func TestHandler_GetCertificate(t *testing.T) {
	leaf, err := pemutil.ReadCertificate("../../authority/testdata/certs/foo.crt")
	assert.FatalError(t, err)
	inter, err := pemutil.ReadCertificate("../../authority/testdata/certs/intermediate_ca.crt")
	assert.FatalError(t, err)
	root, err := pemutil.ReadCertificate("../../authority/testdata/certs/root_ca.crt")
	assert.FatalError(t, err)

	certBytes := append(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: leaf.Raw,
	}), pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: inter.Raw,
	})...)
	certBytes = append(certBytes, pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: root.Raw,
	})...)
	certID := "certID"

	prov := newProv()
	provName := url.PathEscape(prov.GetName())
	baseURL := &url.URL{Scheme: "https", Host: "test.ca.smallstep.com"}
	// Request with chi context
	chiCtx := chi.NewRouteContext()
	chiCtx.URLParams.Add("certID", certID)
	url := fmt.Sprintf("%s/acme/%s/certificate/%s",
		baseURL.String(), provName, certID)

	type test struct {
		db         acme.DB
		ctx        context.Context
		statusCode int
		err        *acme.Error
	}
	var tests = map[string]func(t *testing.T) test{
		"fail/no-account": func(t *testing.T) test {
			return test{
				db:         &acme.MockDB{},
				ctx:        context.Background(),
				statusCode: 400,
				err:        acme.NewError(acme.ErrorAccountDoesNotExistType, "account does not exist"),
			}
		},
		"fail/nil-account": func(t *testing.T) test {
			ctx := context.WithValue(context.Background(), accContextKey, nil)
			return test{
				db:         &acme.MockDB{},
				ctx:        ctx,
				statusCode: 400,
				err:        acme.NewError(acme.ErrorAccountDoesNotExistType, "account does not exist"),
			}
		},
		"fail/db.GetCertificate-error": func(t *testing.T) test {
			acc := &acme.Account{ID: "accID"}
			ctx := context.WithValue(context.Background(), accContextKey, acc)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			return test{
				db: &acme.MockDB{
					MockError: acme.NewErrorISE("force"),
				},
				ctx:        ctx,
				statusCode: 500,
				err:        acme.NewErrorISE("force"),
			}
		},
		"fail/account-id-mismatch": func(t *testing.T) test {
			acc := &acme.Account{ID: "accID"}
			ctx := context.WithValue(context.Background(), accContextKey, acc)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			return test{
				db: &acme.MockDB{
					MockGetCertificate: func(ctx context.Context, id string) (*acme.Certificate, error) {
						assert.Equals(t, id, certID)
						return &acme.Certificate{AccountID: "foo"}, nil
					},
				},
				ctx:        ctx,
				statusCode: 401,
				err:        acme.NewError(acme.ErrorUnauthorizedType, "account id mismatch"),
			}
		},
		"ok": func(t *testing.T) test {
			acc := &acme.Account{ID: "accID"}
			ctx := context.WithValue(context.Background(), accContextKey, acc)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			return test{
				db: &acme.MockDB{
					MockGetCertificate: func(ctx context.Context, id string) (*acme.Certificate, error) {
						assert.Equals(t, id, certID)
						return &acme.Certificate{
							AccountID:     "accID",
							OrderID:       "ordID",
							Leaf:          leaf,
							Intermediates: []*x509.Certificate{inter, root},
							ID:            id,
						}, nil
					},
				},
				ctx:        ctx,
				statusCode: 200,
			}
		},
	}
	for name, run := range tests {
		tc := run(t)
		t.Run(name, func(t *testing.T) {
			h := &Handler{db: tc.db}
			req := httptest.NewRequest("GET", url, nil)
			req = req.WithContext(tc.ctx)
			w := httptest.NewRecorder()
			h.GetCertificate(w, req)
			res := w.Result()

			assert.Equals(t, res.StatusCode, tc.statusCode)

			body, err := ioutil.ReadAll(res.Body)
			res.Body.Close()
			assert.FatalError(t, err)

			if res.StatusCode >= 400 && assert.NotNil(t, tc.err) {
				var ae acme.Error
				assert.FatalError(t, json.Unmarshal(bytes.TrimSpace(body), &ae))

				assert.Equals(t, ae.Type, tc.err.Type)
				assert.HasPrefix(t, ae.Detail, tc.err.Detail)
				assert.Equals(t, ae.Identifier, tc.err.Identifier)
				assert.Equals(t, ae.Subproblems, tc.err.Subproblems)
				assert.Equals(t, res.Header["Content-Type"], []string{"application/problem+json"})
			} else {
				assert.Equals(t, bytes.TrimSpace(body), bytes.TrimSpace(certBytes))
				assert.Equals(t, res.Header["Content-Type"], []string{"application/pem-certificate-chain; charset=utf-8"})
			}
		})
	}
}

func ch() acme.Challenge {
	return acme.Challenge{
		Type:    "http-01",
		Status:  "pending",
		Token:   "tok2",
		URL:     "https://ca.smallstep.com/acme/challenge/chID",
		ID:      "chID",
		AuthzID: "authzID",
	}
}

func TestHandler_GetChallenge(t *testing.T) {
	chiCtx := chi.NewRouteContext()
	chiCtx.URLParams.Add("chID", "chID")
	chiCtx.URLParams.Add("authzID", "authzID")
	prov := newProv()
	provName := url.PathEscape(prov.GetName())

	baseURL := &url.URL{Scheme: "https", Host: "test.ca.smallstep.com"}

	url := fmt.Sprintf("%s/acme/%s/challenge/%s/%s",
		baseURL.String(), provName, "authzID", "chID")

	type test struct {
		db         acme.DB
		vco        *acme.ValidateChallengeOptions
		ctx        context.Context
		statusCode int
		ch         *acme.Challenge
		err        *acme.Error
	}
	var tests = map[string]func(t *testing.T) test{
		"fail/no-account": func(t *testing.T) test {
			return test{
				ctx:        context.Background(),
				statusCode: 400,
				err:        acme.NewError(acme.ErrorAccountDoesNotExistType, "account does not exist"),
			}
		},
		"fail/nil-account": func(t *testing.T) test {
			return test{
				ctx:        context.WithValue(context.Background(), accContextKey, nil),
				statusCode: 400,
				err:        acme.NewError(acme.ErrorAccountDoesNotExistType, "account does not exist"),
			}
		},
		"fail/no-payload": func(t *testing.T) test {
			acc := &acme.Account{ID: "accID"}
			ctx := context.WithValue(context.Background(), accContextKey, acc)
			return test{
				ctx:        ctx,
				statusCode: 400,
				err:        acme.NewError(acme.ErrorMalformedType, "payload expected in request context"),
			}
		},
		"fail/nil-payload": func(t *testing.T) test {
			acc := &acme.Account{ID: "accID"}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, nil)
			return test{
				ctx:        ctx,
				statusCode: 400,
				err:        acme.NewError(acme.ErrorMalformedType, "payload expected in request context"),
			}
		},
		"fail/db.GetChallenge-error": func(t *testing.T) test {
			acc := &acme.Account{ID: "accID"}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{isEmptyJSON: true})
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			return test{
				db: &acme.MockDB{
					MockGetChallenge: func(ctx context.Context, chID, azID string) (*acme.Challenge, error) {
						assert.Equals(t, chID, "chID")
						assert.Equals(t, azID, "authzID")
						return nil, acme.NewErrorISE("force")
					},
				},
				ctx:        ctx,
				statusCode: 500,
				err:        acme.NewErrorISE("force"),
			}
		},
		"fail/account-id-mismatch": func(t *testing.T) test {
			acc := &acme.Account{ID: "accID"}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{isEmptyJSON: true})
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			return test{
				db: &acme.MockDB{
					MockGetChallenge: func(ctx context.Context, chID, azID string) (*acme.Challenge, error) {
						assert.Equals(t, chID, "chID")
						assert.Equals(t, azID, "authzID")
						return &acme.Challenge{AccountID: "foo"}, nil
					},
				},
				ctx:        ctx,
				statusCode: 401,
				err:        acme.NewError(acme.ErrorUnauthorizedType, "accout id mismatch"),
			}
		},
		"fail/no-jwk": func(t *testing.T) test {
			acc := &acme.Account{ID: "accID"}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{isEmptyJSON: true})
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			return test{
				db: &acme.MockDB{
					MockGetChallenge: func(ctx context.Context, chID, azID string) (*acme.Challenge, error) {
						assert.Equals(t, chID, "chID")
						assert.Equals(t, azID, "authzID")
						return &acme.Challenge{AccountID: "accID"}, nil
					},
				},
				ctx:        ctx,
				statusCode: 500,
				err:        acme.NewErrorISE("missing jwk"),
			}
		},
		"fail/nil-jwk": func(t *testing.T) test {
			acc := &acme.Account{ID: "accID"}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{isEmptyJSON: true})
			ctx = context.WithValue(ctx, jwkContextKey, nil)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			return test{
				db: &acme.MockDB{
					MockGetChallenge: func(ctx context.Context, chID, azID string) (*acme.Challenge, error) {
						assert.Equals(t, chID, "chID")
						assert.Equals(t, azID, "authzID")
						return &acme.Challenge{AccountID: "accID"}, nil
					},
				},
				ctx:        ctx,
				statusCode: 500,
				err:        acme.NewErrorISE("nil jwk"),
			}
		},
		"fail/validate-challenge-error": func(t *testing.T) test {
			acc := &acme.Account{ID: "accID"}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{isEmptyJSON: true})
			_jwk, err := jose.GenerateJWK("EC", "P-256", "ES256", "sig", "", 0)
			assert.FatalError(t, err)
			_pub := _jwk.Public()
			ctx = context.WithValue(ctx, jwkContextKey, &_pub)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			return test{
				db: &acme.MockDB{
					MockGetChallenge: func(ctx context.Context, chID, azID string) (*acme.Challenge, error) {
						assert.Equals(t, chID, "chID")
						assert.Equals(t, azID, "authzID")
						return &acme.Challenge{
							Status:    acme.StatusPending,
							Type:      "http-01",
							AccountID: "accID",
						}, nil
					},
					MockUpdateChallenge: func(ctx context.Context, ch *acme.Challenge) error {
						assert.Equals(t, ch.Status, acme.StatusPending)
						assert.Equals(t, ch.Type, "http-01")
						assert.Equals(t, ch.AccountID, "accID")
						assert.HasSuffix(t, ch.Error.Type, acme.ErrorConnectionType.String())
						return acme.NewErrorISE("force")
					},
				},
				vco: &acme.ValidateChallengeOptions{
					HTTPGet: func(string) (*http.Response, error) {
						return nil, errors.New("force")
					},
				},
				ctx:        ctx,
				statusCode: 500,
				err:        acme.NewErrorISE("force"),
			}
		},
		"ok": func(t *testing.T) test {
			acc := &acme.Account{ID: "accID"}
			ctx := context.WithValue(context.Background(), provisionerContextKey, prov)
			ctx = context.WithValue(ctx, accContextKey, acc)
			ctx = context.WithValue(ctx, payloadContextKey, &payloadInfo{isEmptyJSON: true})
			_jwk, err := jose.GenerateJWK("EC", "P-256", "ES256", "sig", "", 0)
			assert.FatalError(t, err)
			_pub := _jwk.Public()
			ctx = context.WithValue(ctx, jwkContextKey, &_pub)
			ctx = context.WithValue(ctx, baseURLContextKey, baseURL)
			ctx = context.WithValue(ctx, chi.RouteCtxKey, chiCtx)
			return test{
				db: &acme.MockDB{
					MockGetChallenge: func(ctx context.Context, chID, azID string) (*acme.Challenge, error) {
						assert.Equals(t, chID, "chID")
						assert.Equals(t, azID, "authzID")
						return &acme.Challenge{
							ID:        "chID",
							AuthzID:   "authzID",
							Status:    acme.StatusPending,
							Type:      "http-01",
							AccountID: "accID",
						}, nil
					},
					MockUpdateChallenge: func(ctx context.Context, ch *acme.Challenge) error {
						assert.Equals(t, ch.Status, acme.StatusPending)
						assert.Equals(t, ch.Type, "http-01")
						assert.Equals(t, ch.AccountID, "accID")
						assert.HasSuffix(t, ch.Error.Type, acme.ErrorConnectionType.String())
						return nil
					},
				},
				ch: &acme.Challenge{
					ID:        "chID",
					AuthzID:   "authzID",
					Status:    acme.StatusPending,
					Type:      "http-01",
					AccountID: "accID",
					URL:       url,
					Error:     acme.NewError(acme.ErrorConnectionType, "force"),
				},
				vco: &acme.ValidateChallengeOptions{
					HTTPGet: func(string) (*http.Response, error) {
						return nil, errors.New("force")
					},
				},
				ctx:        ctx,
				statusCode: 200,
			}
		},
	}
	for name, run := range tests {
		tc := run(t)
		t.Run(name, func(t *testing.T) {
			h := &Handler{db: tc.db, linker: NewLinker("dns", "acme"), validateChallengeOptions: tc.vco}
			req := httptest.NewRequest("GET", url, nil)
			req = req.WithContext(tc.ctx)
			w := httptest.NewRecorder()
			h.GetChallenge(w, req)
			res := w.Result()

			assert.Equals(t, res.StatusCode, tc.statusCode)

			body, err := ioutil.ReadAll(res.Body)
			res.Body.Close()
			assert.FatalError(t, err)

			if res.StatusCode >= 400 && assert.NotNil(t, tc.err) {
				var ae acme.Error
				assert.FatalError(t, json.Unmarshal(bytes.TrimSpace(body), &ae))

				assert.Equals(t, ae.Type, tc.err.Type)
				assert.Equals(t, ae.Detail, tc.err.Detail)
				assert.Equals(t, ae.Identifier, tc.err.Identifier)
				assert.Equals(t, ae.Subproblems, tc.err.Subproblems)
				assert.Equals(t, res.Header["Content-Type"], []string{"application/problem+json"})
			} else {
				expB, err := json.Marshal(tc.ch)
				assert.FatalError(t, err)
				assert.Equals(t, bytes.TrimSpace(body), expB)
				assert.Equals(t, res.Header["Link"], []string{fmt.Sprintf("<%s/acme/%s/authz/%s>;rel=\"up\"", baseURL, provName, tc.ch.AuthzID)})
				assert.Equals(t, res.Header["Location"], []string{url})
				assert.Equals(t, res.Header["Content-Type"], []string{"application/json"})
			}
		})
	}
}
