// Command capture-proxy is a logging TLS-intercepting forward proxy for
// reverse-engineering Zen request fidelity. It terminates CONNECT tunnels
// with on-the-fly certificates signed by a local CA, records exact wire
// bytes (headers in sent order plus bodies) for both directions, and
// forwards to the real upstream. Traces are appended as JSONL to TRACE_DIR.
package main

import (
	"bufio"
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	listenAddr = getenv("CAPTURE_ADDR", "127.0.0.1:18090")
	traceDir   = getenv("TRACE_DIR", "/tmp/opencode/traces")
	maxBody    = 16 << 20
)

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

type traceEvent struct {
	Timestamp string `json:"ts"`
	Flow      string `json:"flow"`
	Direction string `json:"dir"`
	Host      string `json:"host"`
	RawHead   string `json:"raw_head"`
	BodyLen   int    `json:"body_len"`
	Body      string `json:"body,omitempty"`
	Note      string `json:"note,omitempty"`
}

var (
	traceMu sync.Mutex
	traceFH *os.File
)

func trace(ev traceEvent) {
	ev.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	traceMu.Lock()
	defer traceMu.Unlock()
	_, _ = traceFH.Write(append(data, '\n'))
}

func main() {
	os.Exit(run())
}

func run() int {
	if err := os.MkdirAll(traceDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "capture-proxy: mkdir: %v\n", err)
		return 1
	}
	caCert, caKey := loadCA(
		getenv("MITM_CA_CERT", filepath.Join(traceDir, "mitm-ca.pem")),
		getenv("MITM_CA_KEY", filepath.Join(traceDir, "mitm-ca-key.pem")),
	)
	fh, err := os.OpenFile(filepath.Join(traceDir, "wire.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "capture-proxy: open trace: %v\n", err)
		return 1
	}
	defer fh.Close()
	traceFH = fh

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "capture-proxy: listen: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "capture-proxy: listening %s traces=%s\n", listenAddr, traceDir)
	issuer := &leafIssuer{cert: caCert, key: caKey}
	var flowID int64
	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		flowID++
		go handleClient(conn, fmt.Sprintf("flow-%04d", flowID), issuer)
	}
}

func loadCA(certPath, keyPath string) (*x509.Certificate, crypto.Signer) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		panic(err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		panic(err)
	}
	block, _ := pem.Decode(certPEM)
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		panic(err)
	}
	block, _ = pem.Decode(keyPEM)
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return caCert, key
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return caCert, key
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		panic(err)
	}
	switch key := parsed.(type) {
	case *ecdsa.PrivateKey:
		return caCert, key
	case *rsa.PrivateKey:
		return caCert, key
	default:
		panic(fmt.Sprintf("unsupported CA key type %T", parsed))
	}
}

type leafIssuer struct {
	cert  *x509.Certificate
	key   crypto.Signer
	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

func (issuer *leafIssuer) forHost(host string) *tls.Certificate {
	issuer.mu.Lock()
	defer issuer.mu.Unlock()
	if issuer.cache == nil {
		issuer.cache = map[string]*tls.Certificate{}
	}
	if cached, ok := issuer.cache[host]; ok {
		return cached
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer.cert, &leafKey.PublicKey, issuer.key)
	if err != nil {
		return nil
	}
	cert := &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  leafKey,
	}
	issuer.cache[host] = cert
	return cert
}

func handleClient(downstream net.Conn, flowID string, issuer *leafIssuer) {
	defer downstream.Close()
	reader := bufio.NewReader(downstream)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	parts := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
	if len(parts) < 2 {
		return
	}
	method, target := parts[0], parts[1]
	if strings.ToUpper(method) != "CONNECT" {
		trace(traceEvent{Flow: flowID, Direction: "note", Note: "non-CONNECT: " + strings.TrimSpace(line)})
		return
	}
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		host = target
		target += ":443"
	}
	if _, err := reader.ReadString('\n'); err != nil {
		return
	}
	for {
		header, err := reader.ReadString('\n')
		if err != nil || header == "\r\n" || header == "\n" {
			break
		}
	}
	if _, err := downstream.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	leaf := issuer.forHost(host)
	if leaf == nil {
		return
	}
	tlsConn := tls.Server(downstream, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
	})
	if err := tlsConn.Handshake(); err != nil {
		trace(traceEvent{Flow: flowID, Direction: "note", Host: host, Note: "tls handshake: " + err.Error()})
		return
	}
	cs := tlsConn.ConnectionState()
	trace(traceEvent{Flow: flowID, Direction: "note", Host: host,
		Note: fmt.Sprintf("tls negotiated version=%s cipher=%s servername=%s",
			tlsVersion(cs.Version), tls.CipherSuiteName(cs.CipherSuite), cs.ServerName)})

	upstream, err := tls.Dial("tcp", target, &tls.Config{ServerName: host})
	if err != nil {
		trace(traceEvent{Flow: flowID, Direction: "note", Host: host, Note: "upstream dial: " + err.Error()})
		return
	}
	defer upstream.Close()

	downstreamReader := bufio.NewReader(tlsConn)
	upstreamReader := bufio.NewReader(upstream)
	for {
		head, _, body, err := readFramedMessage(downstreamReader)
		if err != nil {
			if err != io.EOF && !isClosedErr(err) {
				trace(traceEvent{Flow: flowID, Direction: "note", Host: host, Note: "client read: " + err.Error()})
			}
			return
		}
		trace(traceEvent{Flow: flowID, Direction: "C->S", Host: host, RawHead: string(head), BodyLen: len(body), Body: printable(body)})
		if _, err := upstream.Write(append(head, body...)); err != nil {
			return
		}
		respHead, _, respBody, err := readFramedMessage(upstreamReader)
		if err != nil {
			trace(traceEvent{Flow: flowID, Direction: "note", Host: host, Note: "upstream read: " + err.Error()})
			return
		}
		trace(traceEvent{Flow: flowID, Direction: "S->C", Host: host, RawHead: string(respHead), BodyLen: len(respBody), Body: printable(respBody)})
		if _, err := tlsConn.Write(append(respHead, respBody...)); err != nil {
			return
		}
		if connectionCloses(respHead) {
			return
		}
	}
}

