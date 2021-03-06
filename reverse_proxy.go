// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Fork of Go's net/http/httputil/reverseproxy.go with multiple changes,
// including:
//
// * caching
// * load balancing
// * service discovery

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/pmylund/go-cache"

	"github.com/TykTechnologies/tyk/apidef"
)

const defaultUserAgent = "Tyk/" + VERSION

var ServiceCache *cache.Cache

func urlFromService(spec *APISpec) (*apidef.HostList, error) {

	doCacheRefresh := func() (*apidef.HostList, error) {
		log.Debug("--> Refreshing")
		spec.ServiceRefreshInProgress = true
		sd := ServiceDiscovery{}
		sd.Init(&spec.Proxy.ServiceDiscovery)
		data, err := sd.Target(spec.Proxy.ServiceDiscovery.QueryEndpoint)
		if err != nil {
			spec.ServiceRefreshInProgress = false
			return nil, err
		}
		// Set the cached value
		if data.Len() == 0 {
			spec.HasRun = true
			spec.ServiceRefreshInProgress = false
			log.Warning("[PROXY][SD] Service Discovery returned empty host list! Returning last good set.")

			if spec.LastGoodHostList == nil {
				log.Warning("[PROXY][SD] Last good host list is nil, returning empty set.")
				spec.LastGoodHostList = apidef.NewHostList()
			}

			return spec.LastGoodHostList, nil
		}

		ServiceCache.Set(spec.APIID, data, cache.DefaultExpiration)
		// Stash it too
		spec.LastGoodHostList = data
		spec.HasRun = true
		spec.ServiceRefreshInProgress = false
		return data, nil
	}

	// First time? Refresh the cache and return that
	if !spec.HasRun {
		log.Debug("First run! Setting cache")
		return doCacheRefresh()
	}

	// Not first run - check the cache
	cachedServiceData, found := ServiceCache.Get(spec.APIID)
	if !found {
		if spec.ServiceRefreshInProgress {
			// Are we already refreshing the cache? skip and return last good conf
			log.Debug("Cache expired! But service refresh in progress")
			return spec.LastGoodHostList, nil
		}
		// Refresh the spec
		log.Debug("Cache expired! Refreshing...")
		return doCacheRefresh()
	}

	log.Debug("Returning from cache.")
	return cachedServiceData.(*apidef.HostList), nil
}

// httpScheme matches http://* and https://*, case insensitive
var httpScheme = regexp.MustCompile(`^(?i)https?://`)

func EnsureTransport(host string) string {
	if httpScheme.MatchString(host) {
		return host
	}
	// no prototcol, assume http
	return "http://" + host
}

func nextTarget(targetData *apidef.HostList, spec *APISpec) string {
	if spec.Proxy.EnableLoadBalancing {
		log.Debug("[PROXY] [LOAD BALANCING] Load balancer enabled, getting upstream target")
		// Use a HostList
		// TODO: do better than a mutex to avoid contention
		spec.RoundRobin.Lock()
		spec.RoundRobin.SetLen(targetData.Len())
		startPos := spec.RoundRobin.Pos()
		spec.RoundRobin.Unlock()

		pos := startPos
		for {
			gotHost, err := targetData.GetIndex(pos)
			if err != nil {
				log.Error("[PROXY] [LOAD BALANCING] ", err)
				return gotHost
			}

			host := EnsureTransport(gotHost)

			if !spec.Proxy.CheckHostAgainstUptimeTests {
				return host // we don't care if it's up
			}
			if !GlobalHostChecker.IsHostDown(host) {
				return host // we do care and it's up
			}
			// if the host is down, keep trying all the rest
			// in order from where we started.
			if pos = (pos + 1) % targetData.Len(); pos == startPos {
				log.Error("[PROXY] [LOAD BALANCING] All hosts seem to be down, all uptime tests are failing!")
				return host
			}
		}

	}
	// Use standard target - might still be service data
	log.Debug("TARGET DATA:", targetData)

	gotHost, err := targetData.GetIndex(0)
	if err != nil {
		log.Error("[PROXY] ", err)
		return gotHost
	}
	return EnsureTransport(gotHost)
}

