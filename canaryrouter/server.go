package canaryrouter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/juju/errors"
	"github.com/juju/ratelimit"
	log "github.com/sirupsen/logrus"
	"github.com/tiket-libre/canary-router/canaryrouter/config"
	"github.com/tiket-libre/canary-router/canaryrouter/instrumentation"
)

const infinityDuration time.Duration = 0x7fffffffffffffff

// StatusSidecarError means there is an error when proceeding request forwarded to sidecar
const StatusSidecarError = http.StatusServiceUnavailable

// Server holds necessary components as a proxy server
type Server struct {
	version      string
	config       config.Config
	mainProxy    *httputil.ReverseProxy
	canaryProxy  *httputil.ReverseProxy
	sidecarProxy *httputil.ReverseProxy
	canaryBucket *ratelimit.Bucket
}

// NewServer initiates a new proxy server
func NewServer(config config.Config, version string) (*Server, error) {
	server := &Server{
		config:  config,
		version: version,
	}

	// === init main proxy ===
	mainProxy, err := newReverseProxy(config.MainTarget, config.MainHeaderHost)
	if err != nil {
		return nil, errors.Trace(err)
	}
	mainProxy.Transport = newTransport(config.Client.MainAndCanary)
	mainProxy.ErrorLog = stdlog.New(os.Stderr, "[proxy-main] ", stdlog.LstdFlags|stdlog.Llongfile)
	server.mainProxy = mainProxy

	// === init canary proxy ===
	canaryProxy, err := newReverseProxy(config.CanaryTarget, config.CanaryHeaderHost)
	if err != nil {
		return nil, errors.Trace(err)
	}
	canaryProxy.Transport = newTransport(config.Client.MainAndCanary)
	canaryProxy.ErrorLog = stdlog.New(os.Stderr, "[proxy-canary] ", stdlog.LstdFlags|stdlog.Llongfile)
	server.canaryProxy = canaryProxy

	// === init sidecar proxy ===
	if server.isSidecarProvided() {
		sidecarProxy, err := newReverseProxy(config.SidecarURL, "")
		if err != nil {
			return nil, errors.Trace(err)
		}
		sidecarProxy.Transport = newTransport(config.Client.Sidecar)
		sidecarProxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
			w.WriteHeader(StatusSidecarError)
			_, errWrite := w.Write([]byte(err.Error()))
			if errWrite != nil {
				log.Printf("Failed to write sidecar error body")
			}
		}
		server.sidecarProxy = sidecarProxy
	}

	if config.CircuitBreaker.RequestLimitCanary != 0 {
		server.canaryBucket = ratelimit.NewBucket(infinityDuration, int64(config.CircuitBreaker.RequestLimitCanary))
	}

	return server, nil
}

// Run initialize a new HTTP server
func (s *Server) Run() error {
	serveMux := http.NewServeMux()
	serveMux.HandleFunc("/", s.ServeHTTP)
	serveMux.HandleFunc("/application/health", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("OK")); err != nil {
			log.Printf("Failed to write health check body")
		}
	}))

	address := fmt.Sprintf("%s:%s", s.config.Server.Host, s.config.Server.Port)
	server := &http.Server{
		ReadTimeout:  time.Duration(s.config.Server.ReadTimeout) * time.Second,
		WriteTimeout: time.Duration(s.config.Server.WriteTimeout) * time.Second,
		IdleTimeout:  time.Duration(s.config.Server.IdleTimeout) * time.Second,
		Handler:      serveMux,
		Addr:         address,
	}

	log.Printf("Canary Router is now running on %s", address)

	return server.ListenAndServe()
}

// ServeHTTP handles incoming traffics via provided proxies
func (s *Server) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	s.viaProxy()(res, req)
}

// IsCanaryLimited checks if circuit breaker (canary request limiter) feature is enabled
func (s *Server) IsCanaryLimited() bool {
	return s.canaryBucket != nil
}

func (s *Server) isSidecarProvided() bool {
	return s.config.SidecarURL != ""
}