// readFramedMessage reads one HTTP/1.x message preserving exact head bytes
// and framing the body via Content-Length or chunked transfer coding.
func readFramedMessage(reader *bufio.Reader) (head []byte, headers http.Header, body []byte, err error) {
	var buffer bytes.Buffer
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, nil, nil, err
		}
		buffer.WriteString(line)
		if line == "\r\n" || line == "\n" {
			break
		}
		if buffer.Len() > 1<<20 {
			return nil, nil, nil, fmt.Errorf("head too large")
		}
	}
	head = buffer.Bytes()
	headers = parseHeaders(head)
	if isChunked(headers) {
		body, err = readChunked(reader)
		if err != nil {
			return nil, nil, nil, err
		}
	} else if length := contentLength(headers); length > 0 {
		if length > maxBody {
			return nil, nil, nil, fmt.Errorf("body too large: %d", length)
		}
		body = make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			return nil, nil, nil, err
		}
	}
	return head, headers, body, nil
}

func parseHeaders(head []byte) http.Header {
	headers := http.Header{}
	lines := bytes.Split(head, []byte("\r\n"))
	if len(lines) == 1 {
		lines = bytes.Split(head, []byte("\n"))
	}
	for _, line := range lines[1:] {
		if len(line) == 0 {
			break
		}
		if index := bytes.IndexByte(line, ':'); index > 0 {
			headers.Add(string(line[:index]), strings.TrimSpace(string(line[index+1:])))
		}
	}
	return headers
}

func contentLength(headers http.Header) int {
	value := strings.TrimSpace(headers.Get("Content-Length"))
	if value == "" {
		return 0
	}
	length, err := strconv.Atoi(value)
	if err != nil || length < 0 {
		return 0
	}
	return length
}

func isChunked(headers http.Header) bool {
	return strings.Contains(strings.ToLower(headers.Get("Transfer-Encoding")), "chunked")
}

func readChunked(reader *bufio.Reader) ([]byte, error) {
	var out bytes.Buffer
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		out.WriteString(line)
		sizeStr := strings.TrimSpace(strings.SplitN(line, ";", 2)[0])
		size, err := strconv.ParseInt(sizeStr, 16, 64)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			trailer, err := reader.ReadString('\n')
			if err != nil {
				return nil, err
			}
			out.WriteString(trailer)
			break
		}
		if out.Len()+int(size) > maxBody+65536 {
			return nil, fmt.Errorf("chunked body too large")
		}
		if _, err := io.CopyN(&out, reader, size); err != nil {
			return nil, err
		}
		crlf := make([]byte, 2)
		if _, err := io.ReadFull(reader, crlf); err != nil {
			return nil, err
		}
		out.Write(crlf)
	}
	return out.Bytes(), nil
}

func connectionCloses(head []byte) bool {
	lower := bytes.ToLower(head)
	return bytes.Contains(lower, []byte("connection: close"))
}

func printable(body []byte) string {
	if len(body) > maxBody {
		return ""
	}
	return string(body)
}

func isClosedErr(err error) bool {
	return strings.Contains(err.Error(), "closed") || strings.Contains(err.Error(), "reset")
}

func tlsVersion(version uint16) string {
	switch version {
	case tls.VersionTLS10:
		return "1.0"
	case tls.VersionTLS11:
		return "1.1"
	case tls.VersionTLS12:
		return "1.2"
	case tls.VersionTLS13:
		return "1.3"
	default:
		return fmt.Sprintf("0x%04x", version)
	}
}
