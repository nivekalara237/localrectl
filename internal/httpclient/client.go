package httpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/nivekalara237/localrectl/internal/util"
)

type OptionParamItem struct {
	Key   string
	Value any
}

type MultipartData struct {
	Fields map[string]string
	Files  map[string]io.Reader // filename -> reader
}

// ProgressFunc receives the bytes transferred so far and the total, -1 when it is unknown.
type ProgressFunc = util.ProgressFunc

type RequestOptions struct {
	QueryParams []OptionParamItem
	Headers     []OptionParamItem
	Body        any        // Pour JSON ou form. An io.Reader is streamed as is, never loaded in memory
	Form        url.Values // pour application/x-www-form-urlencoded
	Multipart   *MultipartData

	// ContentType overrides the Content-Type of Body: application/json by default,
	// application/octet-stream for an io.Reader.
	ContentType string
	// ContentLength is the size of an io.Reader Body. It is found alone for an *os.File and a
	// reader with a Len method; without it, the body is sent chunked.
	ContentLength int64
	// UploadProgress reports the bytes of an io.Reader Body sent so far. It restarts from 0 on
	// a retry.
	UploadProgress ProgressFunc
	// DownloadProgress reports the bytes of the response body read so far by the caller: read
	// it as a stream (io.Copy), not with io.ReadAll. The cache is skipped.
	DownloadProgress ProgressFunc
}

type Option func(*Client)

type RetryConfig struct {
	MaxRetries int
	Backoff    time.Duration // Durée de base pour l'exponentiel
	RetryOn    func(resp *http.Response, err error) bool
}

type TLSConfig struct {
	CaCertPath    string
	CaCertPemData string
}

type Client struct {
	headers        http.Header
	httpClient     *http.Client
	baseURL        *url.URL
	retry          RetryConfig
	cache          Cache
	allowSubdomain bool
	hasTlsEnabled  bool
}

func isArrayOrSlice(value any) bool {
	return isArray(value) || isSlice(value)
}

func isSlice(value any) bool {
	return reflect.ValueOf(value).Kind() == reflect.Slice
}

func isArray(value any) bool {
	return reflect.ValueOf(value).Kind() == reflect.Array
}

func NewClientHttp(opts ...Option) *Client {
	c := &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		headers:    make(http.Header),
		retry: RetryConfig{
			MaxRetries: 0,
			Backoff:    time.Second,
		},
		cache: nil,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

func WithBaseURL(rawURL string) Option {
	return func(c *Client) {
		u, err := url.Parse(rawURL)
		if err != nil {
			panic(err)
		}
		c.baseURL = u
	}
}

func WithAllowSubdomain(allowed bool) Option {
	return func(client *Client) {
		client.allowSubdomain = allowed
	}
}

func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		c.httpClient.Timeout = d
	}
}

func WithDefaultHeader(key, value string) Option {
	return func(c *Client) {
		c.headers.Set(key, value)
	}
}

func WithRetry(maxRetries int, backoff time.Duration) Option {
	return func(c *Client) {
		c.retry.MaxRetries = maxRetries
		c.retry.Backoff = backoff
	}
}

func WithCache(cache Cache) Option {
	return func(c *Client) {
		c.cache = cache
	}
}

func WithTLSCACertFile(caCertFile string) Option {
	return func(c *Client) {
		ca, err := loadCAPool(caCertFile)
		if err != nil {
			panic(err)
		}
		tr, ok := c.httpClient.Transport.(*http.Transport)
		if !ok || tr == nil {
			tr = http.DefaultTransport.(*http.Transport).Clone()
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: ca, MinVersion: tls.VersionTLS12}
		c.httpClient.Transport = tr
		c.hasTlsEnabled = true
	}
}

func WithTLSCAPemData(data string) Option {
	return func(c *Client) {
		ca, err := loadPemData(data)
		if err != nil {
			panic(err)
		}
		c.httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    ca,
				MinVersion: tls.VersionTLS12,
			},
		}
		c.hasTlsEnabled = true
	}
}

func NewRequestOption(key string, value any) OptionParamItem {
	return OptionParamItem{Key: key, Value: value}
}

