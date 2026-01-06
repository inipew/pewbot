package tgui

import "fmt"

// PaginateSlice returns a sub-slice for the requested page and helper flags.
// page is 0-based. size must be > 0.
func PaginateSlice[T any](items []T, page, size int) (sub []T, page2 int, size2 int, from int, to int, hasPrev bool, hasNext bool) {
	if size <= 0 {
		size = 10
	}
	if page < 0 {
		page = 0
	}
	total := len(items)
	start := page * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	sub = items[start:end]
	hasPrev = page > 0
	hasNext = end < total
	return sub, page, size, start, end, hasPrev, hasNext
}

// PageLabel returns a compact, human-friendly pagination label.
// page is 0-based.
func PageLabel(page, size, total int) string {
	if size <= 0 {
		size = 10
	}
	if total <= 0 {
		return "Halaman 1/1"
	}
	pages := total / size
	if total%size != 0 {
		pages++
	}
	if pages <= 0 {
		pages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	from := page*size + 1
	to := (page + 1) * size
	if to > total {
		to = total
	}
	return fmt.Sprintf("Halaman %d/%d • %d–%d dari %d", page+1, pages, from, to, total)
}
