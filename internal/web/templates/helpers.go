package templates

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"

	"github.com/a-h/templ"

	"github.com/toggle-corp/toggle-monitor/internal/store"
)

func summary(rows []store.DiscoverySnapshotRow) string {
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Status]++
	}
	return fmt.Sprintf("%d total · %d added · %d kube-paused · %d kube-invalid",
		len(rows), counts["added"], counts["kube-paused"], counts["kube-invalid"])
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

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
