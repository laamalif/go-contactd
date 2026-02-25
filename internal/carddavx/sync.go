package carddavx

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/laamalif/go-contactd/internal/db"
)

const syncTokenPrefix = "urn:contactd:sync:"

const (
	syncCursorTTL      = 2 * time.Minute
	syncCursorMaxItems = 10000
)

var (
	// Global cache caps bound authenticated memory use across all paginated sync cursors.
	syncCursorCacheMaxEntries    = 256
	syncCursorCacheMaxTotalItems = 200000
)

type SyncToken struct {
	AddressbookID int64
	Revision      int64
}

type invalidSyncTokenError struct {
	cause error
}

func (e *invalidSyncTokenError) Error() string {
	if e == nil || e.cause == nil {
		return "invalid sync token"
	}
	return "invalid sync token: " + e.cause.Error()
}

func (e *invalidSyncTokenError) Unwrap() error { return e.cause }

func IsInvalidSyncToken(err error) bool {
	var target *invalidSyncTokenError
	return errors.As(err, &target)
}

func FormatSyncToken(addressbookID, revision int64) string {
	return fmt.Sprintf("%s%d:%d", syncTokenPrefix, addressbookID, revision)
}

func ParseSyncToken(raw string) (SyncToken, error) {
	if strings.TrimSpace(raw) == "" {
		return SyncToken{}, fmt.Errorf("empty token")
	}
	if !strings.HasPrefix(raw, syncTokenPrefix) {
		return SyncToken{}, fmt.Errorf("bad prefix")
	}
	rest := strings.TrimPrefix(raw, syncTokenPrefix)
	parts := strings.Split(rest, ":")
	if len(parts) != 2 {
		return SyncToken{}, fmt.Errorf("bad token parts")
	}
	abID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || abID <= 0 {
		return SyncToken{}, fmt.Errorf("bad addressbook id")
	}
	rev, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || rev < 0 {
		return SyncToken{}, fmt.Errorf("bad revision")
	}
	return SyncToken{AddressbookID: abID, Revision: rev}, nil
}

type SyncRef struct {
	Href string
	ETag string
}

type SyncResult struct {
	SyncToken string
	Updated   []SyncRef
	Deleted   []string
	Truncated bool
}

type SyncService struct {
	store *db.Store
	mu    sync.Mutex
	page  map[string]syncCursor
}

func NewSyncService(store *db.Store) *SyncService {
	return &SyncService{store: store, page: make(map[string]syncCursor)}
}

type syncPageItem struct {
	Revision int64
	Href     string
	ETagHex  string
	Deleted  bool
}

type syncCursor struct {
	AddressbookID int64
	HeadRevision  int64
	ExpiresAt     time.Time
	Items         []syncPageItem
}

type CollectionState struct {
	AddressbookID int64
	Revision      int64
	SyncToken     string
	CTag          string
}

func (s *SyncService) CollectionState(ctx context.Context, username, slug string) (CollectionState, error) {
	ab, err := s.store.GetAddressbookByUsernameSlug(ctx, username, slug)
	if err != nil {
		return CollectionState{}, err
	}
	return CollectionState{
		AddressbookID: ab.ID,
		Revision:      ab.Revision,
		SyncToken:     FormatSyncToken(ab.ID, ab.Revision),
		CTag:          strconv.FormatInt(ab.Revision, 10),
	}, nil
}

