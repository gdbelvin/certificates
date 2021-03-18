package nosql

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/smallstep/assert"
	"github.com/smallstep/certificates/acme"
	"github.com/smallstep/certificates/db"
	"github.com/smallstep/nosql"
	nosqldb "github.com/smallstep/nosql/database"

	"go.step.sm/crypto/pemutil"
)

func TestDB_CreateCertificate(t *testing.T) {
	leaf, err := pemutil.ReadCertificate("../../../authority/testdata/certs/foo.crt")
	assert.FatalError(t, err)
	inter, err := pemutil.ReadCertificate("../../../authority/testdata/certs/intermediate_ca.crt")
	assert.FatalError(t, err)
	root, err := pemutil.ReadCertificate("../../../authority/testdata/certs/root_ca.crt")
	assert.FatalError(t, err)
	type test struct {
		db   nosql.DB
		cert *acme.Certificate
		err  error
		_id  *string
	}
	var tests = map[string]func(t *testing.T) test{
		"fail/cmpAndSwap-error": func(t *testing.T) test {
			cert := &acme.Certificate{
				AccountID:     "accounttID",
				OrderID:       "orderID",
				Leaf:          leaf,
				Intermediates: []*x509.Certificate{inter, root},
			}
			return test{
				db: &db.MockNoSQLDB{
					MCmpAndSwap: func(bucket, key, old, nu []byte) ([]byte, bool, error) {
						assert.Equals(t, bucket, certTable)
						assert.Equals(t, key, []byte(cert.ID))
						assert.Equals(t, old, nil)

						dbc := new(dbCert)
						assert.FatalError(t, json.Unmarshal(nu, dbc))
						assert.FatalError(t, err)
						assert.Equals(t, dbc.ID, string(key))
						assert.Equals(t, dbc.ID, cert.ID)
						assert.Equals(t, dbc.AccountID, cert.AccountID)
						assert.True(t, clock.Now().Add(-time.Minute).Before(dbc.Created))
						assert.True(t, clock.Now().Add(time.Minute).After(dbc.Created))
						return nil, false, errors.New("force")
					},
				},
				cert: cert,
				err:  errors.New("error saving acme certificate: force"),
			}
		},
		"ok": func(t *testing.T) test {
			cert := &acme.Certificate{
				AccountID:     "accounttID",
				OrderID:       "orderID",
				Leaf:          leaf,
				Intermediates: []*x509.Certificate{inter, root},
			}
			var (
				id    string
				idPtr = &id
			)

			return test{
				db: &db.MockNoSQLDB{
					MCmpAndSwap: func(bucket, key, old, nu []byte) ([]byte, bool, error) {
						*idPtr = string(key)
						assert.Equals(t, bucket, certTable)
						assert.Equals(t, key, []byte(cert.ID))
						assert.Equals(t, old, nil)

						dbc := new(dbCert)
						assert.FatalError(t, json.Unmarshal(nu, dbc))
						assert.FatalError(t, err)
						assert.Equals(t, dbc.ID, string(key))
						assert.Equals(t, dbc.ID, cert.ID)
						assert.Equals(t, dbc.AccountID, cert.AccountID)
						assert.True(t, clock.Now().Add(-time.Minute).Before(dbc.Created))
						assert.True(t, clock.Now().Add(time.Minute).After(dbc.Created))
						return nil, true, nil
					},
				},
				_id:  idPtr,
				cert: cert,
			}
		},
	}
	for name, run := range tests {
		tc := run(t)
		t.Run(name, func(t *testing.T) {
			db := DB{db: tc.db}
			if err := db.CreateCertificate(context.Background(), tc.cert); err != nil {
				if assert.NotNil(t, tc.err) {
					assert.HasPrefix(t, err.Error(), tc.err.Error())
				}
			} else {
				if assert.Nil(t, tc.err) {
					assert.Equals(t, tc.cert.ID, *tc._id)
				}
			}
		})
	}
}

