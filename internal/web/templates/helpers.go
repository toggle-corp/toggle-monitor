package templates

import (
	"net/url"
	"strconv"

	"github.com/a-h/templ"
)

func pageCount(total, perPage int) int {
	if perPage <= 0 {
		return 1
	}
	n := total / perPage
	if total%perPage != 0 {
		n++
	}
	if n == 0 {
		return 1
	}
	return n
}

func paginatorLink(base string, extra url.Values, page int) templ.SafeURL {
	v := url.Values{}
	for k, vals := range extra {
		for _, val := range vals {
			if val != "" {
				v.Add(k, val)
			}
		}
	}
	v.Set("page", strconv.Itoa(page))
	return templ.URL(base + "?" + v.Encode())
}

func paramsFromFilter(f MonitorsFilter) url.Values {
	v := url.Values{}
	if f.Search != "" {
		v.Set("q", f.Search)
	}
	if f.Status != "" {
		v.Set("status", f.Status)
	}
	if f.Group != "" {
		v.Set("group", f.Group)
	}
	return v
}