// TykNewSingleHostReverseProxy returns a new ReverseProxy that rewrites
// URLs to the scheme, host, and base path provided in target. If the
// target's path is "/base" and the incoming request was for "/dir",
// the target request will be for /base/dir. This version modifies the
// stdlib version by also setting the host to the target, this allows
// us to work with heroku and other such providers
func TykNewSingleHostReverseProxy(target *url.URL, spec *APISpec) *ReverseProxy {
	if spec.Proxy.ServiceDiscovery.UseDiscoveryService {
		log.Debug("[PROXY] Service discovery enabled")
		if ServiceCache == nil {
			log.Debug("[PROXY] Service cache initialising")
			expiry := 120
			if globalConf.ServiceDiscovery.DefaultCacheTimeout > 0 {
				expiry = globalConf.ServiceDiscovery.DefaultCacheTimeout
			}
			ServiceCache = cache.New(time.Duration(expiry)*time.Second, 15*time.Second)
		}
	}

	targetQuery := target.RawQuery
	director := func(req *http.Request) {
		hostList := spec.Proxy.StructuredTargetList
		switch {
		case spec.Proxy.ServiceDiscovery.UseDiscoveryService:
			var err error
			hostList, err = urlFromService(spec)
			if err != nil {
				log.Error("[PROXY] [SERVICE DISCOVERY] Failed target lookup: ", err)
			}
			fallthrough // implies load balancing, with replaced host list
		case spec.Proxy.EnableLoadBalancing:
			lbRemote, err := url.Parse(nextTarget(hostList, spec))
			if err != nil {
				log.Error("[PROXY] [LOAD BALANCING] Couldn't parse target URL:", err)
			} else {
				// Only replace target if everything is OK
				target = lbRemote
				targetQuery = target.RawQuery
			}
		}

		targetToUse := target

		if spec.URLRewriteEnabled && req.Context().Value(RetainHost) == true {
			log.Debug("Detected host rewrite, overriding target")
			tmpTarget, err := url.Parse(req.URL.String())
			if err != nil {
				log.Error("Failed to parse URL! Err: ", err)
			} else {
				// Specifically override with a URL rewrite
				targetToUse = tmpTarget
			}
		}

		// No override, and no load balancing? Use the existing target

		// if this is false, there was an url rewrite, thus we
		// don't want to do anything to the path - req.URL is
		// already final.
		if targetToUse == target {
			req.URL.Scheme = targetToUse.Scheme
			req.URL.Host = targetToUse.Host
			req.URL.Path = singleJoiningSlash(targetToUse.Path, req.URL.Path)
		}
		if !spec.Proxy.PreserveHostHeader {
			req.Host = targetToUse.Host
		}
		if targetQuery == "" || req.URL.RawQuery == "" {
			req.URL.RawQuery = targetQuery + req.URL.RawQuery
		} else {
			req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
		}
		if _, ok := req.Header["User-Agent"]; !ok {
			// Set Tyk's own default user agent. Without
			// this line, we would get the net/http default.
			req.Header.Set("User-Agent", defaultUserAgent)
		}
	}

	return &ReverseProxy{Director: director, TykAPISpec: spec, FlushInterval: time.Duration(globalConf.HttpServerOptions.FlushInterval) * time.Millisecond}
}

// onExitFlushLoop is a callback set by tests to detect the state of the
// flushLoop() goroutine.
var onExitFlushLoop func()

// ReverseProxy is an HTTP Handler that takes an incoming request and
// sends it to another server, proxying the response back to the
// client.
type ReverseProxy struct {
	// Director must be a function which modifies
	// the request into a new request to be sent
	// using Transport. Its response is then copied
	// back to the original client unmodified.
	Director func(*http.Request)

	// The transport used to perform proxy requests.
	// If nil, http.DefaultTransport is used.
	Transport http.RoundTripper

	// FlushInterval specifies the flush interval
	// to flush to the client while copying the
	// response body.
	// If zero, no periodic flushing is done.
	FlushInterval time.Duration

	// TLSClientConfig specifies the TLS configuration to use for 'wss'.
	// If nil, the default configuration is used.
	TLSClientConfig *tls.Config

	TykAPISpec   *APISpec
	ErrorHandler ErrorHandler
}

type TykTransporter struct {
	http.Transport
}

func (t *TykTransporter) SetTimeout(timeOut int) {
	//t.Dial.Timeout = time.Duration(timeOut) * time.Second
	t.ResponseHeaderTimeout = time.Duration(timeOut) * time.Second
}

