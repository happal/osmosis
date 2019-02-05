package proxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
	"time"

	"github.com/happal/osmosis/certauth"
	"golang.org/x/net/context/ctxhttp"
	"golang.org/x/net/http2"
	"golang.org/x/sync/errgroup"
)

// Proxy allows intercepting and modifying requests.
type Proxy struct {
	server       *http.Server
	serverConfig *tls.Config

	client *http.Client

	logger *log.Logger

	ca *certauth.CertificateAuthority
}

func newHTTPClient(enableHTTP2 bool) *http.Client {
	// initialize HTTP client
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).Dial,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       15 * time.Second,
	}

	if enableHTTP2 {
		http2.ConfigureTransport(tr)
	}

	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// New initializes a proxy.
func New(address string, ca *certauth.CertificateAuthority) *Proxy {
	proxy := &Proxy{
		logger: log.New(os.Stdout, "server: ", 0),
		ca:     ca,
	}
	proxy.serverConfig = &tls.Config{
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(ch *tls.ClientHelloInfo) (*tls.Certificate, error) {
			crt, err := ca.NewCertificate(ch.ServerName, []string{ch.ServerName})
			if err != nil {
				return nil, err
			}

			tlscrt := &tls.Certificate{
				Certificate: [][]byte{
					crt.Raw,
				},
				PrivateKey: ca.Key,
			}

			return tlscrt, nil
		},
		Renegotiation: 0,
	}

	// initialize HTTP server
	proxy.server = &http.Server{
		Addr:     address,
		ErrorLog: proxy.logger,
		Handler:  proxy,
	}

	proxy.client = newHTTPClient(true)

	return proxy
}

// filterHeaders contains a list of (lower-case) header names received from the
// client which are not sent to the upstream server.
var filterHeaders = map[string]struct{}{
	"proxy-connection": struct{}{},
	"connection":       struct{}{},
}

func prepareRequest(proxyRequest *http.Request, host, scheme string) (*http.Request, error) {
	url := proxyRequest.URL
	if host != "" {
		url.Scheme = scheme
		url.Host = host
	}

	req, err := http.NewRequest(proxyRequest.Method, url.String(), proxyRequest.Body)
	if err != nil {
		return nil, err
	}

	// use Host header from received request
	req.Host = proxyRequest.Host

	for name, values := range proxyRequest.Header {
		if _, ok := filterHeaders[strings.ToLower(name)]; ok {
			// header is filtered, do not send it to the upstream server
			continue
		}
		req.Header[name] = values
	}
	return req, nil
}

// isWebsocketHandshake returns true if the request tries to initiate a websocket handshake.
func isWebsocketHandshake(req *http.Request) bool {
	upgrade := strings.ToLower(req.Header.Get("upgrade"))
	return strings.Contains(upgrade, "websocket")
}

