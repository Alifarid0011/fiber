package paginate

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/internal/apptest"
	"github.com/stretchr/testify/require"
)

// --- Config tests ---

func Test_ConfigDefault(t *testing.T) {
	t.Parallel()

	cfg := configDefault()
	require.Equal(t, "page", cfg.PageKey)
	require.Equal(t, 1, cfg.DefaultPage)
	require.Equal(t, "limit", cfg.LimitKey)
	require.Equal(t, 10, cfg.DefaultLimit)
	require.Equal(t, DefaultMaxLimit, cfg.MaxLimit)
}

func Test_ConfigOverride(t *testing.T) {
	t.Parallel()

	cfg := configDefault(Config{
		PageKey:      "p",
		LimitKey:     "l",
		DefaultPage:  5,
		DefaultLimit: 50,
	})
	require.Equal(t, "p", cfg.PageKey)
	require.Equal(t, "l", cfg.LimitKey)
	require.Equal(t, 5, cfg.DefaultPage)
	require.Equal(t, 50, cfg.DefaultLimit)
}

func Test_ConfigDefaultCursorKey(t *testing.T) {
	t.Parallel()

	cfg := configDefault()
	require.Equal(t, "cursor", cfg.CursorKey)
}

func Test_ConfigOverrideCursorKey(t *testing.T) {
	t.Parallel()

	cfg := configDefault(Config{
		CursorKey:   "after",
		CursorParam: "starting_after",
	})
	require.Equal(t, "after", cfg.CursorKey)
	require.Equal(t, "starting_after", cfg.CursorParam)
}

func Test_ConfigNegativeDefaults(t *testing.T) {
	t.Parallel()

	cfg := configDefault(Config{
		DefaultPage:  -1,
		DefaultLimit: -1,
	})
	require.Equal(t, 1, cfg.DefaultPage)
	require.Equal(t, 10, cfg.DefaultLimit)
}

// --- PageInfo tests ---

func Test_SortOrderFromString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected SortOrder
	}{
		{"asc", ASC},
		{"desc", DESC},
		{"DESC", DESC},
		{"Desc", DESC},
		{"invalid", ASC},
		{"", ASC},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, SortOrderFromString(tt.input))
		})
	}
}

func Test_PageInfoStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		pageInfo PageInfo
		expected int
	}{
		{"Page 1, limit 10", PageInfo{Page: 1, Limit: 10}, 0},
		{"Page 1, limit 1", PageInfo{Page: 1, Limit: 1}, 0},
		{"Page 2, limit 10", PageInfo{Page: 2, Limit: 10}, 10},
		{"Page 2, limit 1", PageInfo{Page: 2, Limit: 1}, 1},
		{"Page 3, limit 20", PageInfo{Page: 3, Limit: 20}, 40},
		{"With offset", PageInfo{Page: 2, Limit: 10, Offset: 25}, 25},
		{"Zero page", PageInfo{Page: 0, Limit: 10}, 0},
		{"Overflow clamps to MaxInt", PageInfo{Page: int(^uint(0) >> 1), Limit: 100}, int(^uint(0) >> 1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, tt.pageInfo.Start())
		})
	}
}

func Test_PageInfoSortBy(t *testing.T) {
	t.Parallel()

	p := NewPageInfo(1, 10, 0, nil)
	p.SortBy("name", ASC).SortBy("date", DESC)

	require.Len(t, p.Sort, 2)
	require.Equal(t, "name", p.Sort[0].Field)
	require.Equal(t, ASC, p.Sort[0].Order)
	require.Equal(t, "date", p.Sort[1].Field)
	require.Equal(t, DESC, p.Sort[1].Order)
}

// Test_PageInfoPageURLs covers PageInfo.NextPageURL and PageInfo.PreviousPageURL.
func Test_PageInfoPageURLs(t *testing.T) {
	t.Parallel()

	const baseURL = "https://example.com/users"

	for _, tc := range []struct {
		pageURL  func(*PageInfo, string) string
		name     string
		expected string
		pageInfo PageInfo
	}{
		{name: "Next, middle page", pageURL: (*PageInfo).NextPageURL, pageInfo: PageInfo{Page: 2, Limit: 10}, expected: baseURL + "?limit=10&page=3"},
		{name: "Next, first page", pageURL: (*PageInfo).NextPageURL, pageInfo: PageInfo{Page: 1, Limit: 20}, expected: baseURL + "?limit=20&page=2"},
		{name: "Previous, middle page", pageURL: (*PageInfo).PreviousPageURL, pageInfo: PageInfo{Page: 2, Limit: 10}, expected: baseURL + "?limit=10&page=1"},
		{name: "Previous, first page returns empty", pageURL: (*PageInfo).PreviousPageURL, pageInfo: PageInfo{Page: 1, Limit: 20}, expected: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.expected, tc.pageURL(&tc.pageInfo, baseURL))
		})
	}
}