func TestDB_GetCertificate(t *testing.T) {
	leaf, err := pemutil.ReadCertificate("../../../authority/testdata/certs/foo.crt")
	assert.FatalError(t, err)
	inter, err := pemutil.ReadCertificate("../../../authority/testdata/certs/intermediate_ca.crt")
	assert.FatalError(t, err)
	root, err := pemutil.ReadCertificate("../../../authority/testdata/certs/root_ca.crt")
	assert.FatalError(t, err)

	certID := "certID"
	type test struct {
		db  nosql.DB
		err error
	}
	var tests = map[string]func(t *testing.T) test{
		"fail/not-found": func(t *testing.T) test {
			return test{
				db: &db.MockNoSQLDB{
					MGet: func(bucket, key []byte) ([]byte, error) {
						assert.Equals(t, bucket, certTable)
						assert.Equals(t, string(key), certID)

						return nil, nosqldb.ErrNotFound
					},
				},
				err: errors.New("certificate certID not found"),
			}
		},
		"fail/db.Get-error": func(t *testing.T) test {
			return test{
				db: &db.MockNoSQLDB{
					MGet: func(bucket, key []byte) ([]byte, error) {
						assert.Equals(t, bucket, certTable)
						assert.Equals(t, string(key), certID)

						return nil, errors.Errorf("force")
					},
				},
				err: errors.New("error loading certificate certID: force"),
			}
		},
		"fail/unmarshal-error": func(t *testing.T) test {
			return test{
				db: &db.MockNoSQLDB{
					MGet: func(bucket, key []byte) ([]byte, error) {
						assert.Equals(t, bucket, certTable)
						assert.Equals(t, string(key), certID)

						return []byte("foobar"), nil
					},
				},
				err: errors.New("error unmarshaling certificate certID"),
			}
		},
		"fail/parseBundle-error": func(t *testing.T) test {
			return test{
				db: &db.MockNoSQLDB{
					MGet: func(bucket, key []byte) ([]byte, error) {
						assert.Equals(t, bucket, certTable)
						assert.Equals(t, string(key), certID)

						cert := dbCert{
							ID:        certID,
							AccountID: "accountID",
							OrderID:   "orderID",
							Leaf: pem.EncodeToMemory(&pem.Block{
								Type:  "Public Key",
								Bytes: leaf.Raw,
							}),
							Created: clock.Now(),
						}
						b, err := json.Marshal(cert)
						assert.FatalError(t, err)

						return b, nil
					},
				},
				err: errors.Errorf("error parsing certificate chain for ACME certificate with ID certID"),
			}
		},
		"ok": func(t *testing.T) test {
			return test{
				db: &db.MockNoSQLDB{
					MGet: func(bucket, key []byte) ([]byte, error) {
						assert.Equals(t, bucket, certTable)
						assert.Equals(t, string(key), certID)

						cert := dbCert{
							ID:        certID,
							AccountID: "accountID",
							OrderID:   "orderID",
							Leaf: pem.EncodeToMemory(&pem.Block{
								Type:  "CERTIFICATE",
								Bytes: leaf.Raw,
							}),
							Intermediates: append(pem.EncodeToMemory(&pem.Block{
								Type:  "CERTIFICATE",
								Bytes: inter.Raw,
							}), pem.EncodeToMemory(&pem.Block{
								Type:  "CERTIFICATE",
								Bytes: root.Raw,
							})...),
							Created: clock.Now(),
						}
						b, err := json.Marshal(cert)
						assert.FatalError(t, err)

						return b, nil
					},
				},
			}
		},
	}
	for name, run := range tests {
		tc := run(t)
		t.Run(name, func(t *testing.T) {
			db := DB{db: tc.db}
			cert, err := db.GetCertificate(context.Background(), certID)
			if err != nil {
				if assert.NotNil(t, tc.err) {
					assert.HasPrefix(t, err.Error(), tc.err.Error())
				}
			} else {
				if assert.Nil(t, tc.err) {
					assert.Equals(t, cert.ID, certID)
					assert.Equals(t, cert.AccountID, "accountID")
					assert.Equals(t, cert.OrderID, "orderID")
					assert.Equals(t, cert.Leaf, leaf)
					assert.Equals(t, cert.Intermediates, []*x509.Certificate{inter, root})
				}
			}
		})
	}
}

func Test_parseBundle(t *testing.T) {
	leaf, err := pemutil.ReadCertificate("../../../authority/testdata/certs/foo.crt")
	assert.FatalError(t, err)
	inter, err := pemutil.ReadCertificate("../../../authority/testdata/certs/intermediate_ca.crt")
	assert.FatalError(t, err)
	root, err := pemutil.ReadCertificate("../../../authority/testdata/certs/root_ca.crt")
	assert.FatalError(t, err)

	var certs []byte
	for _, cert := range []*x509.Certificate{leaf, inter, root} {
		certs = append(certs, pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: cert.Raw,
		})...)
	}

	type test struct {
		b   []byte
		err error
	}
	var tests = map[string]test{
		"fail/bad-type-error": {
			b: pem.EncodeToMemory(&pem.Block{
				Type:  "Public Key",
				Bytes: leaf.Raw,
			}),
			err: errors.Errorf("error decoding PEM: data contains block that is not a certificate"),
		},
		"fail/bad-pem-error": {
			b: pem.EncodeToMemory(&pem.Block{
				Type:  "CERTIFICATE",
				Bytes: []byte("foo"),
			}),
			err: errors.Errorf("error parsing x509 certificate"),
		},
		"fail/unexpected-data": {
			b: append(pem.EncodeToMemory(&pem.Block{
				Type:  "CERTIFICATE",
				Bytes: leaf.Raw,
			}), []byte("foo")...),
			err: errors.Errorf("error decoding PEM: unexpected data"),
		},
		"ok": {
			b: certs,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ret, err := parseBundle(tc.b)
			if err != nil {
				if assert.NotNil(t, tc.err) {
					assert.HasPrefix(t, err.Error(), tc.err.Error())
				}
			} else {
				if assert.Nil(t, tc.err) {
					assert.Equals(t, ret, []*x509.Certificate{leaf, inter, root})
				}
			}
		})
	}
}
