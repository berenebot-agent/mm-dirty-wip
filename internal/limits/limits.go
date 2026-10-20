// Package limits holds the pagination bounds shared by the store and the
// discovery surfaces. Keeping them in one leaf package means the advertised
// limits can never drift from the clamping applied when a query runs.
package limits

const (
	// PageSizeDefault is the page size used when a listing request omits limit
	// or supplies a non-positive one.
	PageSizeDefault = 100
	// PageSizeMaxList is the ceiling for list endpoints backed by a
	// newest-first LIMIT scan (messages, threads, search, outbox, send
	// requests and the admin delivery logs).
	PageSizeMaxList = 200
	// PageSizeMaxEvents is the ceiling for events and drafts, which are paged
	// more deeply because a consumer may need to catch up across a backlog.
	PageSizeMaxEvents = 500
)