func Test_PageInfoStartCursorMode(t *testing.T) {
	t.Parallel()

	// In cursor mode, Page is 0 (not set). Start() should return 0, not negative.
	p := &PageInfo{Page: 0, Limit: 20}
	require.Equal(t, 0, p.Start())
}

func Test_PageInfoNextPageURLWithExistingQueryParams(t *testing.T) {
	t.Parallel()

	p := PageInfo{Page: 2, Limit: 10}
	result := p.NextPageURL("https://example.com/users?filter=active")
	require.Contains(t, result, "filter=active")
	require.Contains(t, result, "page=3")
	require.Contains(t, result, "limit=10")
}

func Test_encodeQueryValues(t *testing.T) {
	t.Parallel()

	cases := []url.Values{
		{},
		{"a": {"1"}},
		{"filter": {"active"}, "page": {"2"}, "limit": {"10"}},
		{"q": {"a b+c&d=e", "second value"}, "sp ace": {"100%"}},
		{"unicode": {"héllo wörld"}, "reserved": {";/?:@&=+$,#[]"}},
		{"empty": {""}, "multi": {"", "x", ""}},
	}
	for _, q := range cases {
		require.Equal(t, q.Encode(), encodeQueryValues(q))
	}
}

func Test_PageInfoPreviousPageURLWithExistingQueryParams(t *testing.T) {
	t.Parallel()

	p := PageInfo{Page: 3, Limit: 10}
	result := p.PreviousPageURL("https://example.com/users?filter=active")
	require.Contains(t, result, "filter=active")
	require.Contains(t, result, "page=2")
	require.Contains(t, result, "limit=10")
}

func Test_PageInfoCursorFields(t *testing.T) {
	t.Parallel()

	p := &PageInfo{
		Cursor:     "abc123",
		HasMore:    true,
		NextCursor: "def456",
	}

	require.Equal(t, "abc123", p.Cursor)
	require.True(t, p.HasMore)
	require.Equal(t, "def456", p.NextCursor)
}

func Test_CursorValuesRoundTrip(t *testing.T) {
	t.Parallel()

	original := map[string]any{
		"id":         float64(42),
		"created_at": "2026-01-01T00:00:00Z",
	}

	p := &PageInfo{}
	require.NoError(t, p.SetNextCursor(original))

	require.True(t, p.HasMore)
	require.NotEmpty(t, p.NextCursor)

	p2 := &PageInfo{Cursor: p.NextCursor}
	decoded := p2.CursorValues()

	require.NotNil(t, decoded)
	require.InEpsilon(t, float64(42), decoded["id"], 0)
	require.Equal(t, "2026-01-01T00:00:00Z", decoded["created_at"])
}

// Test_CursorValuesUndecodable covers the cursors CursorValues cannot decode.
func Test_CursorValuesUndecodable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		cursor string
	}{
		{name: "empty cursor", cursor: ""},
		{name: "invalid base64", cursor: "not-valid-base64!!!"},
		{name: "valid base64 but invalid JSON", cursor: "bm90LWpzb24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := &PageInfo{Cursor: tc.cursor}
			require.Nil(t, p.CursorValues())
		})
	}
}

func Test_NextCursorURL(t *testing.T) {
	t.Parallel()

	t.Run("with HasMore", func(t *testing.T) {
		t.Parallel()
		p := &PageInfo{Limit: 20}
		require.NoError(t, p.SetNextCursor(map[string]any{"id": float64(42)}))

		nextURL := p.NextCursorURL("https://example.com/users")
		expected := fmt.Sprintf("https://example.com/users?cursor=%s&limit=20", p.NextCursor)
		require.Equal(t, expected, nextURL)
	})

	t.Run("without HasMore", func(t *testing.T) {
		t.Parallel()
		p := &PageInfo{Limit: 20}
		require.Empty(t, p.NextCursorURL("https://example.com/users"))
	})
}