func (s *SyncService) SyncCollection(ctx context.Context, username, slug, rawToken string, limit int) (SyncResult, error) {
	ab, err := s.store.GetAddressbookByUsernameSlug(ctx, username, slug)
	if err != nil {
		return SyncResult{}, err
	}
	if strings.TrimSpace(rawToken) == "" {
		stateLimit := 0
		if limit > 0 {
			// Fetch the full state list so a truncated page can cache remaining items and survive prune between pages.
			stateLimit = 0
		}
		states, err := s.store.ListCurrentCardSyncStates(ctx, ab.ID, stateLimit)
		if err != nil {
			return SyncResult{}, err
		}
		items := make([]syncPageItem, 0, len(states))
		for _, c := range states {
			items = append(items, syncPageItem{
				Revision: c.Revision,
				Href:     c.Href,
				ETagHex:  c.ETagHex,
			})
		}
		return s.buildPagedSyncResult(ab.ID, ab.Revision, username, slug, items, limit)
	}

	token, err := ParseSyncToken(strings.TrimSpace(rawToken))
	if err != nil {
		return SyncResult{}, &invalidSyncTokenError{cause: err}
	}
	if token.AddressbookID != ab.ID {
		return SyncResult{}, &invalidSyncTokenError{cause: fmt.Errorf("addressbook mismatch")}
	}
	if limit > 0 {
		if out, ok := s.takeCursorPage(token, username, slug, limit); ok {
			return out, nil
		}
	}
	if token.Revision > ab.Revision {
		return SyncResult{}, &invalidSyncTokenError{cause: fmt.Errorf("revision ahead")}
	}

	changeLimit := limit
	if limit > 0 {
		changeLimit = 0
	}
	changes, err := s.store.ListCardChangesSince(ctx, ab.ID, token.Revision, changeLimit)
	if err != nil {
		return SyncResult{}, err
	}
	if token.Revision < ab.Revision {
		if len(changes) == 0 {
			return SyncResult{}, &invalidSyncTokenError{cause: fmt.Errorf("stale token")}
		}
		if changes[0].Revision > token.Revision+1 {
			return SyncResult{}, &invalidSyncTokenError{cause: fmt.Errorf("stale token")}
		}
	}
	items := make([]syncPageItem, 0, len(changes))
	for _, ch := range changes {
		items = append(items, syncPageItem{
			Revision: ch.Revision,
			Href:     ch.Href,
			ETagHex:  ch.ETagHex,
			Deleted:  ch.Deleted,
		})
	}
	return s.buildPagedSyncResult(ab.ID, ab.Revision, username, slug, items, limit)
}

func (s *SyncService) buildPagedSyncResult(addressbookID, headRevision int64, username, slug string, items []syncPageItem, limit int) (SyncResult, error) {
	pageItems := items
	remaining := []syncPageItem(nil)
	truncated := false
	if limit > 0 && len(items) > limit {
		truncated = true
		pageItems = items[:limit]
		remaining = append(remaining, items[limit:]...)
	}
	tokenRevision := syncPageTokenRevision(headRevision, pageItems, truncated)
	emitItems := collapseSyncPageItems(pageItems)
	out := SyncResult{
		SyncToken: FormatSyncToken(addressbookID, tokenRevision),
		Updated:   make([]SyncRef, 0, len(emitItems)),
		Deleted:   make([]string, 0, len(emitItems)),
		Truncated: truncated,
	}
	for _, it := range emitItems {
		fullHref := "/" + username + "/" + slug + "/" + it.Href
		if it.Deleted {
			out.Deleted = append(out.Deleted, fullHref)
			continue
		}
		out.Updated = append(out.Updated, SyncRef{
			Href: fullHref,
			ETag: `"` + it.ETagHex + `"`,
		})
	}
	if truncated && len(remaining) <= syncCursorMaxItems {
		s.putCursor(out.SyncToken, syncCursor{
			AddressbookID: addressbookID,
			HeadRevision:  headRevision,
			ExpiresAt:     time.Now().Add(syncCursorTTL),
			Items:         remaining,
		})
	}
	return out, nil
}

func (s *SyncService) putCursor(token string, cur syncCursor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putCursorLocked(token, cur, time.Now())
}