func (s *Server) viaProxy() http.HandlerFunc {
	var handlerFunc http.HandlerFunc

	if !s.isSidecarProvided() {
		handlerFunc = s.serveMain
	} else {
		handlerFunc = s.viaProxyWithSidecar()
	}

	return func(w http.ResponseWriter, req *http.Request) {
		defer func() {
			if r := recover(); r != nil {
				var err error
				switch t := r.(type) {
				case string:
					err = errors.New(t)
				case error:
					err = t
				default:
					msg := fmt.Sprintf("Unknown error: %v", r)
					err = errors.New(msg)
				}

				log.Printf("[Panic] Recovered in request handling: %v\nRequest payload: %v", r, req)
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		}()

		ctx := instrumentation.InitializeLatencyTracking(req.Context())
		req = req.WithContext(ctx)
		req.URL.Path = trimRequestPathPrefix(req.URL, s.config.TrimPrefix)

		// NOTE: Override handlerFunc if X-Canary header is provided
		xCanaryVal := req.Header.Get("X-Canary")
		xCanary, err := convertToBool(xCanaryVal)
		if err == nil {
			req = setRoutingReason(req, "Routed via X-Canary header value: %s", xCanaryVal)
			if xCanary {
				s.serveCanary(w, req)
			} else {
				s.serveMain(w, req)
			}
			return
		}

		handlerFunc(w, req)
	}
}

func (s *Server) serveMain(w http.ResponseWriter, req *http.Request) {
	defer s.recordMetricTarget(req.Context(), "main")
	log.Infof("Routed to main target: %+v", req)
	s.mainProxy.ServeHTTP(w, req)
}

func (s *Server) serveCanary(w http.ResponseWriter, req *http.Request) {
	defer s.recordMetricTarget(req.Context(), "canary")
	log.Infof("Routed to canary target: %+v", req)
	s.canaryProxy.ServeHTTP(w, req)
}

func (s *Server) callSidecar(req *http.Request) (int, error) {
	// Duplicate reader so that the original req.Body can still be used throughout
	// the request
	var bodyBuffer bytes.Buffer
	body := io.TeeReader(req.Body, &bodyBuffer)

	defer func() {
		req.Body = ioutil.NopCloser(&bodyBuffer)
	}()

	ctx := req.Context()
	outreq := req.WithContext(ctx)

	outBody, err := ioutil.ReadAll(body)
	if err != nil {
		return 0, err
	}
	outreq.Body = ioutil.NopCloser(bytes.NewReader(outBody))

	recorder := httptest.NewRecorder()
	s.sidecarProxy.ServeHTTP(recorder, outreq)

	if recorder.Code == StatusSidecarError {
		return recorder.Code, errors.New(recorder.Body.String())
	}

	return recorder.Code, nil
}

func (s *Server) viaProxyWithSidecar() http.HandlerFunc {

	return func(w http.ResponseWriter, req *http.Request) {
		if s.IsCanaryLimited() && s.canaryBucket.Available() <= 0 {
			req = setRoutingReason(req, "Canary limit reached")

			s.serveMain(w, req)
			return
		}

		statusCode, err := s.callSidecar(req)
		if err != nil {
			req = setRoutingReason(req, err.Error())
			log.Print(fmt.Errorf("Error when calling sidecar: %v", err))

			s.serveMain(w, req)
			return
		}

		switch statusCode {
		case StatusCodeMain:
			req = setRoutingReason(req, "Sidecar returns status code %d", statusCode)
			s.serveMain(w, req)
		case StatusCodeCanary:
			if s.IsCanaryLimited() && s.canaryBucket.TakeAvailable(1) == 0 {
				req = setRoutingReason(req, "Sidecar returns status code %d, but canary limit reached", statusCode)
				s.serveMain(w, req)
			} else {
				req = setRoutingReason(req, "Sidecar returns status code %d", statusCode)
				s.serveCanary(w, req)
			}
		default:
			req = setRoutingReason(req, "Sidecar returns non standard status code %d", statusCode)
			s.serveMain(w, req)
		}
	}
}

func convertToBool(boolStr string) (bool, error) {
	if boolStr == "true" || boolStr == "false" {
		return strconv.ParseBool(boolStr)
	}

	return false, errors.New("neither 'true' nor 'false'")
}

func setRoutingReason(req *http.Request, reason string, reasonArg ...interface{}) *http.Request {
	if len(reasonArg) > 0 {
		reason = fmt.Sprintf(reason, reasonArg...)
	}

	ctx, err := instrumentation.AddReasonTag(req.Context(), reason)
	if err != nil {
		log.Print(err)
		return req
	}

	return req.WithContext(ctx)
}

func (s *Server) recordMetricTarget(ctx context.Context, target string) {
	ctx, err := instrumentation.AddTargetTag(ctx, target)
	if err != nil {
		log.Errorln(err)
	}

	ctx, err = instrumentation.AddVersionTag(ctx, s.version)
	if err != nil {
		log.Errorln(err)
	}

	instrumentation.RecordLatency(ctx)
}

func trimRequestPathPrefix(reqURL *url.URL, prefix string) string {
	return strings.TrimPrefix(reqURL.Path, prefix)
}
