package handler

import (
	"fmt"
	"net/url"
	"strconv"

	"github.com/Tushar-programador/trip-management-api/internal/domain"
)

const (
	DefaultPage     = 1
	DefaultPageSize = 20
	MaxPageSize     = 100
	// MaxPage bounds OFFSET so deep pages cannot become pathological scans
	// (or overflow). 1,000,000 x 100 rows is far beyond realistic paging.
	MaxPage = 1_000_000
)

// Pagination is the pagination block of a list response.
type Pagination struct {
	Page       int `json:"page"`
	PageSize   int `json:"page_size"`
	TotalItems int `json:"total_items"`
	TotalPages int `json:"total_pages"`
}

func newPagination(f domain.ListFilter, total int) Pagination {
	return Pagination{
		Page:       f.Page,
		PageSize:   f.PageSize,
		TotalItems: total,
		TotalPages: (total + f.PageSize - 1) / f.PageSize,
	}
}

// parseListFilter validates page, page_size and status. Out-of-range values
// are rejected rather than silently clamped, so clients notice their bug.
func parseListFilter(q url.Values) (domain.ListFilter, error) {
	f := domain.ListFilter{Page: DefaultPage, PageSize: DefaultPageSize}
	var errs []domain.FieldError

	if raw, ok := q["page"]; ok {
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 1 || n > MaxPage {
			errs = append(errs, domain.FieldError{Field: "page", Message: fmt.Sprintf("must be an integer between 1 and %d", MaxPage)})
		} else {
			f.Page = n
		}
	}
	if raw, ok := q["page_size"]; ok {
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 1 || n > MaxPageSize {
			errs = append(errs, domain.FieldError{Field: "page_size", Message: fmt.Sprintf("must be an integer between 1 and %d", MaxPageSize)})
		} else {
			f.PageSize = n
		}
	}
	if len(errs) > 0 {
		return domain.ListFilter{}, &domain.ValidationError{Fields: errs}
	}

	if raw, ok := q["status"]; ok {
		s, valid := domain.ParseStatus(raw[0])
		if !valid {
			return domain.ListFilter{}, &domain.InvalidStatusError{Value: raw[0]}
		}
		f.Status = &s
	}
	return f, nil
}
