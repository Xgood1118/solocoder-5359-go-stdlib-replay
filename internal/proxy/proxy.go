package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"example.com/replay/internal/session"
)

type Config struct {
	ListenAddr  string
	OutputPath  string
	MaxBodySize int64
	Exclude     []string
	IncludeOnly []string
}

type RecordingProxy struct {
	config    Config
	session   *session.Session
	mu        sync.Mutex
	client    *http.Client
	certCache *CertCache
}

func NewRecordingProxy(cfg Config) *RecordingProxy {
	cc, err := NewCertCache()
	if err != nil {
		fmt.Printf("警告: 证书缓存初始化失败，HTTPS 录制将不可用: %v\n", err)
	}
	return &RecordingProxy{
		config:    cfg,
		session:   session.NewSession(),
		certCache: cc,
		client: &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (p *RecordingProxy) Start() error {
	server := &http.Server{
		Addr: p.config.ListenAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				p.handleConnect(w, r)
			} else {
				p.handleHTTP(w, r)
			}
		}),
	}

	fmt.Printf("录制代理已启动，监听 %s\n", p.config.ListenAddr)
	fmt.Printf("录制数据将保存到: %s\n", p.config.OutputPath)
	if err := server.ListenAndServe(); err != nil {
		return fmt.Errorf("代理服务启动失败: %w", err)
	}
	return nil
}

func (p *RecordingProxy) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return session.Save(p.session, p.config.OutputPath)
}

func (p *RecordingProxy) shouldRecord(targetURL string) bool {
	for _, excl := range p.config.Exclude {
		if strings.Contains(targetURL, excl) {
			return false
		}
	}
	if len(p.config.IncludeOnly) > 0 {
		matched := false
		for _, incl := range p.config.IncludeOnly {
			if strings.Contains(targetURL, incl) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func (p *RecordingProxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	targetURL := r.URL.String()
	if !r.URL.IsAbs() {
		targetURL = "http://" + r.Host + r.URL.String()
	}

	if !p.shouldRecord(targetURL) {
		p.forwardDirect(w, r)
		return
	}

	requestTimestamp := session.NowRFC3339()
	reqBody, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	outReq, err := http.NewRequestWithContext(ctx, r.Method, targetURL, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "创建转发请求失败", http.StatusBadGateway)
		return
	}
	for k, v := range r.Header {
		if !isHopHeader(k) {
			outReq.Header[k] = v
		}
	}

	resp, err := p.client.Do(outReq)
	if err != nil {
		http.Error(w, "转发请求失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	responseTimestamp := session.NowRFC3339()
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "读取响应体失败", http.StatusBadGateway)
		return
	}

	for k, v := range resp.Header {
		if !isHopHeader(k) {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)

	p.record(targetURL, r, reqBody, resp, respBody, requestTimestamp, responseTimestamp)
}

func (p *RecordingProxy) forwardDirect(w http.ResponseWriter, r *http.Request) {
	targetURL := r.URL.String()
	if !r.URL.IsAbs() {
		targetURL = "http://" + r.Host + r.URL.String()
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "创建转发请求失败", http.StatusBadGateway)
		return
	}
	for k, v := range r.Header {
		if !isHopHeader(k) {
			outReq.Header[k] = v
		}
	}

	resp, err := p.client.Do(outReq)
	if err != nil {
		http.Error(w, "转发请求失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, v := range resp.Header {
		if !isHopHeader(k) {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (p *RecordingProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	if p.certCache == nil {
		p.handleConnectTunnel(w, r)
		return
	}

	if !p.shouldRecord(r.Host) {
		p.handleConnectTunnel(w, r)
		return
	}

	p.handleConnectMITM(w, r)
}

func (p *RecordingProxy) handleConnectTunnel(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, err = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if err != nil {
		clientConn.Close()
		return
	}
	serverConn, err := net.Dial("tcp", r.Host)
	if err != nil {
		clientConn.Close()
		return
	}
	go io.Copy(clientConn, serverConn)
	go io.Copy(serverConn, clientConn)
}

func (p *RecordingProxy) handleConnectMITM(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	_, err = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if err != nil {
		clientConn.Close()
		return
	}

	host := r.Host
	hostWithoutPort, port, splitErr := net.SplitHostPort(host)
	if splitErr != nil {
		hostWithoutPort = host
		port = "443"
	}

	tlsCert, err := p.certCache.Get(host)
	if err != nil {
		fmt.Printf("警告: 无法为 %s 生成证书: %v，降级为隧道模式\n", host, err)
		serverConn, dialErr := net.Dial("tcp", host)
		if dialErr != nil {
			clientConn.Close()
			return
		}
		go io.Copy(clientConn, serverConn)
		go io.Copy(serverConn, clientConn)
		return
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*tlsCert},
		MinVersion:   tls.VersionTLS12,
	}

	tlsConn := tls.Server(clientConn, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		fmt.Printf("警告: 与客户端 TLS 握手失败 (%s): %v\n", host, err)
		tlsConn.Close()
		return
	}

	serverConn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 10 * time.Second},
		"tcp",
		net.JoinHostPort(hostWithoutPort, port),
		&tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
		},
	)
	if err != nil {
		fmt.Printf("警告: 与服务器 TLS 连接失败 (%s): %v\n", host, err)
		tlsConn.Close()
		return
	}

	p.serveHTTPS(tlsConn, serverConn, hostWithoutPort)
}

func (p *RecordingProxy) serveHTTPS(clientConn *tls.Conn, serverConn *tls.Conn, host string) {
	defer clientConn.Close()
	defer serverConn.Close()

	reader := bufio.NewReader(clientConn)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}

		targetURL := "https://" + host + req.URL.String()
		req.URL, _ = url.Parse(targetURL)

		reqBody, err := ioutil.ReadAll(req.Body)
		if err != nil {
			return
		}
		req.Body.Close()

		if !p.shouldRecord(targetURL) {
			_, writeErr := serverConn.Write(buildRawRequest(req, reqBody))
			if writeErr != nil {
				return
			}
			resp, readErr := http.ReadResponse(bufio.NewReader(serverConn), req)
			if readErr != nil {
				return
			}
			resp.Body.Close()
			writeResponse(clientConn, resp, nil)
			continue
		}

		requestTimestamp := session.NowRFC3339()

		outReq, err := http.NewRequest(req.Method, targetURL, bytes.NewReader(reqBody))
		if err != nil {
			return
		}
		for k, v := range req.Header {
			if !isHopHeader(k) {
				outReq.Header[k] = v
			}
		}

		resp, err := p.client.Do(outReq)
		if err != nil {
			errResp := buildErrorResponse(err)
			writeResponse(clientConn, errResp, nil)
			return
		}

		responseTimestamp := session.NowRFC3339()
		respBody, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return
		}

		writeResponse(clientConn, resp, respBody)

		p.recordHTTPS(targetURL, req, reqBody, resp, respBody, requestTimestamp, responseTimestamp)
	}
}

