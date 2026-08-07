package sbpfx

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mistermoe/httpr"
)

const (
	BaseURL        = "https://www.sbp.org.pk/assets/document"
	HTTPStatusOK   = 200
	YearModulo     = 100 // For getting last 2 digits of year
	ratePrefix     = "/mark-to-market-revaluation-exchange-rate"
	pdfSignature   = "%PDF"            // PDF files begin with this magic header
	pdfContentType = "application/pdf" // Content-Type advertised for a real sheet

	// cfMitigatedHeader is set by Cloudflare when a request was challenged or
	// blocked by its bot protection. sbp.org.pk sits behind Cloudflare, and a
	// challenged request answers every path — including ones that host a real
	// sheet — with a 403 and an HTML interstitial. Reporting that as "PDF not
	// found" sends readers hunting for a URL bug that isn't there.
	cfMitigatedHeader = "Cf-Mitigated"

	// defaultUserAgent names this client to sbp.org.pk. Cloudflare challenges
	// the generic agents — "Go-http-client/1.1", curl's, and an empty one are
	// all served the interstitial — but passes a UA that identifies a project
	// and where to reach it. This is an honest identifier, not a browser
	// impersonation; no browser UA is needed to be let through.
	defaultUserAgent = "sbpfx/1.0 (+https://github.com/mistermoe/sbpfx)"

	// Connection-pool and handshake settings, matching http.DefaultTransport.
	maxIdleConns          = 100
	idleConnTimeout       = 90 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	expectContinueTimeout = 1 * time.Second
)

// http1Transport returns a transport that speaks HTTP/1.1 only.
//
// Cloudflare fingerprints the HTTP/2 handshake, and Go's fingerprint matches no
// browser, so an h2 request to sbp.org.pk is challenged whatever User-Agent it
// carries. HTTP/1.1 exposes no such fingerprint, and the same request over it
// is served the PDF.
// It is built from scratch rather than cloned from http.DefaultTransport: the
// clone inherits a TLSClientConfig that already advertises h2 in its ALPN list,
// so the server answers in h2 frames that the HTTP/1.1 code path then reads as
// a malformed response. Pinning NextProtos is what actually settles it.
func http1Transport() *http.Transport {
	return &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"http/1.1"},
		},
		MaxIdleConns:          maxIdleConns,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
	}
}

// looksLikePDF reports whether a response body is a real rate-sheet PDF.
//
// The new /assets/document host serves a 200 with a non-PDF body for missing
// sheets instead of a 404, so the status code alone is not enough to tell a
// real rate sheet from a "not found" response.
//
// The body's %PDF signature is authoritative: the Content-Type header is not
// always trustworthy (per PR review), so we never accept on the header alone.
// It is used only to corroborate — a body that passes the signature check but
// whose Content-Type explicitly advertises a non-PDF type is still rejected,
// which catches an HTML soft-404 even if its body were to start with %PDF.
func looksLikePDF(contentType string, content []byte) bool {
	if !bytes.HasPrefix(content, []byte(pdfSignature)) {
		return false
	}

	ct := strings.ToLower(contentType)
	if ct != "" && !strings.Contains(ct, pdfContentType) {
		return false
	}

	return true
}

// SBP has hosted the daily sheets under several naming schemes as it migrated
// to the /assets/document host. ratePaths returns the candidate URL paths for a
// date, in priority order:
//   - migrationOverrides: transition-window sheets with irregular names
//   - after 2026-07-02: current prefix + DD-month-YYYY (e.g. 14-july-2026)
//     AND the bare DD-Mon-YY name (e.g. 17-Jul-26) — see below
//   - after 2026-05-31: bare DD-Mon-YY (e.g. 23-Jun-26)
//   - earlier: prefix + DD-Mon-YY (e.g. 27-Aug-25)
//
// From July 2026 onward SBP posts the same daily sheet under either the long
// prefixed name or the bare DD-Mon-YY name with no discernible pattern (e.g.
// .../mark-to-market-revaluation-exchange-rate-14-july-2026.pdf on one day and
// .../17-Jul-26.pdf on another), so the current era yields both candidates and
// the caller tries each until one resolves to a real PDF.
//
// The boundaries are the last day of the previous scheme so the checks can use
// date.After; dates are day-truncated by ForDate/ForTime.
var (
	lastLegacyDay   = time.Date(2026, time.July, 2, 0, 0, 0, 0, time.UTC) // current scheme starts the next day
	lastPrefixedDay = time.Date(2026, time.May, 31, 0, 0, 0, 0, time.UTC) // bare scheme starts the next day
)