func Test_SetNextCursorSetsFields(t *testing.T) {
	t.Parallel()

	p := &PageInfo{Limit: 10}
	require.NoError(t, p.SetNextCursor(map[string]any{"id": float64(1)}))
	require.True(t, p.HasMore)
	require.NotEmpty(t, p.NextCursor)
}

// --- Middleware handler tests ---

// paginateTestApp returns an app that runs New(cfg...) in front of GET /, which
// answers with the PageInfo the middleware stored on the request context as
// JSON, or with 400 when it stored none.
func paginateTestApp(cfg ...Config) *fiber.App {
	app := fiber.New()
	app.Use(New(cfg...))

	app.Get("/", func(c fiber.Ctx) error {
		pageInfo, ok := FromContext(c)
		if !ok {
			return fiber.ErrBadRequest
		}
		return c.JSON(pageInfo)
	})

	return app
}

// paginateTestGet requests target from paginateTestApp(cfg...) and returns the
// PageInfo it answers with. The answer must be a 200.
func paginateTestGet(t *testing.T, target string, cfg ...Config) PageInfo {
	t.Helper()

	resp := apptest.Expect(t, paginateTestApp(cfg...), target, fiber.StatusOK)
	var got PageInfo
	require.NoError(t, json.Unmarshal(apptest.BodyBytes(t, resp), &got))
	return got
}

// paginateResponse is what Test_PaginateWithQueries' handler derives from the
// PageInfo, including the page links it builds from the request's base URL.
type paginateResponse struct {
	NextPageURL     string      `json:"next_page_url"`
	PreviousPageURL string      `json:"prev_page_url"`
	Sort            []SortField `json:"sort"`
	Page            int         `json:"page"`
	Limit           int         `json:"limit"`
	Offset          int         `json:"offset"`
	Start           int         `json:"start"`
}

func Test_PaginateWithQueries(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Use(New(Config{
		DefaultSort: "id",
	}))

	app.Get("/", func(c fiber.Ctx) error {
		pageInfo, ok := FromContext(c)
		if !ok {
			return fiber.ErrBadRequest
		}

		return c.JSON(paginateResponse{
			Page:            pageInfo.Page,
			Limit:           pageInfo.Limit,
			Offset:          pageInfo.Offset,
			Start:           pageInfo.Start(),
			Sort:            pageInfo.Sort,
			NextPageURL:     pageInfo.NextPageURL(c.BaseURL()),
			PreviousPageURL: pageInfo.PreviousPageURL(c.BaseURL()),
		})
	})

	resp := apptest.Expect(t, app, "/?page=2&limit=20", fiber.StatusOK)
	defer resp.Body.Close() //nolint:errcheck // close error not relevant in tests

	var body paginateResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

	require.Equal(t, 2, body.Page)
	require.Equal(t, 20, body.Limit)
	require.Equal(t, 0, body.Offset)
	require.Equal(t, 20, body.Start)
	require.Equal(t, "http://example.com?limit=20&page=3", body.NextPageURL)
	require.Equal(t, "http://example.com?limit=20&page=1", body.PreviousPageURL)
	require.Equal(t, []SortField{{Field: "id", Order: ASC}}, body.Sort)
}

