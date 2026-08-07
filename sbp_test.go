package sbpfx_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/alecthomas/assert/v2"
	"github.com/mistermoe/httpr"
	"github.com/mistermoe/sbpfx"
	"github.com/mistermoe/sbpfx/vcr"
	"gopkg.in/dnaeon/go-vcr.v3/recorder"
)

var testMode vcr.Mode = vcr.Replay

func bootstrap(_ *testing.T, mode vcr.Mode, rec *recorder.Recorder) *sbpfx.Client {
	recorder := rec.GetDefaultClient()
	return sbpfx.New(httpr.HTTPClient(*recorder))
}

func TestGetExchangeRates(t *testing.T) {
	vcr.Test(t, testMode, bootstrap, func(t *testing.T, client *sbpfx.Client, c vcr.Cassette) {
		rate, err := client.GetExchangeRate(t.Context(), sbpfx.USD, sbpfx.ForDate("2025-08-27"))
		assert.NoError(t, err)
		assert.NotZero(t, rate)

		assert.Equal(t, sbpfx.USD, rate.Currency)
		assert.NotZero(t, rate.Ready)
		assert.NotZero(t, rate.Date)
		assert.NotZero(t, rate.URL)
	})
}

func TestDownloadRateSheet(t *testing.T) {
	vcr.Test(t, testMode, bootstrap, func(t *testing.T, client *sbpfx.Client, c vcr.Cassette) {
		// Create a temporary file path
		tempFile := t.TempDir() + "/test_rate_sheet.pdf"

		// Download the rate sheet
		err := client.DownloadRateSheet(t.Context(), tempFile, sbpfx.ForDate("2025-08-27"))
		assert.NoError(t, err)

		// Verify the file was created and has content
		stat, err := os.Stat(tempFile)
		assert.NoError(t, err, "File should be created successfully")
		assert.True(t, stat.Size() > 0, "Downloaded file should have content")
	})
}

func TestGetExchangeRatesFutureDate(t *testing.T) {
	vcr.Test(t, testMode, bootstrap, func(t *testing.T, client *sbpfx.Client, c vcr.Cassette) {
		// Try to get exchange rates for a date far in the future using string format
		rate, err := client.GetExchangeRate(t.Context(), sbpfx.USD, sbpfx.ForDate("2030-12-25"))

		// Should return an error since the PDF won't exist for future dates
		assert.Error(t, err, "Should return error for future date")
		assert.Zero(t, rate, "Rate should be nil/zero for future date")

		// The error should indicate the PDF was not found
		assert.Contains(t, err.Error(), "PDF not found", "Error should mention PDF not found")
	})
}

func TestGetExchangeRatesDualFormat(t *testing.T) {
	vcr.Test(t, testMode, bootstrap, func(t *testing.T, client *sbpfx.Client, c vcr.Cassette) {
		// From July 2026 SBP posts the daily sheet under either the long
		// prefixed name or the bare DD-Mon-YY name. In this cassette the long
		// name (mark-to-market-...-17-july-2026.pdf) soft-404s and the sheet is
		// served under the bare name (17-Jul-26.pdf). The client should fall
		// through to the bare candidate and report the URL that served it.
		rate, err := client.GetExchangeRate(t.Context(), sbpfx.USD, sbpfx.ForDate("2026-07-17"))
		assert.NoError(t, err)
		assert.NotZero(t, rate)
		assert.Equal(t, "https://www.sbp.org.pk/assets/document/17-Jul-26.pdf", rate.URL)
	})
}

func TestGetExchangeRatesMalformedSheet(t *testing.T) {
	vcr.Test(t, testMode, bootstrap, func(t *testing.T, client *sbpfx.Client, c vcr.Cassette) {
		// During the June 2026 migration SBP posted a few PDFs that exist but
		// are not parseable rate sheets (e.g. 2026-06-01). The client should
		// return a clear, date-tagged error rather than the raw parser message.
		_, err := client.GetExchangeRates(t.Context(), sbpfx.ForDate("2026-06-01"))
		assert.Error(t, err, "malformed sheet should error")
		assert.Contains(t, err.Error(), "no valid rate sheet", "error should be the domain error")
	})
}

func TestForDateAndForTime(t *testing.T) {
	vcr.Test(t, testMode, bootstrap, func(t *testing.T, client *sbpfx.Client, c vcr.Cassette) {
		// Test ForDate with string format
		rateFromString, err1 := client.GetExchangeRate(t.Context(), sbpfx.USD, sbpfx.ForDate("2025-08-27"))

		// Test ForTime with time.Time
		specificTime := time.Date(2025, 8, 27, 0, 0, 0, 0, time.UTC)
		rateFromTime, err2 := client.GetExchangeRate(t.Context(), sbpfx.USD, sbpfx.ForTime(specificTime))

		// Both should work and return the same data (assuming the date exists)
		if err1 == nil && err2 == nil {
			assert.Equal(t, rateFromString.Currency, rateFromTime.Currency)
			assert.Equal(t, rateFromString.Ready, rateFromTime.Ready)
			assert.Equal(t, rateFromString.Date.Format("2006-01-02"), rateFromTime.Date.Format("2006-01-02"))
			assert.Equal(t, rateFromString.URL, rateFromTime.URL)
		}

		// At minimum, both should have the same error status
		assert.Equal(t, err1 != nil, err2 != nil, "Both ForDate and ForTime should behave consistently")
	})
}