func copyHeader(dst, src http.Header) {
	for name, values := range src {
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

// ServeProxyRequest is called for each request the proxy receives.
func (p *Proxy) ServeProxyRequest(res http.ResponseWriter, req *http.Request, forceHost, forceScheme string) {
	p.logger.Printf("%v %v", req.Method, req.URL)
	if isWebsocketHandshake(req) {
		p.HandleUpgradeRequest(req, res, forceHost, forceScheme)
		return
	}

	clientRequest, err := prepareRequest(req, forceHost, forceScheme)
	if err != nil {
		p.sendError(res, "error preparing request: %v", err)
		return
	}

	response, err := ctxhttp.Do(req.Context(), p.client, clientRequest)
	if err != nil {
		p.sendError(res, "error executing request: %v", err)
		return
	}

	if isWebsocketHandshake(req) {
		dumpResponse(response)
	}

	p.logger.Printf("%v %v -> %v", req.Method, req.URL, response.Status)

	copyHeader(res.Header(), response.Header)
	res.WriteHeader(response.StatusCode)

	_, err = io.Copy(res, response.Body)
	if err != nil {
		p.logger.Printf("error copying body: %v", err)
		return
	}

	err = response.Body.Close()
	if err != nil {
		p.logger.Printf("error closing body: %v", err)
		return
	}
}

func copyUntilError(src, dst io.ReadWriteCloser) error {
	var g errgroup.Group
	g.Go(func() error {
		_, err := io.Copy(src, dst)
		fmt.Printf("end one closed: %v\n", err)
		src.Close()
		dst.Close()
		return err
	})

	g.Go(func() error {
		_, err := io.Copy(dst, src)
		fmt.Printf("end two closed: %v\n", err)
		src.Close()
		dst.Close()
		return err
	})

	return g.Wait()
}

// HandleUpgradeRequest handles an upgraded connection (e.g. websockets).
func (p *Proxy) HandleUpgradeRequest(req *http.Request, rw http.ResponseWriter, forceHost, forceScheme string) {
	reqUpgrade := req.Header.Get("upgrade")
	p.logger.Printf("received upgrade request for %v", reqUpgrade)

	host := req.URL.Host
	if forceHost != "" {
		host = forceHost
	}

	scheme := req.URL.Scheme
	if forceHost != "" {
		scheme = forceScheme
	}

	var outgoingConn net.Conn
	var err error

	if scheme == "https" {
		outgoingConn, err = tls.Dial("tcp", host, nil)
	} else {
		outgoingConn, err = net.Dial("tcp", host)
	}

	if err != nil {
		p.sendError(rw, "connecting to %v failed: %v", host, err)
		req.Body.Close()
		return
	}

	defer outgoingConn.Close()

	p.logger.Printf("connected to %v", host)

	outReq, err := prepareRequest(req, host, scheme)
	if err != nil {
		p.sendError(rw, "preparing request to %v failed: %v", host, err)
		req.Body.Close()
		return
	}

	// put back the "Connection" header
	outReq.Header.Set("connection", req.Header.Get("connection"))

	dumpRequest(req)
	dumpRequest(outReq)

	err = outReq.Write(outgoingConn)
	if err != nil {
		p.sendError(rw, "unable to forward request to %v: %v", host, err)
		req.Body.Close()
		return
	}

	outgoingReader := bufio.NewReader(outgoingConn)
	outRes, err := http.ReadResponse(outgoingReader, outReq)
	if err != nil {
		p.sendError(rw, "unable to read response from %v: %v", host, err)
		req.Body.Close()
		return
	}

	dumpResponse(outRes)

	hj, ok := rw.(http.Hijacker)
	if !ok {
		p.sendError(rw, "switching protocols failed, incoming connection is not bidirectional")
		req.Body.Close()
		return
	}

	clientConn, _, err := hj.Hijack()
	if !ok {
		p.sendError(rw, "switching protocols failed, hijacking incoming connection failed: %v", err)
		req.Body.Close()
		return
	}
	defer clientConn.Close()

	err = outRes.Write(clientConn)
	if err != nil {
		p.logger.Printf("writing response to client failed: %v", err)
		return
	}

	p.logger.Printf("start forwarding data")
	err = copyUntilError(outgoingConn, clientConn)
	if err != nil {
		p.logger.Printf("copying data for websocket returned error: %v", err)
	}
	p.logger.Printf("connection done")
}

func writeConnectSuccess(wr io.Writer) error {
	res := http.Response{
		Proto:         "HTTP/1.0",
		ProtoMajor:    1,
		ProtoMinor:    0,
		Status:        http.StatusText(http.StatusOK),
		StatusCode:    http.StatusOK,
		ContentLength: -1,
	}

	return res.Write(wr)
}

func writeConnectError(wr io.WriteCloser, err error) {
	res := http.Response{
		Proto:         "HTTP/1.0",
		ProtoMajor:    1,
		ProtoMinor:    0,
		Status:        http.StatusText(http.StatusInternalServerError),
		StatusCode:    http.StatusInternalServerError,
		ContentLength: -1,
	}

	res.Write(wr)
	fmt.Fprintf(wr, "error: %v\n", err)
	wr.Close()
}

type fakeListener struct {
	ch   chan net.Conn
	addr net.Addr
}

func (l *fakeListener) Accept() (net.Conn, error) {
	conn, ok := <-l.ch
	if !ok {
		return nil, nil
	}
	return conn, nil
}

func (l *fakeListener) Close() error {
	close(l.ch)
	return nil
}

func (l *fakeListener) Addr() net.Addr {
	return l.addr
}

func (p *Proxy) sendError(res http.ResponseWriter, msg string, args ...interface{}) {
	p.logger.Printf(msg, args...)
	res.Header().Set("Content-Type", "text/plain")
	res.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(res, msg, args...)
}

type buffConn struct {
	*bufio.Reader
	net.Conn
}

func (b buffConn) Read(p []byte) (int, error) {
	return b.Reader.Read(p)
}

// HandleConnect makes a connection to a target host and forwards all packets.
// If an error is returned, hijacking the connection hasn't worked.
func (p *Proxy) HandleConnect(responseWriter http.ResponseWriter, req *http.Request) {
	p.logger.Printf("CONNECT %v from %v", req.URL.Host, req.RemoteAddr)
	forceHost := req.URL.Host

	hj, ok := responseWriter.(http.Hijacker)
	if !ok {
		p.sendError(responseWriter, "unable to reuse connection for CONNECT")
		return
	}

	conn, rw, err := hj.Hijack()
	if err != nil {
		p.sendError(responseWriter, "reusing connection failed: %v", err)
		return
	}
	defer conn.Close()

	rw.Flush()

	err = writeConnectSuccess(conn)
	if err != nil {
		p.logger.Printf("unable to write proxy response: %v", err)
		writeConnectError(conn, err)
		return
	}

	// try to find out if the client tries to setup TLS
	bconn := buffConn{
		Reader: bufio.NewReader(conn),
		Conn:   conn,
	}

	buf, err := bconn.Peek(1)
	if err != nil {
		p.logger.Printf("peek(1) failed: %v", err)
		return
	}

	listener := &fakeListener{
		ch:   make(chan net.Conn, 1),
		addr: conn.RemoteAddr(),
	}

	var srv *http.Server

	// TLS client hello starts with 0x16
	if buf[0] == 0x16 {
		tlsConn := tls.Server(bconn, p.serverConfig)
		defer tlsConn.Close()

		err = tlsConn.Handshake()
		if err != nil {
			p.logger.Printf("TLS handshake for %v failed: %v", req.URL.Host, err)
			return
		}

		// p.logger.Printf("TLS handshake for %v succeeded, next protocol: %v", req.URL.Host, tlsConn.ConnectionState().NegotiatedProtocol)

		listener.ch <- tlsConn

		// handle the next requests as HTTPS
		srv = &http.Server{
			ErrorLog: p.logger,
			Handler: http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
				// send all requests to the host we were told to connect to
				p.ServeProxyRequest(res, req, forceHost, "https")
			}),
		}
	} else {
		listener.ch <- bconn

		// handle the next requests as HTTP
		srv = &http.Server{
			ErrorLog: p.logger,
			Handler: http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
				// send all requests to the host we were told to connect to
				p.ServeProxyRequest(res, req, forceHost, "http")
			}),
		}
	}

	// handle all incoming requests
	err = srv.Serve(listener)
	if err != nil {
		p.logger.Printf("error serving TLS connection: %v", err)
	}
}

