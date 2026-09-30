// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/conduit-sync/conduit/internal/peer"
)

// Every site has its own self-signed certificate. Other sites trust it by
// pinning its SHA-256 fingerprint (see package peer), which travels in invite
// codes, beacons and gossiped member records, so no CA is needed.

// TLSMaterial is this site's certificate and key.
type TLSMaterial struct {
	CertPEM     []byte
	KeyPEM      []byte
	Fingerprint string
}

// GenerateTLS creates a long-lived ECDSA P-256 certificate.
func GenerateTLS(name string) (*TLSMaterial, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "conduit " + name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(20, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &TLSMaterial{
		CertPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:      pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		Fingerprint: peer.Fingerprint(der),
	}, nil
}

// LoadOrCreateTLS returns this site's certificate, creating it on first run.
func LoadOrCreateTLS(ctx context.Context, pool *pgxpool.Pool, name string) (*TLSMaterial, error) {
	var m TLSMaterial
	err := pool.QueryRow(ctx, `SELECT cert_pem, key_pem, fingerprint FROM conduit.tls`).Scan(&m.CertPEM, &m.KeyPEM, &m.Fingerprint)
	if err == nil {
		return &m, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	gen, err := GenerateTLS(name)
	if err != nil {
		return nil, err
	}
	_, err = pool.Exec(ctx, `INSERT INTO conduit.tls (cert_pem, key_pem, fingerprint) VALUES ($1, $2, $3)
		ON CONFLICT (singleton) DO NOTHING`, gen.CertPEM, gen.KeyPEM, gen.Fingerprint)
	if err != nil {
		return nil, err
	}
	return LoadOrCreateTLS(ctx, pool, name)
}

// ServerConfig serves this site's certificate.
func (m *TLSMaterial) ServerConfig() (*tls.Config, error) {
	cert, err := tls.X509KeyPair(m.CertPEM, m.KeyPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}, nil
}