// migrationOverrides holds the transition-window sheets that SBP uploaded with
// irregular names that fit none of the date-based schemes. Keyed by YYYY-MM-DD.
var migrationOverrides = map[string]string{
	"2026-06-30": "/30-Jun-26_1.pdf",            // bare date with a _1 suffix
	"2026-07-02": ratePrefix + "-02-Jul-26.pdf", // prefix but legacy date style
}

// ratePaths builds the candidate URL paths for the exchange rate PDF of the
// given date, in priority order. Most eras resolve to a single deterministic
// path; the current era (July 2026 onward) returns both the long prefixed name
// and the bare DD-Mon-YY name because SBP uses them interchangeably.
func ratePaths(date time.Time) []string {
	if override, ok := migrationOverrides[date.Format("2006-01-02")]; ok {
		return []string{override}
	}

	// Legacy dates: DD-Mon-YY, e.g. 27-Aug-25.
	legacyDate := fmt.Sprintf("%02d-%s-%02d",
		date.Day(),
		date.Format("Jan"),
		date.Year()%YearModulo, // Last 2 digits of year
	)

	switch {
	case date.After(lastLegacyDay):
		// Current era: SBP posts the sheet under either the long prefixed name
		// (prefix + DD-month-YYYY, e.g. 14-july-2026) or the bare DD-Mon-YY
		// name (e.g. 17-Jul-26), so try both.
		longPath := fmt.Sprintf("%s-%02d-%s-%d.pdf",
			ratePrefix,
			date.Day(),
			strings.ToLower(date.Format("January")),
			date.Year(),
		)
		return []string{longPath, "/" + legacyDate + ".pdf"}
	case date.After(lastPrefixedDay):
		// Recent legacy window: bare name, e.g. /23-Jun-26.pdf.
		return []string{"/" + legacyDate + ".pdf"}
	default:
		// Older archive: prefix + DD-Mon-YY, e.g. .../rate-27-Aug-25.pdf.
		return []string{ratePrefix + "-" + legacyDate + ".pdf"}
	}
}

type Client struct {
	httpClient *httpr.Client
}

func New(options ...httpr.ClientOption) *Client {
	// Caller options come last so any of these defaults can be overridden.
	opts := append(
		[]httpr.ClientOption{
			httpr.BaseURL(BaseURL),
			httpr.HTTPClient(http.Client{Transport: http1Transport()}),
			httpr.Header("User-Agent", defaultUserAgent),
		},
		options...,
	)

	return &Client{
		httpClient: httpr.NewClient(opts...),
	}
}

// pathErrors reports the failure of every candidate path in one line.
//
// errors.Join separates its causes with newlines, which reads badly in a
// structured log, so we keep its Unwrap() []error behaviour — errors.Is and
// errors.As still see every cause — and format the causes on a single line.
type pathErrors []error

func (p pathErrors) Error() string {
	msgs := make([]string, 0, len(p))
	for _, err := range p {
		msgs = append(msgs, err.Error())
	}

	return strings.Join(msgs, "; ")
}

func (p pathErrors) Unwrap() []error { return p }

