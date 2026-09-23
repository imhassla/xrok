package cmd

import (
	"crypto/tls"
	"log"
	"os"
	"sync"
	"time"
)

// certReloader serves the TLS certificate from disk and picks up renewals
// (e.g. certbot) without a restart: at most once a minute it compares file
// mtimes and reloads. A failed reload keeps serving the previous pair.
type certReloader struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	modTime time.Time
	checked time.Time
}

var (
	serverCertsMu sync.Mutex
	serverCerts   = map[[2]string]*certReloader{}
)

// serverCertReloader returns the shared reloader for the configured
// certificate pair, loading it on first use.
func serverCertReloader() (*certReloader, error) {
	key := [2]string{serverCertFile, serverKeyFile}
	serverCertsMu.Lock()
	defer serverCertsMu.Unlock()
	if r, ok := serverCerts[key]; ok {
		return r, nil
	}
	r := &certReloader{certFile: key[0], keyFile: key[1]}
	if err := r.load(); err != nil {
		return nil, err
	}
	serverCerts[key] = r
	return r, nil
}

func (r *certReloader) newestModTime() time.Time {
	var newest time.Time
	for _, f := range []string{r.certFile, r.keyFile} {
		if st, err := os.Stat(f); err == nil && st.ModTime().After(newest) {
			newest = st.ModTime()
		}
	}
	return newest
}

func (r *certReloader) load() error {
	mod := r.newestModTime()
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return err
	}
	r.cert, r.modTime, r.checked = &cert, mod, time.Now()
	return nil
}

func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.checked) > time.Minute {
		r.checked = time.Now()
		if mod := r.newestModTime(); mod.After(r.modTime) {
			if err := r.load(); err != nil {
				log.Printf("TLS certificate reload failed, keeping previous: %v", err)
			} else {
				log.Printf("TLS certificate reloaded from %s", r.certFile)
			}
		}
	}
	return r.cert, nil
}