func getMaxIdleConns() int {
	return globalConf.MaxIdleConnsPerHost
}

var TykDefaultTransport = &TykTransporter{http.Transport{
	Proxy:               http.ProxyFromEnvironment,
	MaxIdleConnsPerHost: getMaxIdleConns(),
	Dial: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).Dial,
	TLSHandshakeTimeout: 10 * time.Second,
	TLSClientConfig:     &tls.Config{},
}}

func cleanSlashes(a string) string {
	endSlash := strings.HasSuffix(a, "//")
	startSlash := strings.HasPrefix(a, "//")

	if startSlash {
		a = "/" + strings.TrimPrefix(a, "//")
	}

	if endSlash {
		a = strings.TrimSuffix(a, "//") + "/"
	}

	return a
}

func singleJoiningSlash(a, b string) string {
	a = cleanSlashes(a)
	b = cleanSlashes(b)

	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")

	switch {
	case aslash && bslash:
		log.Debug(a + b)
		return a + b[1:]
	case !aslash && !bslash:
		if len(b) > 0 {
			log.Debug(a + b)
			return a + "/" + b
		}
		log.Debug(a + b)
		return a
	}
	log.Debug(a + b)
	return a + b
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func cloneHeader(h http.Header) http.Header {
	h2 := make(http.Header, len(h))
	for k, vv := range h {
		vv2 := make([]string, len(vv))
		copy(vv2, vv)
		h2[k] = vv2
	}
	return h2
}

// Hop-by-hop headers. These are removed when sent to the backend.
// http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
var hopHeaders = []string{
	"Connection",
	"Proxy-Connection", // non-standard but still sent by libcurl and rejected by e.g. google
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",      // canonicalized version of "TE"
	"Trailer", // not Trailers per URL above; http://www.rfc-editor.org/errata_search.php?eid=4522
	"Transfer-Encoding",
	"Upgrade",
}

func (p *ReverseProxy) Init(spec *APISpec) error {
	p.ErrorHandler = ErrorHandler{BaseMiddleware: &BaseMiddleware{spec, p}}
	return nil
}

func (p *ReverseProxy) ServeHTTP(rw http.ResponseWriter, req *http.Request) *http.Response {
	return p.WrappedServeHTTP(rw, req, RecordDetail(req))
	// return nil
}

func (p *ReverseProxy) ServeHTTPForCache(rw http.ResponseWriter, req *http.Request) *http.Response {
	return p.WrappedServeHTTP(rw, req, true)
}

func (p *ReverseProxy) CheckHardTimeoutEnforced(spec *APISpec, req *http.Request) (bool, int) {
	if !spec.EnforcedTimeoutEnabled {
		return false, globalConf.ProxyDefaultTimeout
	}

	_, versionPaths, _, _ := spec.Version(req)
	found, meta := spec.CheckSpecMatchesStatus(req, versionPaths, HardTimeout)
	if found {
		intMeta := meta.(*int)
		log.Debug("HARD TIMEOUT ENFORCED: ", *intMeta)
		return true, *intMeta
	}

	return false, globalConf.ProxyDefaultTimeout
}

func (p *ReverseProxy) CheckCircuitBreakerEnforced(spec *APISpec, req *http.Request) (bool, *ExtendedCircuitBreakerMeta) {
	if !spec.CircuitBreakerEnabled {
		return false, nil
	}

	_, versionPaths, _, _ := spec.Version(req)
	found, meta := spec.CheckSpecMatchesStatus(req, versionPaths, CircuitBreaker)
	if found {
		exMeta := meta.(*ExtendedCircuitBreakerMeta)
		log.Debug("CB Enforced for path: ", *exMeta)
		return true, exMeta
	}

	return false, nil
}

func httpTransport(timeOut int, rw http.ResponseWriter, req *http.Request, p *ReverseProxy) http.RoundTripper {
	transport := TykDefaultTransport
	transport.TLSClientConfig.InsecureSkipVerify = globalConf.ProxySSLInsecureSkipVerify

	// Use the default unless we've modified the timout
	if timeOut > 0 {
		log.Debug("Setting timeout for outbound request to: ", timeOut)
		transport.DialContext = (&net.Dialer{
			Timeout:   time.Duration(timeOut) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		transport.SetTimeout(timeOut)

	}

	if IsWebsocket(req) {
		wsTransport := &WSDialer{transport, rw, p.TLSClientConfig}
		return wsTransport
	}

	return transport
}

func (p *ReverseProxy) WrappedServeHTTP(rw http.ResponseWriter, req *http.Request, withCache bool) *http.Response {
	// 1. Check if timeouts are set for this endpoint
	_, timeout := p.CheckHardTimeoutEnforced(p.TykAPISpec, req)
	transport := httpTransport(timeout, rw, req, p)

	ctx := req.Context()
	if cn, ok := rw.(http.CloseNotifier); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		notifyChan := cn.CloseNotify()
		go func() {
			select {
			case <-notifyChan:
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	// Do this before we make a shallow copy
	session := ctxGetSession(req)

	outreq := new(http.Request)
	logreq := new(http.Request)

	*outreq = *req // includes shallow copies of maps, but okay
	*logreq = *req
	// remove context data from the copies
	setContext(outreq, context.Background())
	setContext(logreq, context.Background())

	log.Debug("UPSTREAM REQUEST URL: ", req.URL)

	// We need to double set the context for the outbound request to reprocess the target
	if p.TykAPISpec.URLRewriteEnabled {
		if req.Context().Value(RetainHost) == true {
			log.Debug("Detected host rewrite, notifying director")
			setCtxValue(outreq, RetainHost, true)
		}
	}

	if req.ContentLength == 0 {
		outreq.Body = nil // Issue 16036: nil Body for http.Transport retries
	}
	outreq = outreq.WithContext(ctx)

	outreq.Header = cloneHeader(req.Header)

	p.Director(outreq)
	outreq.Close = false

	// Remove hop-by-hop headers listed in the "Connection" header.
	// See RFC 2616, section 14.10.
	if c := outreq.Header.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				outreq.Header.Del(f)
			}
		}
	}

	log.Debug("Outbound Request: ", outreq.URL.String())

	// Do not modify outbound request headers if they are WS
	if !IsWebsocket(outreq) {
		// Remove hop-by-hop headers to the backend. Especially
		// important is "Connection" because we want a persistent
		// connection, regardless of what the client sent to us.
		for _, h := range hopHeaders {
			if outreq.Header.Get(h) != "" {
				outreq.Header.Del(h)
				logreq.Header.Del(h)
			}
		}
	}

	addrs := requestAddrs(req)
	outreq.Header.Set("X-Forwarded-For", addrs)

	// Circuit breaker
	breakerEnforced, breakerConf := p.CheckCircuitBreakerEnforced(p.TykAPISpec, req)

	var res *http.Response
	var err error
	if breakerEnforced {
		log.Debug("ON REQUEST: Breaker status: ", breakerConf.CB.Ready())
		if breakerConf.CB.Ready() {
			res, err = transport.RoundTrip(outreq)
			if err != nil {
				breakerConf.CB.Fail()
			} else if res.StatusCode == 500 {
				breakerConf.CB.Fail()
			} else {
				breakerConf.CB.Success()
			}
		} else {
			p.ErrorHandler.HandleError(rw, logreq, "Service temporarily unnavailable.", 503)
			return nil
		}
	} else {
		res, err = transport.RoundTrip(outreq)
	}

	if err != nil {

		token := ctxGetAuthToken(req)

		var obfuscated string
		if len(token) > 4 {
			obfuscated = "****" + token[len(token)-4:]
		}

		var alias string
		if session != nil {
			alias = session.Alias
		}

		log.WithFields(logrus.Fields{
			"prefix":      "proxy",
			"user_ip":     addrs,
			"server_name": outreq.Host,
			"user_id":     obfuscated,
			"user_name":   alias,
			"org_id":      p.TykAPISpec.OrgID,
			"api_id":      p.TykAPISpec.APIID,
		}).Error("http: proxy error: ", err)

		if strings.Contains(err.Error(), "timeout awaiting response headers") {
			p.ErrorHandler.HandleError(rw, logreq, "Upstream service reached hard timeout.", 408)

			if p.TykAPISpec.Proxy.ServiceDiscovery.UseDiscoveryService {
				if ServiceCache != nil {
					log.Debug("[PROXY] [SERVICE DISCOVERY] Upstream host failed, refreshing host list")
					ServiceCache.Delete(p.TykAPISpec.APIID)
				}
			}
			return nil
		}
		if strings.Contains(err.Error(), "no such host") {
			p.ErrorHandler.HandleError(rw, logreq, "Upstream host lookup failed", 500)
			return nil
		}

		p.ErrorHandler.HandleError(rw, logreq, "There was a problem proxying the request", 500)
		return nil

	}

	if IsWebsocket(req) {
		return nil
	}

	inres := new(http.Response)
	if withCache {
		*inres = *res // includes shallow copies of maps, but okay

		defer res.Body.Close()

		// Buffer body data
		var bodyBuffer bytes.Buffer
		bodyBuffer2 := new(bytes.Buffer)

		p.CopyResponse(&bodyBuffer, res.Body)
		*bodyBuffer2 = bodyBuffer

		// Create new ReadClosers so we can split output
		res.Body = ioutil.NopCloser(&bodyBuffer)
		inres.Body = ioutil.NopCloser(bodyBuffer2)
	}

	ses := new(SessionState)
	if session != nil {
		ses = session
	}

	if p.TykAPISpec.ResponseHandlersActive {
		// Middleware chain handling here - very simple, but should do the trick
		err := handleResponseChain(p.TykAPISpec.ResponseChain, rw, res, req, ses)
		if err != nil {
			log.Error("Response chain failed! ", err)
		}
	}

	// We should at least copy the status code in
	inres.StatusCode = res.StatusCode
	inres.ContentLength = res.ContentLength
	p.HandleResponse(rw, res, ses)
	return inres
}

func (p *ReverseProxy) HandleResponse(rw http.ResponseWriter, res *http.Response, ses *SessionState) error {

	// Remove hop-by-hop headers listed in the
	// "Connection" header of the response.
	if c := res.Header.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				res.Header.Del(f)
			}
		}
	}

	for _, h := range hopHeaders {
		res.Header.Del(h)
	}
	defer res.Body.Close()

	// Close connections
	if globalConf.CloseConnections {
		res.Header.Set("Connection", "close")
	}

	// Add resource headers
	if ses != nil {
		// We have found a session, lets report back
		res.Header.Set("X-RateLimit-Limit", strconv.Itoa(int(ses.QuotaMax)))
		res.Header.Set("X-RateLimit-Remaining", strconv.Itoa(int(ses.QuotaRemaining)))
		res.Header.Set("X-RateLimit-Reset", strconv.Itoa(int(ses.QuotaRenews)))
	}

	copyHeader(rw.Header(), res.Header)

	rw.WriteHeader(res.StatusCode)
	p.CopyResponse(rw, res.Body)
	return nil
}

