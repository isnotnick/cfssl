package transport

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"time"

	"github.com/cloudflare/cfssl/csr"
	"github.com/cloudflare/cfssl/log"
	"github.com/cloudflare/cfssl/transport/ca"
	"github.com/cloudflare/cfssl/transport/core"
	"github.com/cloudflare/cfssl/transport/kp"
)

func envOrDefault(key, def string) string {
	val := os.Getenv(key)
	if val == "" {
		return def
	}
	return val
}

var (
	// NewKeyProvider is the function used to build key providers
	// from some identity.
	NewKeyProvider = func(id *core.Identity) (kp.KeyProvider, error) {
		return kp.NewStandardProvider(id)
	}

	// NewCA is used to load a configuration for a certificate
	// authority.
	NewCA = func(id *core.Identity) (ca.CertificateAuthority, error) {
		return ca.NewCFSSLProvider(id, nil)
	}
)

// A Transport is capable of providing transport-layer security using
// TLS.
type Transport struct {
	// Before defines how long before the certificate expires the
	// transport should start attempting to refresh the
	// certificate. For example, if this is 24h, then 24 hours
	// before the certificate expires the Transport will start
	// attempting to replace it.
	Before time.Duration

	// Provider contains a key management provider.
	Provider kp.KeyProvider

	// CA contains a mechanism for obtaining signed certificates.
	CA ca.CertificateAuthority

	// Roots contains the pool of X.509 certificates that are
	// valid for authenticating peers.
	Roots *x509.CertPool

	// Identity contains information about the entity that will be
	// used to construct certificates.
	Identity *core.Identity
}

// NewTransport builds a new transport from the default
func NewTransport(before time.Duration, identity *core.Identity) (*Transport, error) {
	var tr = &Transport{
		Before:   before,
		Roots:    core.SystemRoots,
		Identity: identity,
	}

	var err error
	tr.Provider, err = NewKeyProvider(identity)
	if err != nil {
		return nil, err
	}

	tr.CA, err = NewCA(identity)
	if err != nil {
		return nil, err
	}

	return tr, nil
}

// Lifespan returns how much time is left before the transport's
// certificate expires, or 0 if the certificate is not present or
// expired.
func (tr *Transport) Lifespan() time.Duration {
	cert := tr.Provider.Certificate()
	if cert == nil {
		return 0
	}

	now := time.Now()
	if now.After(cert.NotAfter) {
		return 0
	}

	now = now.Add(tr.Before)
	ls := cert.NotAfter.Sub(now)
	if ls < 0 {
		return 0
	}
	return ls
}

// RefreshKeys will make sure the Transport has loaded keys and has a
// valid certificate. It will handle any persistence, check that the
// certificate is valid (i.e. that its expiry date is within the
// Before date), and handle certificate reissuance as needed.
func (tr *Transport) RefreshKeys() (err error) {
	if !tr.Provider.Ready() {
		log.Debug("key and certificate aren't ready, loading")
		err = tr.Provider.Load()
		if err != nil && err != kp.ErrCertificateUnavailable {
			log.Debugf("failed to load keypair: %v", err)
			kr := tr.Identity.Request.KeyRequest
			if kr == nil {
				kr = csr.NewBasicKeyRequest()
			}

			err = tr.Provider.Generate(kr.Algo(), kr.Size())
			if err != nil {
				log.Debugf("failed to generate key: %v", err)
				return
			}
		}
	}

	lifespan := tr.Lifespan()
	if lifespan < tr.Before {
		log.Debugf("transport's certificate is out of date (lifespan %s)", lifespan)
		req, err := tr.Provider.CertificateRequest(tr.Identity.Request)
		if err != nil {
			log.Debugf("couldn't get a CSR: %v", err)
			return err
		}

		log.Debug("requesting certificate from CA")
		cert, err := tr.CA.SignCSR(req)
		if err != nil {
			log.Debugf("failed to get the certificate signed: %v", err)
			return err
		}

		log.Debug("giving the certificate to the provider")
		err = tr.Provider.SetCertificatePEM(cert)
		if err != nil {
			log.Debugf("failed to set the provider's certificate: %v", err)
			return err
		}

		log.Debug("storing the certificate")
		err = tr.Provider.Store()
		if err != nil {
			log.Debugf("the provider failed to store the certificate: %v", err)
			return err
		}
	}

	return nil
}

func (tr *Transport) getCertificate() (cert tls.Certificate, err error) {
	if !tr.Provider.Ready() {
		log.Debug("transport isn't ready; attempting to refresh keypair")
		err = tr.RefreshKeys()
		if err != nil {
			log.Debugf("transport couldn't get a certificate: %v", err)
			return
		}
	}

	cert, err = tr.Provider.X509KeyPair()
	if err != nil {
		log.Debugf("couldn't generate an X.509 keypair: %v", err)
	}

	return
}

// Dial initiates a TLS connection to an outbound server. It returns a
// TLS connection to the server.
func Dial(address string, tr *Transport) (*tls.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		// Assume address is a hostname, and that it should
		// use the HTTPS port number.
		host = address
		address = net.JoinHostPort(address, "443")
	}

	cert, err := tr.getCertificate()
	if err != nil {
		return nil, err
	}

	cfg := core.TLSClientAuthClientConfig(cert, host)
	return tls.Dial("tcp", address, cfg)
}

// AutoUpdate will automatically update the listener. If a non-nil
// certUpdates chan is provided, it will receive timestamps for
// reissued certificates. If errChan is non-nil, any errors that occur
// in the updater will be passed along.
func (tr *Transport) AutoUpdate(certUpdates chan time.Time, errChan chan error) {
	for {
		// Wait until it's time to update the certificate.
		target := time.Now().Add(tr.Lifespan())
		if PollInterval == 0 {
			<-time.After(tr.Lifespan())
		} else {
			pollWait(target)
		}

		// Keep trying to update the certificate until it's
		// ready.
		for {
			log.Debugf("attempting to refresh keypair")
			err := tr.RefreshKeys()
			if err == nil {
				break
			}

			log.Debug("failed to update certificate, will try again in 5 minutes")
			if errChan != nil {
				errChan <- err
			}

			<-time.After(5 * time.Minute)
		}

		log.Debugf("certificate updated")
		if certUpdates != nil {
			certUpdates <- time.Now()
		}
	}
}
