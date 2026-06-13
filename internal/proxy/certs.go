package proxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"time"
)

type CertCache struct {
	mu       sync.Mutex
	caCert   *x509.Certificate
	caKey    *rsa.PrivateKey
	caTLSCert tls.Certificate
	certPool *x509.CertPool
	cache    map[string]*tls.Certificate
}

func NewCertCache() (*CertCache, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Replay Proxy CA"},
			CommonName:   "Replay Proxy CA",
		},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}

	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, err
	}

	caTLSCert := tls.Certificate{
		Certificate: [][]byte{caCertDER},
		PrivateKey:  caKey,
		Leaf:        caCert,
	}

	certPool := x509.NewCertPool()
	certPool.AddCert(caCert)

	return &CertCache{
		caCert:    caCert,
		caKey:     caKey,
		caTLSCert: caTLSCert,
		certPool:  certPool,
		cache:     make(map[string]*tls.Certificate),
	}, nil
}

func (cc *CertCache) Get(host string) (*tls.Certificate, error) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	if cert, ok := cc.cache[host]; ok {
		return cert, nil
	}

	cert, err := cc.generateCert(host)
	if err != nil {
		return nil, err
	}

	cc.cache[host] = cert
	return cert, nil
}

func (cc *CertCache) generateCert(host string) (*tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	hostWithoutPort, _, _ := net.SplitHostPort(host)
	if hostWithoutPort == "" {
		hostWithoutPort = host
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			Organization: []string{"Replay Proxy"},
			CommonName:   hostWithoutPort,
		},
		NotBefore:   time.Now().Add(-1 * time.Hour),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{hostWithoutPort},
		IPAddresses: parseIPAddresses(hostWithoutPort),
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, cc.caCert, &key.PublicKey, cc.caKey)
	if err != nil {
		return nil, err
	}

	tlsCert := &tls.Certificate{
		Certificate: [][]byte{certDER, cc.caTLSCert.Certificate[0]},
		PrivateKey:  key,
		Leaf:        nil,
	}
	leaf, _ := x509.ParseCertificate(certDER)
	if leaf != nil {
		tlsCert.Leaf = leaf
	}

	return tlsCert, nil
}

func parseIPAddresses(host string) []net.IP {
	ip := net.ParseIP(host)
	if ip != nil {
		return []net.IP{ip}
	}
	return nil
}
