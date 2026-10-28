package cmd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in because this starts a real browser, always with a temporary profile.
// No certificate-verification bypass is used: WebTransport pins the short-lived
// ECDSA leaf through the browser's serverCertificateHashes API.
func TestWebTransportBrowserChrome(t *testing.T) {
	if os.Getenv("XROK_BROWSER_TEST") != "1" {
		t.Skip("set XROK_BROWSER_TEST=1 for real Chrome integration")
	}
	chrome := os.Getenv("XROK_CHROME")
	if chrome == "" {
		chrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	}
	if _, err := os.Stat(chrome); err != nil {
		t.Fatal("set XROK_CHROME to an installed Chrome/Chromium executable")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "owned WebTransport browser test"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(7 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	f := webTransportTLSFixture(t, true, &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key})
	token := testJWT(f.route.Capability, time.Now().Add(time.Minute).Unix(), "browser")
	endpoint, _ := json.Marshal(f.url + "?cap=" + token)
	hash := sha256.Sum256(der)
	values := make([]int, len(hash))
	for i, b := range hash {
		values[i] = int(b)
	}
	hashJSON, _ := json.Marshal(values)
	result := make(chan string, 1)
	page := fmt.Sprintf(`<!doctype html><meta charset="utf-8"><title>xrok WebTransport test</title><pre id="result">running</pre><script>
(async()=>{let answer;try{
 const wt=new WebTransport(%s,{serverCertificateHashes:[{algorithm:'sha-256',value:Uint8Array.from(%s)}]});await wt.ready;
 const s=await wt.createBidirectionalStream();const writer=s.writable.getWriter();await writer.write(new TextEncoder().encode('browser'));await writer.close();
 const reader=s.readable.getReader();let reply='';for(;;){const {value,done}=await reader.read();if(done)break;reply+=new TextDecoder().decode(value);}
 if(reply!=='reply:browser')throw new Error('stream integrity: '+reply);
 const dw=wt.datagrams.writable.getWriter();await dw.write(new TextEncoder().encode('browser-udp'));
 const dr=wt.datagrams.readable.getReader();const data=await dr.read();if(new TextDecoder().decode(data.value)!=='udp:browser-udp')throw new Error('datagram integrity');
 wt.close();answer='PASS streams+half-close+datagrams+certificate-pin';
}catch(e){answer='FAIL '+String(e)}document.getElementById('result').textContent=answer;await fetch('/result',{method:'POST',body:answer});})();</script>`, endpoint, hashJSON)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/result" {
			b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			select {
			case result <- string(b):
			default:
			}
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, page)
	}))
	defer front.Close()
	f.route.WebTransportOrigins = []string{front.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, chrome, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check", "--disable-background-networking", "--disable-component-update", "--disable-sync", "--no-proxy-server", "--user-data-dir="+filepath.Join(t.TempDir(), "profile"), front.URL)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	select {
	case answer := <-result:
		if answer != "PASS streams+half-close+datagrams+certificate-pin" {
			t.Fatal(answer)
		}
		t.Log(answer)
	case err := <-done:
		done <- err
		t.Fatalf("Chrome exited before reporting: %v", err)
	case <-ctx.Done():
		t.Fatal("Chrome did not finish WebTransport integration")
	}
}