func TestGetUrlDateFormats(t *testing.T) {
	client := sbpfx.New()

	const base = "https://www.sbp.org.pk/assets/document"

	tests := []struct {
		date string
		want string
	}{
		// Older archive (through 2026-05-31): prefix + DD-Mon-YY
		{"2025-08-27", base + "/mark-to-market-revaluation-exchange-rate-27-Aug-25.pdf"},
		{"2026-05-31", base + "/mark-to-market-revaluation-exchange-rate-31-May-26.pdf"},
		// Recent legacy window (2026-06-01 through 2026-07-02): bare DD-Mon-YY
		{"2026-06-01", base + "/01-Jun-26.pdf"},
		{"2026-06-23", base + "/23-Jun-26.pdf"},
		{"2026-06-29", base + "/29-Jun-26.pdf"},
		// Transition-window overrides (irregular names)
		{"2026-06-30", base + "/30-Jun-26_1.pdf"},
		{"2026-07-02", base + "/mark-to-market-revaluation-exchange-rate-02-Jul-26.pdf"},
		// Current format (2026-07-03 onward): prefix + DD-month-YYYY
		{"2026-07-03", base + "/mark-to-market-revaluation-exchange-rate-03-july-2026.pdf"},
		{"2026-07-07", base + "/mark-to-market-revaluation-exchange-rate-07-july-2026.pdf"},
	}

	for _, tt := range tests {
		got := client.GetUrl(sbpfx.ForDate(tt.date))
		assert.Equal(t, tt.want, got, "GetUrl for %s", tt.date)
	}
}

func TestForDateInvalidFormat(t *testing.T) {
	client := sbpfx.New()

	// Test with invalid date format - should use default date and not crash
	url1 := client.GetUrl(sbpfx.ForDate("invalid-date"))
	url2 := client.GetUrl() // default date

	// The URLs should be different only if the invalid date somehow got parsed differently
	// But more importantly, the function shouldn't crash
	assert.True(t, len(url1) > 0, "Should still generate a URL even with invalid date")
	assert.True(t, len(url2) > 0, "Should generate a URL with default date")
}

// currentEraPaths are the two candidate paths sbpfx tries for a current-era
// date: the long prefixed name first, then the bare DD-Mon-YY fallback.
var currentEraPaths = []string{
	"/mark-to-market-revaluation-exchange-rate-06-august-2026.pdf",
	"/06-Aug-26.pdf",
}

// serveStatus returns a client pointed at a stub host that answers every
// request with the given status and headers, plus the paths it was asked for.
func serveStatus(t *testing.T, status int, headers map[string]string) (*sbpfx.Client, *[]string) {
	t.Helper()

	var requested []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		for k, v := range headers {
			w.Header().Set(k, v)
		}

		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)

	return sbpfx.New(httpr.BaseURL(server.URL)), &requested
}

func TestFetchErrorNamesEveryCandidatePath(t *testing.T) {
	client, requested := serveStatus(t, http.StatusNotFound, nil)

	_, err := client.GetExchangeRates(t.Context(), sbpfx.ForDate("2026-08-06"))
	assert.Error(t, err, "missing sheet should error")
	assert.Equal(t, currentEraPaths, *requested, "both candidates should be tried")

	// Reporting only the last candidate hides the primary path and reads as if
	// the client had built the wrong URL.
	for _, path := range currentEraPaths {
		assert.Contains(t, err.Error(), path, "error should name candidate %s", path)
	}

	assert.Contains(t, err.Error(), "PDF not found", "a real 404 is still a missing PDF")
}

func TestFetchErrorReportsBotProtectionBlock(t *testing.T) {
	// sbp.org.pk sits behind Cloudflare: a challenged request gets a 403 and an
	// HTML interstitial for every path, including ones that host a real sheet.
	client, requested := serveStatus(t, http.StatusForbidden, map[string]string{
		"Cf-Mitigated": "challenge",
		"Content-Type": "text/html; charset=UTF-8",
	})

	_, err := client.GetExchangeRates(t.Context(), sbpfx.ForDate("2026-08-06"))
	assert.Error(t, err, "a blocked request should error")
	assert.Equal(t, currentEraPaths, *requested, "both candidates should be tried")

	assert.Contains(t, err.Error(), "blocked by bot protection", "error should name the block")
	assert.NotContains(t, err.Error(), "PDF not found", "a block says nothing about the sheet existing")
}
