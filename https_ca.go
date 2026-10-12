package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type HTTPSSettings struct {
	Enabled bool     `json:"enabled"`
	Domains []string `json:"domains"`
}
type HTTPSStatus struct {
	HTTPSSettings
	Fingerprint string `json:"fingerprint"`
	Expires     string `json:"expires"`
	Directory   string `json:"directory"`
}
type HTTPSManager struct {
	// nil uses the operating system trust store for upstream servers.
	upstreamRoots *x509.CertPool
	mu            sync.Mutex
	dir           string
	root          *x509.Certificate
	key           *ecdsa.PrivateKey
	public        []byte
	settings      HTTPSSettings
	leaves        map[string]*tls.Certificate
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// The CA identity is created once. An existing invalid identity is never replaced:
// replacing it would silently break the trust embedded in Android debug builds.
func openHTTPSManager(dir string) (*HTTPSManager, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	identity := filepath.Join(dir, "ca.pem")
	data, err := os.ReadFile(identity)
	if os.IsNotExist(err) {
		key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			return nil, e
		}
		serial, e := randomSerial()
		if e != nil {
			return nil, e
		}
		now := time.Now()
		template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Weaknet Local Debug CA"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
		der, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if e != nil {
			return nil, e
		}
		private, e := x509.MarshalECPrivateKey(key)
		if e != nil {
			return nil, e
		}
		generated := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private})...)
		// Publish a complete identity atomically; concurrent first launches reuse the winner.
		tmp, e := os.CreateTemp(dir, ".ca-*")
		if e != nil {
			return nil, e
		}
		name := tmp.Name()
		defer os.Remove(name)
		if _, e = tmp.Write(generated); e != nil {
			tmp.Close()
			return nil, e
		}
		if e = tmp.Sync(); e != nil {
			tmp.Close()
			return nil, e
		}
		if e = tmp.Close(); e != nil {
			return nil, e
		}
		if e = os.Link(name, identity); e != nil && !os.IsExist(e) {
			return nil, e
		}
		data, err = os.ReadFile(identity)
	}
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(data, data)
	if err != nil {
		return nil, fmt.Errorf("固定 CA 无效（不会自动替换）: %w", err)
	}
	root, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || !root.IsCA || root.KeyUsage&x509.KeyUsageCertSign == 0 || time.Now().Before(root.NotBefore) || !time.Now().Before(root.NotAfter) {
		return nil, fmt.Errorf("固定 CA 不是有效的 ECDSA CA，或已过期（不会自动替换）")
	}
	if err := os.Chmod(identity, 0600); err != nil {
		return nil, err
	}
	m := &HTTPSManager{dir: dir, root: root, key: key, public: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}), leaves: map[string]*tls.Certificate{}, settings: HTTPSSettings{Domains: []string{}}}
	settings, err := os.ReadFile(filepath.Join(dir, "https.json"))
	if err == nil {
		if err = json.Unmarshal(settings, &m.settings); err != nil {
			return nil, fmt.Errorf("HTTPS 设置无效: %w", err)
		}
		m.settings, err = validateHTTPSSettings(m.settings)
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return m, nil
}

func validateHTTPSSettings(s HTTPSSettings) (HTTPSSettings, error) {
	seen := map[string]bool{}
	domains := []string{}
	if len(s.Domains) > 128 {
		return s, fmt.Errorf("解密域名最多 128 项")
	}
	for _, raw := range s.Domains {
		host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
		if host == "" {
			continue
		}
		valid := net.ParseIP(host) != nil
		if !valid && len(host) <= 253 {
			valid = true
			for _, label := range strings.Split(host, ".") {
				if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
					valid = false
					break
				}
				for _, c := range label {
					if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
						valid = false
						break
					}
				}
			}
		}
		if !valid {
			return s, fmt.Errorf("无效域名 %q：只填写域名或 IP，不含协议、端口、路径或通配符", raw)
		}
		if !seen[host] {
			domains = append(domains, host)
			seen[host] = true
		}
	}
	if s.Enabled && len(domains) == 0 {
		return s, fmt.Errorf("请先填写需要解密的域名")
	}
	s.Domains = domains
	return s, nil
}
func (m *HTTPSManager) status() HTTPSStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	sum := sha256.Sum256(m.root.Raw)
	s := m.settings
	s.Domains = append([]string{}, s.Domains...)
	return HTTPSStatus{s, strings.ToUpper(hex.EncodeToString(sum[:])), m.root.NotAfter.Format(time.RFC3339), m.dir}
}
func (m *HTTPSManager) update(s HTTPSSettings) error {
	s, err := validateHTTPSSettings(s)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(m.dir, ".https-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(m.dir, "https.json")); err != nil {
		return err
	}
	m.settings = s
	return nil
}
func (m *HTTPSManager) enabledFor(host string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings.Enabled && matchDomain(strings.ToLower(strings.TrimSuffix(host, ".")), m.settings.Domains)
}
func (m *HTTPSManager) certificate(host string) (*tls.Certificate, error) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	m.mu.Lock()
	defer m.mu.Unlock()
	if cert := m.leaves[host]; cert != nil && time.Now().Add(time.Hour).Before(cert.Leaf.NotAfter) {
		return cert, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	until := time.Now().Add(24 * time.Hour)
	if until.After(m.root.NotAfter) {
		until = m.root.NotAfter
	}
	leaf := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, NotBefore: time.Now().Add(-5 * time.Minute), NotAfter: until, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	if ip := net.ParseIP(host); ip != nil {
		leaf.IPAddresses = []net.IP{ip}
	} else {
		leaf.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, m.root, &key.PublicKey, m.key)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{Certificate: [][]byte{der, m.root.Raw}, PrivateKey: key, Leaf: parsed}
	if len(m.leaves) >= 256 {
		m.leaves = map[string]*tls.Certificate{}
	}
	m.leaves[host] = cert
	return cert, nil
}

const androidDebugConfig = `<?xml version="1.0" encoding="utf-8"?>
<network-security-config>
    <debug-overrides>
        <trust-anchors>
            <certificates src="@raw/weaknet_ca" />
        </trust-anchors>
    </debug-overrides>
</network-security-config>
`