// Test_PaginateRequests covers what the middleware reads from the query (page,
// limit, offset and cursor), the configuration that renames or defaults those
// parameters and the PageInfo a handler then finds on the request context.
func Test_PaginateRequests(t *testing.T) {
	t.Parallel()

	idAsc := []SortField{{Field: "id", Order: ASC}}
	cursor42 := base64.RawURLEncoding.EncodeToString([]byte(`{"id":42}`))
	cursor1 := base64.RawURLEncoding.EncodeToString([]byte(`{"id":1}`))

	for _, tc := range []struct {
		name   string
		target string
		cfg    []Config // none: the middleware is created without a config
		want   PageInfo
		start  int // want.Start()
	}{
		{
			name:   "offset",
			target: "/?offset=20&limit=20",
			want:   PageInfo{Page: 1, Limit: 20, Offset: 20, Sort: idAsc},
			start:  20,
		},
		{
			name:   "defaults when no queries",
			target: "/",
			want:   PageInfo{Page: 1, Limit: 10, Sort: idAsc},
		},
		{
			name:   "defaults when no page",
			target: "/?limit=20",
			want:   PageInfo{Page: 1, Limit: 20, Sort: idAsc},
		},
		{
			name:   "defaults when no limit",
			target: "/?page=2",
			want:   PageInfo{Page: 2, Limit: 10, Sort: idAsc},
			start:  10,
		},
		{
			name:   "config default page and default limit",
			cfg:    []Config{{DefaultPage: 100, DefaultLimit: DefaultMaxLimit, DefaultSort: "name"}},
			target: "/",
			want:   PageInfo{Page: 100, Limit: DefaultMaxLimit, Sort: []SortField{{Field: "name", Order: ASC}}},
			start:  9900,
		},
		{
			name:   "config page key and limit key",
			cfg:    []Config{{PageKey: "site", LimitKey: "size", DefaultSort: "id"}},
			target: "/?site=2&size=5",
			want:   PageInfo{Page: 2, Limit: 5, Sort: idAsc},
			start:  5,
		},
		{
			name:   "negative default page and default limit values",
			cfg:    []Config{{DefaultPage: -1, DefaultLimit: -1, DefaultSort: "id"}},
			target: "/",
			want:   PageInfo{Page: 1, Limit: 10, Sort: idAsc},
		},
		{
			name:   "no cursor falls back to page mode",
			cfg:    []Config{{DefaultSort: "id"}},
			target: "/?page=3&limit=15",
			want:   PageInfo{Page: 3, Limit: 15, Sort: idAsc},
			start:  30,
		},
		{
			name:   "cursor with limit",
			cfg:    []Config{{DefaultSort: "id"}},
			target: "/?cursor=" + cursor42 + "&limit=20",
			want:   PageInfo{Cursor: cursor42, Limit: 20, Sort: idAsc},
		},
		{
			name:   "cursor takes priority over page",
			target: "/?cursor=" + cursor42 + "&page=5&limit=10",
			want:   PageInfo{Cursor: cursor42, Limit: 10, Sort: idAsc}, // no Page
		},
		{
			name:   "empty cursor is the first page",
			target: "/?cursor=&limit=10",
			want:   PageInfo{Page: 1, Limit: 10, Sort: idAsc},
		},
		{
			name:   "cursor with sort",
			cfg:    []Config{{SortKey: "sort", DefaultSort: "id", AllowedSorts: []string{"id", "name"}}},
			target: "/?cursor=" + cursor42 + "&sort=name,-id",
			want:   PageInfo{Cursor: cursor42, Limit: 10, Sort: []SortField{{Field: "name", Order: ASC}, {Field: "id", Order: DESC}}},
		},
		{
			name:   "cursor with custom key",
			cfg:    []Config{{CursorKey: "after"}},
			target: "/?after=" + cursor1,
			want:   PageInfo{Cursor: cursor1, Limit: 10, Sort: idAsc},
		},
		{
			name:   "cursor with param alias",
			cfg:    []Config{{CursorParam: "starting_after"}},
			target: "/?starting_after=" + cursor1,
			want:   PageInfo{Cursor: cursor1, Limit: 10, Sort: idAsc},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := paginateTestGet(t, tc.target, tc.cfg...)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.start, got.Start())
		})
	}
}

func Test_PaginateFromContextWithoutNew(t *testing.T) {
	t.Parallel()
	app := fiber.New()

	app.Get("/", func(c fiber.Ctx) error {
		_, ok := FromContext(c)
		if !ok {
			return fiber.ErrBadRequest
		}
		return c.JSON(nil)
	})

	apptest.Expect(t, app, "/", fiber.StatusBadRequest)
}

func Test_PaginateNextSkip(t *testing.T) {
	t.Parallel()

	app := paginateTestApp(Config{
		Next: func(_ fiber.Ctx) bool { return true },
	})

	apptest.Expect(t, app, "/", fiber.StatusBadRequest)
}

func Test_PaginateEdgeCases(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		url           string
		expectedPage  int
		expectedLimit int
	}{
		{"Negative page", "/?page=-1", 1, 10},
		{"Page zero", "/?page=0", 1, 10},
		{"Negative limit", "/?limit=-10", 1, 10},
		{"Limit zero", "/?limit=0", 1, 10},
		{"Limit exceeds max", "/?limit=200", 1, DefaultMaxLimit},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := paginateTestGet(t, tc.url, Config{DefaultSort: "id", DefaultLimit: 10})
			require.Equal(t, tc.expectedPage, result.Page)
			require.Equal(t, tc.expectedLimit, result.Limit)
		})
	}
}

