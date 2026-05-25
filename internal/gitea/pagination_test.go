package gitea

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
)

// pageStub records the sequence of `page` query values served and returns
// canned item counts per page. limit=50 is asserted on every request.
type pageStub struct {
	t        *testing.T
	requests []string
	counts   map[int]int // page → item count
}

func newPageStub(t *testing.T, counts map[int]int) *pageStub {
	return &pageStub{t: t, counts: counts}
}

func (s *pageStub) handler(encode func(w http.ResponseWriter, items []int)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := q.Get("limit"); got != "50" {
			s.t.Errorf("limit param = %q, want 50", got)
		}
		page, _ := strconv.Atoi(q.Get("page"))
		s.requests = append(s.requests, q.Get("page"))

		count, ok := s.counts[page]
		if !ok {
			s.t.Errorf("unexpected request for page=%d", page)
			http.Error(w, "unexpected page", http.StatusInternalServerError)
			return
		}
		items := make([]int, count)
		for i := range items {
			items[i] = page*1000 + i
		}
		encode(w, items)
	})
}

func encodeBare(w http.ResponseWriter, items []int) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

type itemsWrapper struct {
	Meta  string `json:"meta"`
	Items []int  `json:"items"`
}

func encodeWrapped(w http.ResponseWriter, items []int) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(itemsWrapper{Meta: "ignored", Items: items})
}

// paginate stops as soon as a page returns fewer than 50 items.
func TestPaginate_StopsOnShortPage(t *testing.T) {
	stub := newPageStub(t, map[int]int{1: 50, 2: 25})
	srv := httptest.NewServer(stub.handler(encodeBare))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "tok")
	got, err := paginate[int](context.Background(), c, "/api/v1/things?page=%d&limit=50", "list things")
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 75 {
		t.Errorf("got %d items, want 75", len(got))
	}
	if want := []string{"1", "2"}; !slices.Equal(stub.requests, want) {
		t.Errorf("requests = %v, want %v (must stop after short page)", stub.requests, want)
	}
}

// paginateUntilEmpty ignores short pages and only stops on an empty page.
// This is the contract that makes it safe against endpoints that post-filter
// after the SQL LIMIT (e.g. the timeline endpoint).
func TestPaginateUntilEmpty_IgnoresShortPagesStopsOnEmpty(t *testing.T) {
	stub := newPageStub(t, map[int]int{1: 25, 2: 25, 3: 0})
	srv := httptest.NewServer(stub.handler(encodeBare))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "tok")
	got, err := paginateUntilEmpty[int](context.Background(), c, "/api/v1/things?page=%d&limit=50", "list things")
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 50 {
		t.Errorf("got %d items, want 50", len(got))
	}
	if want := []string{"1", "2", "3"}; !slices.Equal(stub.requests, want) {
		t.Errorf("requests = %v, want %v (must NOT stop on short page)", stub.requests, want)
	}
}

// paginateWrapped decodes a wrapping object and uses the extract callback to
// pull out the page items. Same EOF contract as paginate (stop on short).
func TestPaginateWrapped_ExtractsAndStopsOnShortPage(t *testing.T) {
	stub := newPageStub(t, map[int]int{1: 50, 2: 5})
	srv := httptest.NewServer(stub.handler(encodeWrapped))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "tok")
	got, err := paginateWrapped(context.Background(), c,
		"/api/v1/wrapped?page=%d&limit=50", "wrapped",
		func(w *itemsWrapper) []int { return w.Items })
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 55 {
		t.Errorf("got %d items, want 55", len(got))
	}
	if got[0] != 1000 || got[49] != 1049 || got[50] != 2000 || got[54] != 2004 {
		t.Errorf("unexpected items at boundaries: %v ... %v", got[:3], got[len(got)-3:])
	}
	if want := []string{"1", "2"}; !slices.Equal(stub.requests, want) {
		t.Errorf("requests = %v, want %v", stub.requests, want)
	}
}

// paginateWrappedUntilEmpty combines wrapped-object extraction with the
// stop-on-empty contract: the helper must walk past short non-final pages
// AND apply the extract callback on each one.
func TestPaginateWrappedUntilEmpty_IgnoresShortPagesStopsOnEmpty(t *testing.T) {
	stub := newPageStub(t, map[int]int{1: 10, 2: 10, 3: 0})
	srv := httptest.NewServer(stub.handler(encodeWrapped))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "tok")
	got, err := paginateWrappedUntilEmpty(context.Background(), c,
		"/api/v1/wrapped?page=%d&limit=50", "wrapped",
		func(w *itemsWrapper) []int { return w.Items })
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 20 {
		t.Errorf("got %d items, want 20", len(got))
	}
	if want := []string{"1", "2", "3"}; !slices.Equal(stub.requests, want) {
		t.Errorf("requests = %v, want %v (must NOT stop on short page)", stub.requests, want)
	}
}