// fetchRateSheet downloads the first candidate URL that resolves to a real
// rate-sheet PDF for the given date. It returns the PDF bytes and the full URL
// that served them.
//
// When no candidate resolves, the failures of all of them are reported. Naming
// only the last candidate would hide the primary path, so a current-era failure
// would name the bare DD-Mon-YY fallback (e.g. /06-Aug-26.pdf) and read as if
// the client had built the wrong URL.
func (c *Client) fetchRateSheet(ctx context.Context, date time.Time) ([]byte, string, error) {
	var errs pathErrors
	for _, path := range ratePaths(date) {
		content, err := c.fetchPDF(ctx, path)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		return content, BaseURL + path, nil
	}

	// ratePaths never yields an empty list, so errs always has a cause here.
	// Returning an empty pathErrors would hand back a non-nil error interface
	// wrapping nothing, so guard it rather than rely on that invariant.
	if len(errs) == 0 {
		return nil, "", fmt.Errorf("no candidate paths for date: %s", date.Format("2006-01-02"))
	}

	return nil, "", errs
}

// fetchPDF issues a single GET for a candidate path and returns its body only
// if the response is a real rate-sheet PDF.
func (c *Client) fetchPDF(ctx context.Context, path string) ([]byte, error) {
	resp, err := c.httpClient.Get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("failed to download PDF: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != HTTPStatusOK {
		// A challenged request says nothing about whether the sheet exists, so
		// name the block instead of claiming the PDF is missing.
		if mitigated := resp.Header.Get(cfMitigatedHeader); mitigated != "" {
			return nil, fmt.Errorf("blocked by bot protection (cf-mitigated: %s): status %d for path: %s", mitigated, resp.StatusCode, path)
		}

		return nil, fmt.Errorf("PDF not found: status %d for path: %s", resp.StatusCode, path)
	}

	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read PDF: %w", err)
	}

	if !looksLikePDF(resp.Header.Get("Content-Type"), content) {
		return nil, fmt.Errorf("PDF not found: no rate sheet available for path: %s", path)
	}

	return content, nil
}

func (c *Client) GetExchangeRates(ctx context.Context, opts ...Option) (map[Currency]*ExchangeRate, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("failed to apply option: %w", err)
		}
	}

	date := cfg.date

	content, fullURL, err := c.fetchRateSheet(ctx, date)
	if err != nil {
		return nil, err
	}

	rates, err := parsePDFContent(content, date, fullURL)
	if err != nil {
		// The PDF exists but isn't a parseable rate sheet. SBP posted a few
		// malformed/unrelated PDFs during the June 2026 migration (e.g.
		// 2026-06-01, 03, 04, 05); surface a clear, date-tagged error rather
		// than the raw parser message so callers can distinguish it from a bug.
		return nil, fmt.Errorf("no valid rate sheet for %s (%s): %w", date.Format("2006-01-02"), fullURL, err)
	}

	return rates, nil
}

func (c *Client) GetExchangeRate(ctx context.Context, currency Currency, opts ...Option) (*ExchangeRate, error) {
	rates, err := c.GetExchangeRates(ctx, opts...)
	if err != nil {
		return nil, err
	}

	rate, exists := rates[currency]
	if !exists {
		return nil, fmt.Errorf("exchange rate for %s not found", currency)
	}

	return rate, nil
}

func (c *Client) GetUrl(opts ...Option) string {
	cfg := defaultConfig()
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			// For this method, we'll ignore errors and use default config
			// since it returns a string and can't propagate the error
			continue
		}
	}

	date := cfg.date

	// ratePaths always returns at least one candidate; [0] is the primary name.
	// In the current era SBP may instead post under the bare fallback name, so
	// callers that need the URL that actually served a sheet should use
	// GetExchangeRates (which reports the resolved URL) rather than GetUrl.
	return BaseURL + ratePaths(date)[0]
}

// DownloadRateSheet downloads the exchange rate PDF to the specified file path.
func (c *Client) DownloadRateSheet(ctx context.Context, path string, opts ...Option) error {
	cfg := defaultConfig()
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return fmt.Errorf("failed to apply option: %w", err)
		}
	}

	content, _, err := c.fetchRateSheet(ctx, cfg.date)
	if err != nil {
		return err
	}

	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", path, err)
	}
	defer file.Close()

	if _, err = file.Write(content); err != nil {
		return fmt.Errorf("failed to write PDF to file %s: %w", path, err)
	}

	return nil
}