func Test_PaginateWithMultipleSorting(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		url          string
		expectedSort []SortField
	}{
		{"Default Sort", "/", []SortField{{Field: "id", Order: ASC}}},
		{"Single Sort", "/?sort=name", []SortField{{Field: "name", Order: ASC}}},
		{"Multiple Sort", "/?sort=name,-date", []SortField{{Field: "name", Order: ASC}, {Field: "date", Order: DESC}}},
		{"Invalid Sort", "/?sort=invalid", []SortField{{Field: "id", Order: ASC}}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := paginateTestGet(t, tc.url, Config{
				SortKey:      "sort",
				DefaultSort:  "id",
				AllowedSorts: []string{"id", "name", "date"},
			})
			require.Equal(t, tc.expectedSort, result.Sort)
		})
	}
}

func Test_ParseSortQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		query        string
		allowedSorts []string
		defaultSort  string
		expected     []SortField
	}{
		{
			"Empty query",
			"",
			[]string{"id", "name", "date"},
			"id",
			[]SortField{{Field: "id", Order: ASC}},
		},
		{
			"Single allowed field",
			"name",
			[]string{"id", "name", "date"},
			"id",
			[]SortField{{Field: "name", Order: ASC}},
		},
		{
			"Multiple fields with mixed order",
			"name,-date,id",
			[]string{"id", "name", "date"},
			"id",
			[]SortField{
				{Field: "name", Order: ASC},
				{Field: "date", Order: DESC},
				{Field: "id", Order: ASC},
			},
		},
		{
			"Disallowed field",
			"email,name",
			[]string{"id", "name", "date"},
			"id",
			[]SortField{{Field: "name", Order: ASC}},
		},
		{
			"All disallowed fields",
			"email,phone",
			[]string{"id", "name", "date"},
			"id",
			[]SortField{{Field: "id", Order: ASC}},
		},
		{
			"Nil AllowedSorts falls back to default sort",
			"email,-phone",
			nil,
			"id",
			[]SortField{{Field: "id", Order: ASC}},
		},
		{
			"Empty AllowedSorts falls back to default sort",
			"email,-phone",
			[]string{},
			"id",
			[]SortField{{Field: "id", Order: ASC}},
		},
		{
			"Bare dash is skipped",
			"-",
			nil,
			"id",
			[]SortField{{Field: "id", Order: ASC}},
		},
		{
			"Dash in comma list is skipped",
			"name,-,email",
			nil,
			"id",
			[]SortField{{Field: "id", Order: ASC}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := parseSortQuery(tt.query, tt.allowedSorts, tt.defaultSort)
			require.Equal(t, tt.expected, result)
		})
	}
}

// --- Cursor tests ---

func Test_PaginateInvalidCursorReturns400(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		cursor string
	}{
		{"Invalid base64", "not-valid!!!"},
		{"Valid base64 but invalid JSON", base64.RawURLEncoding.EncodeToString([]byte("not-json"))},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			apptest.Expect(t, paginateTestApp(), "/?cursor="+tc.cursor, fiber.StatusBadRequest)
		})
	}
}

func Test_NextPageURLWithKeys(t *testing.T) {
	t.Parallel()

	p := PageInfo{Page: 2, Limit: 10}
	result := p.NextPageURLWithKeys("https://example.com/users", "p", "per_page")
	require.Equal(t, "https://example.com/users?p=3&per_page=10", result)
}

func Test_PreviousPageURLWithKeys(t *testing.T) {
	t.Parallel()

	t.Run("has previous", func(t *testing.T) {
		t.Parallel()
		p := PageInfo{Page: 3, Limit: 15}
		result := p.PreviousPageURLWithKeys("https://example.com/items", "p", "size")
		require.Equal(t, "https://example.com/items?p=2&size=15", result)
	})

	t.Run("first page returns empty", func(t *testing.T) {
		t.Parallel()
		p := PageInfo{Page: 1, Limit: 15}
		result := p.PreviousPageURLWithKeys("https://example.com/items", "p", "size")
		require.Empty(t, result)
	})
}