func dumpResponse(res *http.Response) {
	buf, err := httputil.DumpResponse(res, true)
	if err == nil {
		fmt.Printf("--------------\n%s\n--------------\n", buf)
		// fmt.Printf("body: %#v\n", res.Body)
	}
}

func dumpRequest(req *http.Request) {
	buf, err := httputil.DumpRequest(req, true)
	if err == nil {
		fmt.Printf("--------------\n%s\n--------------\n", buf)
		// fmt.Printf("body: %#v\n", req.Body)
	}
}

func (p *Proxy) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	// handle CONNECT requests for HTTPS
	if req.Method == http.MethodConnect {
		p.HandleConnect(res, req)
		return
	}

	// serve certificate for easier importing
	if req.URL.Hostname() == "proxy" && req.URL.Path == "/ca" {
		p.ServeCA(res, req)
		return
	}

	// handle all other requests
	p.ServeProxyRequest(res, req, "", "")
}

// ServeCA returns the PEM encoded CA certificate.
func (p *Proxy) ServeCA(res http.ResponseWriter, req *http.Request) {
	res.Header().Set("Content-Type", "application/x-x509-ca-cert")
	res.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	res.Header().Set("Pragma", "no-cache")
	res.Header().Set("Expires", "0")

	res.WriteHeader(http.StatusOK)
	res.Write(p.ca.CertificateAsPEM())
}

// Serve runs the proxy and answers requests.
func (p *Proxy) Serve() error {
	return p.server.ListenAndServe()
}
