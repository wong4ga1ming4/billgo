package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"

	"billgo/internal/identity"
)

const certValidity = 10 * 365 * 24 * time.Hour

// Certificate 用节点的身份密钥签一张自签证书。证书公钥就是身份公钥，
// 所以 TLS 校验等价于身份校验，不需要 CA。
func Certificate(priv ed25519.PrivateKey) (tls.Certificate, error) {
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return tls.Certificate{}, fmt.Errorf("私钥不是 ed25519")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("生成证书序列号: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: identity.Fingerprint(pub)[:identity.PeerIDLen]},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("签发自签证书: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// ServerTLSConfig 返回服务端 TLS 配置。
func ServerTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
}

// PinnedConfig 返回只信任指定 peer_id 的客户端 TLS 配置。
// 自己发起的握手不做 CA 校验，改为核对对端证书公钥的指纹。
func PinnedConfig(expectedPeerID string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // 自签证书没有 CA 链，信任来自下面的指纹比对
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("%w: 对端没有提供证书", ErrIdentity)
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("%w: 解析对端证书: %v", ErrIdentity, err)
			}
			pub, ok := cert.PublicKey.(ed25519.PublicKey)
			if !ok {
				return fmt.Errorf("%w: 对端证书公钥不是 ed25519", ErrIdentity)
			}
			if got := identity.Fingerprint(pub)[:identity.PeerIDLen]; got != expectedPeerID {
				return fmt.Errorf("%w: 期望 %s，得到 %s", ErrIdentity, expectedPeerID, got)
			}
			return nil
		},
	}
}
