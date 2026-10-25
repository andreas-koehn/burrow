package grpce2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// testCA signs every certificate of the run: the relays' control and
// dashboard listeners, the uploaded custom-domain certificate, the local TLS
// upstream and the tcp echo service.
type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	pool    *x509.CertPool
}

func newTestCA() (*testCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "burrow grpce2e CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pool: pool}, nil
}

var serial int64 = 100

// leaf is a server certificate for names (DNS names or IPs).
type leaf struct {
	certPEM, keyPEM []byte
	cn              string
}

func (ca *testCA) issue(cn string, names ...string) (leaf, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return leaf{}, err
	}
	serial++
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return leaf{}, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return leaf{}, err
	}
	return leaf{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}),
		cn:      cn,
	}, nil
}

func (l leaf) tlsCert() (tls.Certificate, error) { return tls.X509KeyPair(l.certPEM, l.keyPEM) }

// write stores the pair as <dir>/<name>.pem and <dir>/<name>-key.pem.
func (l leaf) write(dir, name string) (certPath, keyPath string, err error) {
	certPath, keyPath = filepath.Join(dir, name+".pem"), filepath.Join(dir, name+"-key.pem")
	if err = os.WriteFile(certPath, l.certPEM, 0o600); err != nil {
		return
	}
	err = os.WriteFile(keyPath, l.keyPEM, 0o600)
	return
}
