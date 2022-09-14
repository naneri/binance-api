package binance

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgrr/http2"
	"github.com/google/go-querystring/query"
	"github.com/pkg/errors"
	"github.com/segmentio/encoding/json"
	"github.com/valyala/fasthttp"
	"github.com/xenking/bytebufferpool"
)

type RestClient interface {
	Do(method, endpoint string, data interface{}, sign bool, stream bool) ([]byte, error)

	SetWindow(window int)
	UsedWeight() map[string]int
	OrderCount() map[string]int
	RetryAfter() int
}

const DefaultResponseWindow = 5000

func NewRestClient(key, secret string) RestClient {
	return &RestClientImplementation{
		apikey: key,
		hmac:   hmac.New(sha256.New, s2b(secret)),
		client: newHTTPClient(),
		window: DefaultResponseWindow,
	}
}

func NewRestClientHTTP2(key, secret string) (RestClient, error) {
	c, err := newHTTP2Client()

	return &RestClientImplementation{
		apikey: key,
		hmac:   hmac.New(sha256.New, s2b(secret)),
		client: c,
		window: DefaultResponseWindow,
	}, err
}

type RestClientConfig struct {
	APIKey         string
	APISecret      string
	HTTPClient     *fasthttp.HostClient
	ResponseWindow int
}

func (c RestClientConfig) defaults() RestClientConfig {
	if c.HTTPClient == nil {
		c.HTTPClient = newHTTPClient()
	}
	if c.ResponseWindow == 0 {
		c.ResponseWindow = DefaultResponseWindow
	}

	return c
}

func NewCustomRestClient(config RestClientConfig) RestClient {
	c := config.defaults()

	return &RestClientImplementation{
		apikey: c.APIKey,
		hmac:   hmac.New(sha256.New, s2b(c.APISecret)),
		client: c.HTTPClient,
		window: c.ResponseWindow,
	}
}

// RestClientImplementation represents the actual HTTP RestClient, that is being used to interact with binance API server
type RestClientImplementation struct {
	apikey     string
	hmac       hash.Hash
	client     *fasthttp.HostClient
	window     int
	usedWeight sync.Map
	orderCount sync.Map
	retryAfter atomic.Value
}

const (
	DefaultSchema  = "https"
	HeaderTypeJSON = "application/json"
	HeaderTypeForm = "application/x-www-form-urlencoded"
	HeaderAccept   = "Accept"
	HeaderAPIKey   = "X-MBX-APIKEY" //nolint:gosec
)

var (
	HeaderUsedWeight = []byte("X-Mbx-Used-Weight-")
	HeaderOrderCount = []byte("X-Mbx-Order-Count-")
	HeaderRetryAfter = []byte("Retry-After")
)

func newHTTP2Client() (*fasthttp.HostClient, error) {
	hc := &fasthttp.HostClient{
		NoDefaultUserAgentHeader:      true, // Don't send: User-Agent: fasthttp
		DisableHeaderNamesNormalizing: false,
		DisablePathNormalizing:        false,
		IsTLS:                         true,
		Name:                          DefaultUserAgent,
		Addr:                          BaseHost,
	}

	if err := http2.ConfigureClient(hc, http2.ClientOpts{}); err != nil {
		return nil, errors.Wrapf(err, "%s doesn't support http/2", hc.Addr)
	}

	return hc, nil
}

// newHTTPClient create fasthttp.HostClient with default settings
func newHTTPClient() *fasthttp.HostClient {
	return &fasthttp.HostClient{
		NoDefaultUserAgentHeader:      true, // Don't send: User-Agent: fasthttp
		DisableHeaderNamesNormalizing: false,
		DisablePathNormalizing:        false,
		IsTLS:                         true,
		Name:                          DefaultUserAgent,
		Addr:                          BaseHost,
	}
}