func (c *Client) Do(ctx context.Context, method, path string, opts *RequestOptions) (*http.Response, error) {
	// Construction of the complete URL
	reqURL, err := c.buildURL(path, opts)
	if err != nil {
		return nil, err
	}

	useCache := c.cache != nil && method == http.MethodGet && (opts == nil || opts.DownloadProgress == nil)

	// If cache is enabled
	if useCache {
		cacheKey := reqURL.String() // inclut les query params
		if data, ok := c.cache.Get(cacheKey); ok {
			// Retourner une réponse simulée avec le corps caché
			resp := &http.Response{
				StatusCode:    http.StatusOK,
				Body:          io.NopCloser(bytes.NewReader(data)),
				Header:        make(http.Header),
				Status:        http.StatusText(http.StatusOK),
				Proto:         "HTTP.1.1/1",
				ProtoMajor:    1,
				ProtoMinor:    1,
				ContentLength: int64(len(data)),
			}
			resp.Header.Set("Content-Type", "application/octet-stream")
			return resp, nil
		}
	}

	//request body
	var body io.Reader
	var stream io.Reader
	var contentType string
	if opts != nil {
		switch {
		case opts.Multipart != nil:
			body, contentType, err = encodeMultipart(opts.Multipart)
			if err != nil {
				return nil, err
			}
		case opts.Form != nil:
			body = strings.NewReader(opts.Form.Encode())
			contentType = "application/x-www-form-urlencoded"
		case isStream(opts.Body):
			stream = opts.Body.(io.Reader)
			contentType = "application/octet-stream"
		case opts.Body != nil:
			var bs []byte
			switch v := opts.Body.(type) {
			case nil:
				bs = []byte{}
			case []byte:
				bs = v
			case string:
				bs = []byte(v)
			default:
				bs, err = json.Marshal(v)
				if err != nil {
					return nil, err
				}
			}
			body = bytes.NewReader(bs)
			contentType = "application/json"
		}
		if opts.ContentType != "" {
			contentType = opts.ContentType
		}
	}

	// Create the request
	req, err := http.NewRequestWithContext(ctx, method, reqURL.String(), body)
	if err != nil {
		return nil, err
	}
	if stream != nil {
		setStreamBody(req, stream, opts.ContentLength, opts.UploadProgress)
	}

	// Default headers
	maps.Copy(req.Header, c.headers)
	// The specific headers for request
	if opts != nil && len(opts.Headers) > 0 {
		for _, kv := range opts.Headers {
			if isArrayOrSlice(kv.Value) {
				arV := make([]string, 0, len(kv.Value.([]any)))
				for _, s := range kv.Value.([]any) {
					arV = append(arV, fmt.Sprint(s))
				}
				req.Header[kv.Key] = arV
			} else {
				req.Header[kv.Key] = []string{fmt.Sprint(kv.Value)}
			}

		}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	// A streamed transfer may last longer than the timeout of the client: only ctx stops it.
	hc := c.httpClient
	if stream != nil || (opts != nil && opts.DownloadProgress != nil) {
		transfer := *hc
		transfer.Timeout = 0
		hc = &transfer
	}

	// Execute with retry
	response, ferr := c.doWithRetry(hc, req)

	if ferr == nil && opts != nil && opts.DownloadProgress != nil {
		response.Body = util.NewProgressReader(response.Body, response.ContentLength, opts.DownloadProgress)
	}

	if useCache && ferr == nil && response.StatusCode == http.StatusOK {
		cacheKey := reqURL.String()
		bodyBytes, _ := io.ReadAll(response.Body)
		response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		c.cache.Set(cacheKey, bodyBytes, 5*time.Minute) // TTL configurable
	}

	return response, ferr
}

// isStream tells whether body is sent as a stream: an io.Reader, except the types encoded in
// memory by Do.
func isStream(body any) bool {
	switch body.(type) {
	case nil, []byte, string:
		return false
	}
	_, ok := body.(io.Reader)
	return ok
}

// setStreamBody makes r the body of req without reading it. The transport never closes r: the
// caller does. When r is an io.Seeker, GetBody rewinds it, so a retry sends it again.
func setStreamBody(req *http.Request, r io.Reader, size int64, progress ProgressFunc) {
	if size <= 0 {
		size = streamSize(r)
	}
	wrap := func() io.ReadCloser {
		if progress == nil {
			return io.NopCloser(r)
		}
		progress(0, size)
		return io.NopCloser(util.NewProgressReader(r, size, progress))
	}
	req.Body = wrap()
	req.ContentLength = size
	req.GetBody = nil
	if seeker, ok := r.(io.Seeker); ok {
		req.GetBody = func() (io.ReadCloser, error) {
			if _, err := seeker.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
			return wrap(), nil
		}
	}
}

// streamSize returns the size of r when it can be found, else -1.
func streamSize(r io.Reader) int64 {
	switch v := r.(type) {
	case interface{ Len() int }:
		return int64(v.Len())
	case *os.File:
		if info, err := v.Stat(); err == nil && info.Mode().IsRegular() {
			return info.Size()
		}
	}
	return -1
}

func encodeMultipart(data *MultipartData) (io.Reader, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// text field
	for key, val := range data.Fields {
		if err := writer.WriteField(key, val); err != nil {
			return nil, "", err
		}
	}

	for filename, reader := range data.Files {
		part, err := writer.CreateFormFile("file", filename)
		if err != nil {
			return nil, "", err
		}
		if _, err = io.Copy(part, reader); err != nil {
			return nil, "", err
		}
	}

	err := writer.Close()
	if err != nil {
		return nil, "", err
	}
	return &buf, writer.FormDataContentType(), nil
}

func hasHostname(userInput string) bool {
	u, err := url.Parse(userInput)
	if err != nil {
		return false
	}
	return u.Scheme != "" && u.Host != ""
}
func (c *Client) buildURL(path string, opts *RequestOptions) (*url.URL, error) {
	// Base
	var u *url.URL
	if c.baseURL != nil {
		base := *c.baseURL
		if hasHostname(path) {
			full, err := url.Parse(path)
			if err != nil {
				return nil, err
			}

			hostname := strings.ToLower(full.Hostname())
			baseHostname := strings.ToLower(base.Hostname())
			allowed := hostname == baseHostname ||
				(c.allowSubdomain && strings.HasSuffix(hostname, "."+baseHostname))

			if !allowed {
				return nil, fmt.Errorf("host not allowed: %s, expected: %s", hostname, baseHostname)
			}

			// RFC 3986 resolution. Handler absolute and relative
			u = base.ResolveReference(full)

			if u.Scheme != base.Scheme {
				return nil, fmt.Errorf("scheme mismatch: %s, expected: %s", u.Scheme, c.baseURL.Scheme)
			}

			if u.Port() != "" && u.Port() != base.Port() {
				return nil, fmt.Errorf("port not allowed: %s, expected %s", u.Port(), base.Port())
			}

			if u.User != nil {
				return nil, fmt.Errorf("userinfo not allowed")
			}
			// u = full
		} else {

			if !strings.HasSuffix(base.Path, "/") {
				base.Path += "/"
			}
			u = base.ResolveReference(&url.URL{Path: strings.TrimPrefix(path, "/")})
		}
	} else {
		if !hasHostname(path) {
			return nil, fmt.Errorf("invalid path as the base url is undefined: %s", path)
		}
		var err error
		u, err = url.Parse(path)
		if err != nil {
			return nil, err
		}

		if c.hasTlsEnabled && u.Scheme != "https" {
			return nil, fmt.Errorf("the path have to use https scheme")
		}
	}

	// add queries param
	if opts != nil && len(opts.QueryParams) > 0 {
		q := u.Query()
		for _, kv := range opts.QueryParams {
			if isArrayOrSlice(kv.Value) {
				for _, v := range kv.Value.([]any) {
					q.Add(kv.Key, fmt.Sprint(v))
				}
			} else {
				q.Add(kv.Key, fmt.Sprint(kv.Value))
			}
		}
		u.RawQuery = q.Encode()
	}
	return u, nil
}

func (c *Client) doWithRetry(hc *http.Client, req *http.Request) (*http.Response, error) {
	// A body is sent again through GetBody: set by http.NewRequest for an in-memory body, and by
	// setStreamBody for a seekable stream. Without it, the request is sent only once.
	hasBody := req.Body != nil && req.Body != http.NoBody
	maxRetries := c.retry.MaxRetries
	if hasBody && req.GetBody == nil {
		maxRetries = 0
	}
	var lastErr error
	var lastResponse *http.Response
	for attempt := 0; attempt <= maxRetries; attempt++ {
		reqClone := req.Clone(req.Context())
		if attempt > 0 && hasBody {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			reqClone.Body = body
		}

		resp, err := hc.Do(reqClone)
		lastResponse = resp
		if err == nil && !c.shouldRetry(resp) {
			return resp, nil
		}
		if err != nil {
			lastErr = err
			lastResponse = &http.Response{StatusCode: http.StatusInternalServerError, Status: "InternalServerError"}
		} else {
			// Lire et fermer le corps pour éviter les fuites
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("retryable status: %s", resp.Status)
		}

		if attempt < maxRetries {
			// Backoff exponentiel
			wait := c.retry.Backoff * time.Duration(1<<attempt)
			select {
			case <-time.After(wait):
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
	}
	if lastResponse != nil {
		return nil, &ClientError{
			StatusCode: lastResponse.StatusCode,
			Status:     lastResponse.Status,
			Message:    lastErr.Error(),
			Body:       nil,
		}
	}
	return nil, &ClientError{
		StatusCode: http.StatusInternalServerError,
		Status:     http.StatusText(http.StatusInternalServerError),
		Message:    lastErr.Error(),
		Body:       nil,
	}
	// return nil, fmt.Errorf("request failed after %d retries: %w", c.retry.MaxRetries, lastErr)
}

func (c *Client) shouldRetry(resp *http.Response) bool {
	if c.retry.RetryOn != nil {
		return c.retry.RetryOn(resp, nil)
	}
	// By default, retry on 429, 500, 502, 503, 504
	clientError := ClientError{StatusCode: resp.StatusCode}
	return clientError.Retryable()
}

func loadCAPool(certPath string) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system cert pool: %w", err)
	}

	pemData, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate: %w", err)
	}

	if ok := pool.AppendCertsFromPEM(pemData); !ok {
		return nil, fmt.Errorf("append CA certificate")
	}

	return pool, nil
}

func loadPemData(pemData string) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system cert pool: %w", err)
	}

	if ok := pool.AppendCertsFromPEM([]byte(pemData)); !ok {
		return nil, fmt.Errorf("append CA certificate")
	}

	return pool, nil
}