func (p *ReverseProxy) CopyResponse(dst io.Writer, src io.Reader) {
	if p.FlushInterval != 0 {
		if wf, ok := dst.(writeFlusher); ok {
			mlw := &maxLatencyWriter{
				dst:     wf,
				latency: p.FlushInterval,
				done:    make(chan bool),
			}
			go mlw.flushLoop()
			defer mlw.stop()
			dst = mlw
		}
	}

	p.copyBuffer(dst, src, nil)
}

func (p *ReverseProxy) copyBuffer(dst io.Writer, src io.Reader, buf []byte) (int64, error) {
	if len(buf) == 0 {
		buf = make([]byte, 32*1024)
	}
	var written int64
	for {
		nr, rerr := src.Read(buf)
		if rerr != nil && rerr != io.EOF && rerr != context.Canceled {
			log.WithFields(logrus.Fields{
				"prefix": "proxy",
				"org_id": p.TykAPISpec.OrgID,
				"api_id": p.TykAPISpec.APIID,
			}).Error("http: proxy error during body copy: ", rerr)
		}
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if werr != nil {
				return written, werr
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if rerr != nil {
			return written, rerr
		}
	}
}

type writeFlusher interface {
	io.Writer
	http.Flusher
}

type maxLatencyWriter struct {
	dst     writeFlusher
	latency time.Duration

	mu   sync.Mutex // protects Write + Flush
	done chan bool
}

func (m *maxLatencyWriter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dst.Write(p)
}

func (m *maxLatencyWriter) flushLoop() {
	t := time.NewTicker(m.latency)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			if onExitFlushLoop != nil {
				onExitFlushLoop()
			}
			return
		case <-t.C:
			m.mu.Lock()
			m.dst.Flush()
			m.mu.Unlock()
		}
	}
}

func (m *maxLatencyWriter) stop() { m.done <- true }
