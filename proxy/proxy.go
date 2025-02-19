package proxy

import (
	"context"
	"net/http"
	"net/http/httputil"

	"github.com/rs/cors"
	"go.opentelemetry.io/otel"
)

type (
	RespMiddleware func(resp *http.Response, config *HostConfig, body []byte) ([]byte, error)
	ReqMiddleware  func(req *http.Request, config *HostConfig, body []byte) ([]byte, error)
	HostMapper     func(ctx context.Context, r *http.Request) (*HostConfig, error)
	options        struct {
		hostMapper      HostMapper
		onResError      func(*http.Response, error) error
		onReqError      func(*http.Request, error)
		respMiddlewares []RespMiddleware
		reqMiddlewares  []ReqMiddleware
		transport       http.RoundTripper
	}
	HostConfig struct {
		// CorsEnabled is a flag to enable or disable CORS
		// Default: false
		CorsEnabled bool
		// CorsOptions allows to configure CORS
		// If left empty, no CORS headers will be set even when CorsEnabled is true
		CorsOptions *cors.Options
		// CookieDomain is the host under which cookies are set.
		// If left empty, no cookie domain will be set
		CookieDomain string
		// UpstreamHost is the next upstream host the proxy will pass the request to.
		// e.g. fluffy-bear-afiu23iaysd.oryapis.com
		UpstreamHost string
		// UpstreamScheme is the protocol used by the upstream service.
		UpstreamScheme string
		// TargetHost is the final target of the request. Should be the same as UpstreamHost
		// if the request is directly passed to the target service.
		TargetHost string
		// TargetScheme is the final target's scheme
		// (i.e. the scheme the target thinks it is running under)
		TargetScheme string
		// PathPrefix is a prefix that is prepended on the original host,
		// but removed before forwarding.
		PathPrefix string
		// originalHost the original hostname the request is coming from.
		// This value will be maintained internally by the proxy.
		originalHost string
		// originalScheme is the original scheme of the request.
		// This value will be maintained internally by the proxy.
		originalScheme string
	}
	Options    func(*options)
	contextKey string
)

const (
	hostConfigKey contextKey = "host config"
)

// director is a custom internal function for altering a http.Request
func director(o *options) func(*http.Request) {
	return func(r *http.Request) {
		ctx := r.Context()
		ctx, span := otel.GetTracerProvider().Tracer("").Start(ctx, "x.proxy")
		defer span.End()

		c, err := o.getHostConfig(r)
		if err != nil {
			o.onReqError(r, err)
			return
		}

		if forwardedProto := r.Header.Get("X-Forwarded-Proto"); forwardedProto != "" {
			c.originalScheme = forwardedProto
		} else if r.TLS == nil {
			c.originalScheme = "http"
		} else {
			c.originalScheme = "https"
		}
		if forwardedHost := r.Header.Get("X-Forwarded-Host"); forwardedHost != "" {
			c.originalHost = forwardedHost
		} else {
			c.originalHost = r.Host
		}

		*r = *r.WithContext(context.WithValue(ctx, hostConfigKey, c))
		headerRequestRewrite(r, c)

		var body []byte
		var cb *compressableBody

		if r.ContentLength != 0 {
			body, cb, err = readBody(r.Header, r.Body)
			if err != nil {
				o.onReqError(r, err)
				return
			}
		}

		for _, m := range o.reqMiddlewares {
			if body, err = m(r, c, body); err != nil {
				o.onReqError(r, err)
				return
			}
		}

		n, err := cb.Write(body)
		if err != nil {
			o.onReqError(r, err)
			return
		}

		r.Header.Del("Content-Length")
		r.ContentLength = int64(n)
		r.Body = cb
	}
}

// modifyResponse is a custom internal function for altering a http.Response
func modifyResponse(o *options) func(*http.Response) error {
	return func(r *http.Response) error {
		c, err := o.getHostConfig(r.Request)
		if err != nil {
			return err
		}

		if err := headerResponseRewrite(r, c); err != nil {
			return o.onResError(r, err)
		}

		body, cb, err := bodyResponseRewrite(r, c)
		if err != nil {
			return o.onResError(r, err)
		}

		for _, m := range o.respMiddlewares {
			if body, err = m(r, c, body); err != nil {
				return o.onResError(r, err)
			}
		}

		n, err := cb.Write(body)
		if err != nil {
			return o.onResError(r, err)
		}

		n, t, err := handleWebsocketResponse(n, cb, r.Body)
		if err != nil {
			return err
		}

		r.Header.Del("Content-Length")
		r.ContentLength = int64(n)
		r.Body = t
		return nil
	}
}

func WithOnError(onReqErr func(*http.Request, error), onResErr func(*http.Response, error) error) Options {
	return func(o *options) {
		o.onReqError = onReqErr
		o.onResError = onResErr
	}
}

func WithReqMiddleware(middlewares ...ReqMiddleware) Options {
	return func(o *options) {
		o.reqMiddlewares = append(o.reqMiddlewares, middlewares...)
	}
}

func WithRespMiddleware(middlewares ...RespMiddleware) Options {
	return func(o *options) {
		o.respMiddlewares = append(o.respMiddlewares, middlewares...)
	}
}

func WithTransport(t http.RoundTripper) Options {
	return func(o *options) {
		o.transport = t
	}
}

func (o *options) getHostConfig(r *http.Request) (*HostConfig, error) {
	if cached, ok := r.Context().Value(hostConfigKey).(*HostConfig); ok && cached != nil {
		return cached, nil
	}
	c, err := o.hostMapper(r.Context(), r)
	if err != nil {
		return nil, err
	}
	// cache the host config in the request context
	// this will be passed on to the request and response proxy functions
	*r = *r.WithContext(context.WithValue(r.Context(), hostConfigKey, c))
	return c, nil
}

func (o *options) beforeProxyMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// get the hostmapper configurations before the request is proxied
		c, err := o.getHostConfig(request)
		if err != nil {
			o.onReqError(request, err)
			return
		}

		// Add our Cors middleware.
		// This middleware will only trigger if the host config has cors enabled on that request.
		if c.CorsEnabled && c.CorsOptions != nil {
			cors.New(*c.CorsOptions).HandlerFunc(writer, request)
		}
		h.ServeHTTP(writer, request)
	})
}

// New creates a new Proxy
// A Proxy sets up a middleware with custom request and response modification handlers
func New(hostMapper HostMapper, opts ...Options) http.Handler {
	o := &options{
		hostMapper: hostMapper,
		onReqError: func(*http.Request, error) {},
		onResError: func(_ *http.Response, err error) error { return err },
		transport:  http.DefaultTransport,
	}

	for _, op := range opts {
		op(o)
	}

	rp := &httputil.ReverseProxy{
		Director:       director(o),
		ModifyResponse: modifyResponse(o),
		Transport:      o.transport,
	}

	return o.beforeProxyMiddleware(rp)
}
