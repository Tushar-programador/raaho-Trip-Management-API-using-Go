package handler

import (
	"errors"
	"net/url"
	"testing"

	"github.com/Tushar-programador/trip-management-api/internal/domain"
)

func TestParseListFilter(t *testing.T) {
	cases := []struct {
		query        string
		wantPage     int
		wantSize     int
		wantStatus   domain.Status // "" means no filter
		wantErrField string        // VALIDATION_ERROR field, if any
		wantBadStat  bool          // INVALID_STATUS expected
	}{
		{query: "", wantPage: 1, wantSize: 20},
		{query: "page=3&page_size=50", wantPage: 3, wantSize: 50},
		{query: "page_size=1", wantPage: 1, wantSize: 1},
		{query: "page_size=100", wantPage: 1, wantSize: 100},
		{query: "page=1000000", wantPage: 1000000, wantSize: 20},
		{query: "status=ASSIGNED", wantPage: 1, wantSize: 20, wantStatus: domain.StatusAssigned},
		{query: "unknown=x", wantPage: 1, wantSize: 20},

		{query: "page=0", wantErrField: "page"},
		{query: "page=-1", wantErrField: "page"},
		{query: "page=abc", wantErrField: "page"},
		{query: "page=", wantErrField: "page"},
		{query: "page=1.5", wantErrField: "page"},
		{query: "page=1000001", wantErrField: "page"},
		{query: "page=99999999999999999999", wantErrField: "page"},
		{query: "page_size=0", wantErrField: "page_size"},
		{query: "page_size=101", wantErrField: "page_size"},
		{query: "page_size=ten", wantErrField: "page_size"},

		{query: "status=assigned", wantBadStat: true},
		{query: "status=", wantBadStat: true},
		{query: "status=DONE", wantBadStat: true},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			q, err := url.ParseQuery(c.query)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parseListFilter(q)

			switch {
			case c.wantErrField != "":
				var ve *domain.ValidationError
				if !errors.As(err, &ve) || ve.Fields[0].Field != c.wantErrField {
					t.Fatalf("want validation error on %q, got %v", c.wantErrField, err)
				}
			case c.wantBadStat:
				var se *domain.InvalidStatusError
				if !errors.As(err, &se) {
					t.Fatalf("want InvalidStatusError, got %v", err)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if f.Page != c.wantPage || f.PageSize != c.wantSize {
					t.Errorf("got page=%d size=%d, want %d/%d", f.Page, f.PageSize, c.wantPage, c.wantSize)
				}
				if (f.Status == nil) != (c.wantStatus == "") || (f.Status != nil && *f.Status != c.wantStatus) {
					t.Errorf("status = %v, want %q", f.Status, c.wantStatus)
				}
			}
		})
	}
}

func TestParseListFilterReportsBothFields(t *testing.T) {
	_, err := parseListFilter(url.Values{"page": {"0"}, "page_size": {"500"}})
	var ve *domain.ValidationError
	if !errors.As(err, &ve) || len(ve.Fields) != 2 {
		t.Fatalf("want 2 field errors, got %v", err)
	}
}

func TestNewPagination(t *testing.T) {
	cases := []struct{ total, size, wantPages int }{
		{0, 20, 0}, {1, 20, 1}, {20, 20, 1}, {21, 20, 2}, {100, 100, 1}, {101, 100, 2},
	}
	for _, c := range cases {
		p := newPagination(domain.ListFilter{Page: 1, PageSize: c.size}, c.total)
		if p.TotalPages != c.wantPages || p.TotalItems != c.total {
			t.Errorf("total=%d size=%d: got %+v, want %d pages", c.total, c.size, p, c.wantPages)
		}
	}
}