func (s *SyncService) takeCursorPage(token SyncToken, username, slug string, limit int) (SyncResult, bool) {
	raw := FormatSyncToken(token.AddressbookID, token.Revision)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.pruneExpiredCursorsLocked(now)
	cur, ok := s.page[raw]
	if !ok || cur.AddressbookID != token.AddressbookID || now.After(cur.ExpiresAt) {
		return SyncResult{}, false
	}
	delete(s.page, raw)
	pageItems := cur.Items
	remaining := []syncPageItem(nil)
	truncated := false
	if limit > 0 && len(cur.Items) > limit {
		truncated = true
		pageItems = cur.Items[:limit]
		remaining = append(remaining, cur.Items[limit:]...)
	}
	tokenRevision := syncPageTokenRevision(cur.HeadRevision, pageItems, truncated)
	emitItems := collapseSyncPageItems(pageItems)
	out := SyncResult{
		SyncToken: FormatSyncToken(token.AddressbookID, tokenRevision),
		Updated:   make([]SyncRef, 0, len(emitItems)),
		Deleted:   make([]string, 0, len(emitItems)),
		Truncated: truncated,
	}
	for _, it := range emitItems {
		fullHref := "/" + username + "/" + slug + "/" + it.Href
		if it.Deleted {
			out.Deleted = append(out.Deleted, fullHref)
			continue
		}
		out.Updated = append(out.Updated, SyncRef{
			Href: fullHref,
			ETag: `"` + it.ETagHex + `"`,
		})
	}
	if truncated && len(remaining) <= syncCursorMaxItems {
		s.putCursorLocked(out.SyncToken, syncCursor{
			AddressbookID: token.AddressbookID,
			HeadRevision:  cur.HeadRevision,
			ExpiresAt:     now.Add(syncCursorTTL),
			Items:         remaining,
		}, now)
	}
	return out, true
}

func collapseSyncPageItems(items []syncPageItem) []syncPageItem {
	lastIdxByHref := make(map[string]int, len(items))
	for i, it := range items {
		lastIdxByHref[it.Href] = i
	}
	out := make([]syncPageItem, 0, len(items))
	for i, it := range items {
		if lastIdxByHref[it.Href] != i {
			continue
		}
		out = append(out, it)
	}
	return out
}

func syncPageTokenRevision(headRevision int64, pageItems []syncPageItem, truncated bool) int64 {
	if len(pageItems) == 0 {
		return headRevision
	}
	lastRevision := pageItems[len(pageItems)-1].Revision
	if truncated {
		// Continuation tokens must advance to the last revision represented on this page.
		return lastRevision
	}
	if lastRevision > headRevision {
		// Concurrent writes can make the observed page outrun the earlier head read.
		return lastRevision
	}
	return headRevision
}

func (s *SyncService) pruneExpiredCursorsLocked(now time.Time) {
	for k, cur := range s.page {
		if now.After(cur.ExpiresAt) {
			delete(s.page, k)
		}
	}
}

func (s *SyncService) putCursorLocked(token string, cur syncCursor, now time.Time) {
	if s.page == nil {
		s.page = make(map[string]syncCursor)
	}
	s.pruneExpiredCursorsLocked(now)

	if syncCursorCacheMaxTotalItems > 0 && len(cur.Items) > syncCursorCacheMaxTotalItems {
		// Refuse to cache a single cursor that exceeds the total cache budget.
		return
	}

	s.page[token] = cur
	s.evictCursorsToCapLocked()
}

func (s *SyncService) evictCursorsToCapLocked() {
	for {
		if !s.cursorCacheOverCapLocked() {
			return
		}
		victim, ok := s.oldestCursorKeyLocked()
		if !ok {
			return
		}
		delete(s.page, victim)
	}
}

func (s *SyncService) cursorCacheOverCapLocked() bool {
	if syncCursorCacheMaxEntries > 0 && len(s.page) > syncCursorCacheMaxEntries {
		return true
	}
	if syncCursorCacheMaxTotalItems > 0 && s.countCursorItemsLocked() > syncCursorCacheMaxTotalItems {
		return true
	}
	return false
}

func (s *SyncService) countCursorItemsLocked() int {
	total := 0
	for _, cur := range s.page {
		total += len(cur.Items)
	}
	return total
}

func (s *SyncService) oldestCursorKeyLocked() (string, bool) {
	var (
		bestKey string
		bestExp time.Time
		ok      bool
	)
	for k, cur := range s.page {
		if !ok || cur.ExpiresAt.Before(bestExp) || (cur.ExpiresAt.Equal(bestExp) && k < bestKey) {
			bestKey = k
			bestExp = cur.ExpiresAt
			ok = true
		}
	}
	return bestKey, ok
}