func Test_NextCursorURLWithKeys(t *testing.T) {
	t.Parallel()

	t.Run("has more", func(t *testing.T) {
		t.Parallel()
		p := &PageInfo{Limit: 20}
		require.NoError(t, p.SetNextCursor(map[string]any{"id": float64(42)}))

		result := p.NextCursorURLWithKeys("https://example.com/users", "after", "per_page")
		expected := fmt.Sprintf("https://example.com/users?after=%s&per_page=20", p.NextCursor)
		require.Equal(t, expected, result)
	})

	t.Run("no more", func(t *testing.T) {
		t.Parallel()
		p := &PageInfo{Limit: 20}
		result := p.NextCursorURLWithKeys("https://example.com/users", "after", "per_page")
		require.Empty(t, result)
	})
}

func Test_PaginateCustomMaxLimit(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		url           string
		expectedLimit int
	}{
		{"Limit within custom max", "/?limit=40", 40},
		{"Limit exceeds custom max", "/?limit=200", 50},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := paginateTestGet(t, tc.url, Config{DefaultSort: "id", DefaultLimit: 10, MaxLimit: 50})
			require.Equal(t, tc.expectedLimit, result.Limit)
		})
	}
}

func Test_ConfigDefaultMaxLimitNormalization(t *testing.T) {
	t.Parallel()

	cfg := configDefault(Config{MaxLimit: 0})
	require.Equal(t, DefaultMaxLimit, cfg.MaxLimit)

	cfg2 := configDefault(Config{MaxLimit: 50})
	require.Equal(t, 50, cfg2.MaxLimit)
}

// --- Benchmarks ---

func Benchmark_PaginateMiddleware(b *testing.B) {
	app := fiber.New()
	app.Use(New())

	app.Get("/", func(c fiber.Ctx) error {
		pageInfo, _ := FromContext(c)
		return c.JSON(pageInfo)
	})

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/?page=2&limit=20&sort=name,-date", http.NoBody)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close() //nolint:errcheck // close error not relevant in tests
	}
}

func Benchmark_PaginateMiddlewareWithCustomConfig(b *testing.B) {
	app := fiber.New()
	app.Use(New(Config{
		PageKey:      "p",
		LimitKey:     "l",
		SortKey:      "s",
		DefaultPage:  1,
		DefaultLimit: 30,
		DefaultSort:  "id",
		AllowedSorts: []string{"id", "name", "date"},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		pageInfo, _ := FromContext(c)
		return c.JSON(pageInfo)
	})

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/?p=3&l=25&s=name,-id", http.NoBody)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close() //nolint:errcheck // close error not relevant in tests
	}
}

func Benchmark_PaginateCursorMiddleware(b *testing.B) {
	app := fiber.New()
	app.Use(New(Config{
		SortKey:      "sort",
		DefaultSort:  "id",
		AllowedSorts: []string{"id", "name", "date"},
	}))

	app.Get("/", func(c fiber.Ctx) error {
		pageInfo, _ := FromContext(c)
		return c.JSON(pageInfo)
	})

	cursorJSON := `{"id":42,"created_at":"2026-01-01T00:00:00Z"}`
	cursor := base64.RawURLEncoding.EncodeToString([]byte(cursorJSON))

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/?cursor="+cursor+"&limit=20&sort=name,-id", http.NoBody)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close() //nolint:errcheck // close error not relevant in tests
	}
}

func Test_Paginate_EmptySortKey_IgnoresQuery(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(New(Config{AllowedSorts: []string{"name", "id"}, DefaultSort: "id"}))
	app.Get("/", func(c fiber.Ctx) error {
		info, ok := FromContext(c)
		if !ok {
			return c.SendString("missing")
		}
		return c.SendString(strconv.Itoa(len(info.Sort)) + " " + info.Sort[0].Field + " " + string(info.Sort[0].Order))
	})

	for _, target := range []string{"/", "/?=-name"} {
		resp := apptest.Do(t, app, target)
		body := apptest.Body(t, resp)
		require.Equal(t, "1 id asc", body, "GET %s must use the default sort", target)
	}
}

func Test_PageInfo_NextPageURL_NoOverflow(t *testing.T) {
	t.Parallel()

	p := &PageInfo{Page: math.MaxInt, Limit: 10}
	require.NotContains(t, p.NextPageURL("/items"), "-")
	require.Contains(t, p.NextPageURL("/items"), "page="+strconv.Itoa(math.MaxInt))
}