// Do invokes the given API command with the given data
// sign indicates whether the api call should be done with signed payload
// stream indicates if the request is stream related
func (c *RestClientImplementation) Do(method, endpoint string, data interface{}, sign, stream bool) ([]byte, error) {
	// Convert the given data to urlencoded format
	values, err := query.Values(data)
	if err != nil {
		return nil, err
	}
	encoded := values.Encode()
	var pb []byte
	if sign {
		pb = make([]byte, len(encoded), len(encoded)+116)
	} else {
		pb = make([]byte, len(encoded))
	}
	copy(pb, encoded)
	// Signed requests require the additional timestamp, window size and signature of the payload
	// Remark: This is done only to routes with actual data
	if sign {
		buf := bytebufferpool.Get()
		pb = append(pb, "&timestamp="...)                                                               //nolint:makezero
		pb = append(pb, strconv.AppendInt(buf.B, time.Now().UnixNano()/int64(time.Millisecond), 10)...) //nolint:makezero

		buf.Reset()
		pb = append(pb, "&recvWindow="...)                                //nolint:makezero
		pb = append(pb, strconv.AppendInt(buf.B, int64(c.window), 10)...) //nolint:makezero
		_, err = c.hmac.Write(pb)
		if err != nil {
			return nil, err
		}
		pb = append(pb, "&signature="...) //nolint:makezero
		sum := c.hmac.Sum(nil)
		enc := make([]byte, len(sum)*2)
		hex.Encode(enc, sum)
		pb = append(pb, enc...) //nolint:makezero
		c.hmac.Reset()
		bytebufferpool.Put(buf)
	}

	var b strings.Builder
	b.WriteString(endpoint)

	// Construct the http request
	// Remark: GET requests payload is as a query parameters
	// POST requests payload is given as a body
	req := fasthttp.AcquireRequest()
	req.Header.SetHost(BaseHost)
	req.URI().SetScheme(DefaultSchema)
	req.Header.SetMethod(method)

	if method == fasthttp.MethodGet {
		b.Grow(len(pb) + 1)
		b.WriteByte('?')
		b.Write(pb)
	} else {
		req.Header.SetContentType(HeaderTypeForm)
		req.SetBody(pb)
	}
	req.SetRequestURI(b.String())

	if sign || stream {
		req.Header.Add(HeaderAPIKey, c.apikey)
	}

	req.Header.Add(HeaderAccept, HeaderTypeJSON)
	resp := fasthttp.AcquireResponse()

	err = c.client.Do(req, resp)
	if err != nil {
		return nil, err
	}
	fasthttp.ReleaseRequest(req)

	body := append([]byte{}, resp.Body()...)

	pb = append(pb[:0], resp.Header.Header()...)
	status := resp.StatusCode()
	fasthttp.ReleaseResponse(resp)

	if h := getHeader(pb, HeaderUsedWeight); h != nil {
		interval, val, parseErr := parseInterval(h)
		if parseErr == nil {
			c.usedWeight.Store(interval, val)
		}
	}
	if h := getHeader(pb, HeaderOrderCount); h != nil {
		interval, val, parseErr := parseInterval(h)
		if parseErr == nil {
			c.orderCount.Store(interval, val)
		}
	}

	if status != fasthttp.StatusOK {
		if h := getHeader(pb, HeaderRetryAfter); len(h) > 2 {
			retry, parseErr := fasthttp.ParseUint(h[2:])
			if parseErr == nil {
				c.retryAfter.Store(retry)
			}
		}

		apiErr := &APIError{}
		err = json.Unmarshal(body, apiErr)
		if err != nil {
			return nil, err
		}

		return nil, apiErr
	}

	return body, err
}

func parseInterval(header []byte) (interval string, value int, err error) {
	parseValue := false
	for i := 0; i < len(header); i++ {
		c := header[i]
		switch {
		case c == ':', c == ' ':
			parseValue = true

			continue
		case parseValue:
			value, err = fasthttp.ParseUint(header[i:])

			return
		case c >= '0' && c <= '9':
			continue
		}
		interval = string(header[:i+1])
	}

	return
}

func getHeader(header, search []byte) []byte {
	if len(header) == 0 {
		return nil
	}
	if idx := bytes.Index(header, search); idx > 0 {
		for i := idx + len(search); i < len(header); i++ {
			if header[i] == '\n' {
				return header[idx+len(search) : i-1]
			}
		}
	}

	return nil
}

// SetWindow to specify response time window in milliseconds
func (c *RestClientImplementation) SetWindow(window int) {
	c.window = window
}

func (c *RestClientImplementation) UsedWeight() map[string]int {
	res := make(map[string]int)
	c.usedWeight.Range(func(k, v interface{}) bool {
		key, ok1 := k.(string)
		value, ok2 := v.(int)
		if ok1 && ok2 {
			res[key] = value
		}

		return true
	})

	return res
}

func (c *RestClientImplementation) OrderCount() map[string]int {
	res := make(map[string]int)
	c.usedWeight.Range(func(k, v interface{}) bool {
		key, ok1 := k.(string)
		value, ok2 := v.(int)
		if ok1 && ok2 {
			res[key] = value
		}

		return true
	})

	return res
}

func (c *RestClientImplementation) RetryAfter() int {
	retry, ok := c.retryAfter.Load().(int)
	if ok {
		return retry
	}

	return 0
}
