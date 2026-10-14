package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
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
	Source      string `json:"source"`
	Subject     string `json:"subject"`
}

// CA 来源，优先级从高到低：导入 > 程序内置 > 本机生成。
const (
	caSourceImported = "已导入"
	caSourceEmbedded = "程序内置"
	caSourceLocal    = "本机生成"
)

// caIdentity 是签发叶子证书的 CA；导入或移除导入时整体替换。
type caIdentity struct {
	root   *x509.Certificate
	key    crypto.Signer
	public []byte
	source string
}
type HTTPSManager struct {
	// nil uses the operating system trust store for upstream servers.
	upstreamRoots *x509.CertPool
	mu            sync.Mutex
	dir           string
	ca            *caIdentity
	settings      HTTPSSettings
	leaves        map[string]*tls.Certificate
}

// 编译前放入 embedded_ca/ca.pem 的共享 CA 会内置进程序，所有电脑共用同一张证书，
// App 只需打包一次。该文件含私钥，已被 .gitignore 忽略；目录里没有它时按机器生成。
//
//go:embed embedded_ca
var embeddedCAFiles embed.FS

var embeddedCA, _ = embeddedCAFiles.ReadFile("embedded_ca/ca.pem")

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// The CA identity is created once. An existing invalid identity is never replaced:
// replacing it would silently break the trust embedded in Android debug builds.
func loadLocalCA(dir string) ([]byte, error) {
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
	if err := os.Chmod(identity, 0600); err != nil {
		return nil, err
	}
	return data, nil
}

func openHTTPSManager(dir string) (*HTTPSManager, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	ca, err := loadCA(dir)
	if err != nil {
		return nil, err
	}
	m := &HTTPSManager{dir: dir, ca: ca, leaves: map[string]*tls.Certificate{}, settings: HTTPSSettings{Domains: []string{}}}
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

// loadCA 按优先级加载 CA：已导入的身份存在时必须有效，不会悄悄退回其他 CA。
func loadCA(dir string) (*caIdentity, error) {
	data, err := os.ReadFile(filepath.Join(dir, importedCAFile))
	if err == nil {
		return parseCAIdentity(data, caSourceImported)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	return loadDefaultCA(dir)
}

func loadDefaultCA(dir string) (*caIdentity, error) {
	if len(embeddedCA) > 0 {
		return parseCAIdentity(embeddedCA, caSourceEmbedded)
	}
	data, err := loadLocalCA(dir)
	if err != nil {
		return nil, err
	}
	return parseCAIdentity(data, caSourceLocal)
}

const importedCAFile = "ca-imported.pem"

func parseCAIdentity(data []byte, source string) (*caIdentity, error) {
	name := map[string]string{caSourceImported: "导入的 CA", caSourceEmbedded: "程序内置的 CA", caSourceLocal: "固定 CA"}[source]
	pair, err := tls.X509KeyPair(data, data)
	if err != nil {
		return nil, fmt.Errorf("%s无效（不会自动替换）: %w", name, err)
	}
	root, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok || !root.IsCA || root.KeyUsage&x509.KeyUsageCertSign == 0 || time.Now().Before(root.NotBefore) || !time.Now().Before(root.NotAfter) {
		return nil, fmt.Errorf("%s不是可签发证书的有效 CA，或已过期（不会自动替换）", name)
	}
	return &caIdentity{root: root, key: key, public: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}), source: source}, nil
}

// importCA 校验并持久化导入的 CA（证书和私钥 PEM），之后签发的证书都改用它。
func (m *HTTPSManager) importCA(data []byte) error {
	ca, err := parseCAIdentity(data, caSourceImported)
	if err != nil {
		return err
	}
	identity, err := ca.identityPEM()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err = writeFileAtomic(m.dir, importedCAFile, identity); err != nil {
		return err
	}
	m.ca, m.leaves = ca, map[string]*tls.Certificate{}
	return nil
}

// removeImportedCA 删除导入的 CA，恢复为程序内置或本机生成的 CA。
func (m *HTTPSManager) removeImportedCA() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ca.source != caSourceImported {
		return fmt.Errorf("当前没有导入的 CA")
	}
	ca, err := loadDefaultCA(m.dir)
	if err != nil {
		return err
	}
	if err = os.Remove(filepath.Join(m.dir, importedCAFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	m.ca, m.leaves = ca, map[string]*tls.Certificate{}
	return nil
}

func (m *HTTPSManager) publicPEM() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ca.public
}

// identity 导出当前 CA 的证书和私钥，用于备份或迁移到其他电脑。
func (m *HTTPSManager) identity() ([]byte, error) {
	m.mu.Lock()
	ca := m.ca
	m.mu.Unlock()
	return ca.identityPEM()
}

func (ca *caIdentity) identityPEM() ([]byte, error) {
	key, err := x509.MarshalPKCS8PrivateKey(ca.key)
	if err != nil {
		return nil, err
	}
	return append(append([]byte{}, ca.public...), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...), nil
}

func writeFileAtomic(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(0600); err != nil {
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
	return os.Rename(f.Name(), filepath.Join(dir, name))
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
	sum := sha256.Sum256(m.ca.root.Raw)
	s := m.settings
	s.Domains = append([]string{}, s.Domains...)
	return HTTPSStatus{HTTPSSettings: s, Fingerprint: strings.ToUpper(hex.EncodeToString(sum[:])), Expires: m.ca.root.NotAfter.Format(time.RFC3339), Directory: m.dir, Source: m.ca.source, Subject: m.ca.root.Subject.CommonName}
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
	if until.After(m.ca.root.NotAfter) {
		until = m.ca.root.NotAfter
	}
	leaf := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, NotBefore: time.Now().Add(-5 * time.Minute), NotAfter: until, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	if ip := net.ParseIP(host); ip != nil {
		leaf.IPAddresses = []net.IP{ip}
	} else {
		leaf.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, m.ca.root, &key.PublicKey, m.ca.key)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{Certificate: [][]byte{der, m.ca.root.Raw}, PrivateKey: key, Leaf: parsed}
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

// 其他 App：不打包证书，debug 包信任手机上用户安装的 CA，手机需先安装下载的 CA 证书。
const androidUserConfig = `<?xml version="1.0" encoding="utf-8"?>
<network-security-config>
    <debug-overrides>
        <trust-anchors>
            <certificates src="user" />
        </trust-anchors>
    </debug-overrides>
</network-security-config>
`