func buildRawRequest(req *http.Request, body []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("%s %s HTTP/1.1\r\n", req.Method, req.URL.RequestURI()))
	buf.WriteString(fmt.Sprintf("Host: %s\r\n", req.URL.Host))
	for k, v := range req.Header {
		if !isHopHeader(k) {
			for _, vv := range v {
				buf.WriteString(fmt.Sprintf("%s: %s\r\n", k, vv))
			}
		}
	}
	buf.WriteString("\r\n")
	buf.Write(body)
	return buf.Bytes()
}

func writeResponse(clientConn net.Conn, resp *http.Response, body []byte) {
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("HTTP/1.1 %s\r\n", resp.Status))
	for k, v := range resp.Header {
		if !isHopHeader(k) {
			for _, vv := range v {
				buf.WriteString(fmt.Sprintf("%s: %s\r\n", k, vv))
			}
		}
	}
	buf.WriteString("\r\n")
	if body != nil {
		buf.Write(body)
	}
	clientConn.Write(buf.Bytes())
}

func buildErrorResponse(err error) *http.Response {
	return &http.Response{
		StatusCode: http.StatusBadGateway,
		Status:     "502 Bad Gateway",
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       ioutil.NopCloser(strings.NewReader(fmt.Sprintf("代理转发失败: %v", err))),
	}
}

func (p *RecordingProxy) recordHTTPS(
	targetURL string,
	req *http.Request,
	reqBody []byte,
	resp *http.Response,
	respBody []byte,
	reqTimestamp string,
	respTimestamp string,
) {
	reqContentType := req.Header.Get("Content-Type")
	queryMap := make(map[string]string)
	if u, err := url.Parse(targetURL); err == nil {
		for k, v := range u.Query() {
			if len(v) > 0 {
				queryMap[k] = v[0]
			}
		}
	}

	reqRecord := &session.RequestRecord{
		URL:       targetURL,
		Method:    req.Method,
		Headers:   session.HeadersToMap(req.Header),
		Query:     queryMap,
		Body:      session.NewBodyContent(reqBody, reqContentType, p.config.MaxBodySize),
		Timestamp: reqTimestamp,
	}

	respContentType := resp.Header.Get("Content-Type")
	respRecord := &session.ResponseRecord{
		Status:    resp.StatusCode,
		Headers:   session.HeadersToMap(resp.Header),
		Body:      session.NewBodyContent(respBody, respContentType, p.config.MaxBodySize),
		Timestamp: respTimestamp,
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.session.Requests = append(p.session.Requests, &session.Entry{
		Request:  reqRecord,
		Response: respRecord,
	})
	_ = session.Save(p.session, p.config.OutputPath)
}

func (p *RecordingProxy) record(
	targetURL string,
	r *http.Request,
	reqBody []byte,
	resp *http.Response,
	respBody []byte,
	reqTimestamp string,
	respTimestamp string,
) {
	reqContentType := r.Header.Get("Content-Type")
	queryMap := make(map[string]string)
	if u, err := url.Parse(targetURL); err == nil {
		for k, v := range u.Query() {
			if len(v) > 0 {
				queryMap[k] = v[0]
			}
		}
	}

	reqRecord := &session.RequestRecord{
		URL:       targetURL,
		Method:    r.Method,
		Headers:   session.HeadersToMap(r.Header),
		Query:     queryMap,
		Body:      session.NewBodyContent(reqBody, reqContentType, p.config.MaxBodySize),
		Timestamp: reqTimestamp,
	}

	respContentType := resp.Header.Get("Content-Type")
	respRecord := &session.ResponseRecord{
		Status:    resp.StatusCode,
		Headers:   session.HeadersToMap(resp.Header),
		Body:      session.NewBodyContent(respBody, respContentType, p.config.MaxBodySize),
		Timestamp: respTimestamp,
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.session.Requests = append(p.session.Requests, &session.Entry{
		Request:  reqRecord,
		Response: respRecord,
	})
	_ = session.Save(p.session, p.config.OutputPath)
}

var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func isHopHeader(header string) bool {
	h := strings.ToLower(header)
	for _, hh := range hopHeaders {
		if strings.ToLower(hh) == h {
			return true
		}
	}
	return false
}
