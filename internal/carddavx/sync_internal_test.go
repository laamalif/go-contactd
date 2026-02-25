package carddavx

import (
	"testing"
	"time"
)

func TestBuildPagedSyncResult_AdvancesTokenWhenHeadLagsObservedChanges(t *testing.T) {
	t.Parallel()

	svc := &SyncService{}
	items := []syncPageItem{
		{Revision: 3, Href: "a.vcf", ETagHex: "etag3"},
		{Revision: 4, Href: "b.vcf", ETagHex: "etag4"},
	}

	res, err := svc.buildPagedSyncResult(1, 2, "alice", "contacts", items, 0)
	if err != nil {
		t.Fatalf("buildPagedSyncResult: %v", err)
	}
	if got, want := res.SyncToken, FormatSyncToken(1, 4); got != want {
		t.Fatalf("SyncToken = %q, want %q", got, want)
	}
}

func TestTakeCursorPage_AdvancesTokenWhenCursorHeadLagsObservedChanges(t *testing.T) {
	t.Parallel()

	svc := &SyncService{
		page: map[string]syncCursor{
			FormatSyncToken(1, 2): {
				AddressbookID: 1,
				HeadRevision:  2,
				ExpiresAt:     time.Now().Add(time.Minute),
				Items: []syncPageItem{
					{Revision: 3, Href: "a.vcf", ETagHex: "etag3"},
				},
			},
		},
	}

	out, ok := svc.takeCursorPage(SyncToken{AddressbookID: 1, Revision: 2}, "alice", "contacts", 10)
	if !ok {
		t.Fatal("takeCursorPage ok=false, want true")
	}
	if got, want := out.SyncToken, FormatSyncToken(1, 3); got != want {
		t.Fatalf("SyncToken = %q, want %q", got, want)
	}
}

func TestPutCursor_EnforcesGlobalCacheCaps(t *testing.T) {
	t.Parallel()

	oldMaxEntries := syncCursorCacheMaxEntries
	oldMaxItems := syncCursorCacheMaxTotalItems
	syncCursorCacheMaxEntries = 3
	syncCursorCacheMaxTotalItems = 5
	defer func() {
		syncCursorCacheMaxEntries = oldMaxEntries
		syncCursorCacheMaxTotalItems = oldMaxItems
	}()

	svc := &SyncService{page: make(map[string]syncCursor)}
	now := time.Now()
	for i := 0; i < 6; i++ {
		svc.putCursor(FormatSyncToken(1, int64(i+1)), syncCursor{
			AddressbookID: 1,
			HeadRevision:  int64(i + 1),
			ExpiresAt:     now.Add(time.Duration(i+1) * time.Minute),
			Items: []syncPageItem{
				{Revision: int64(i + 1), Href: "a.vcf"},
				{Revision: int64(i + 1), Href: "b.vcf"},
			},
		})
	}
	if got := len(svc.page); got > syncCursorCacheMaxEntries {
		t.Fatalf("cursor entries=%d exceeds cap=%d", got, syncCursorCacheMaxEntries)
	}
	if got := countCachedSyncItems(svc.page); got > syncCursorCacheMaxTotalItems {
		t.Fatalf("cached items=%d exceeds cap=%d", got, syncCursorCacheMaxTotalItems)
	}
}

func TestTakeCursorPage_ReinsertedContinuationEnforcesGlobalCacheCaps(t *testing.T) {
	t.Parallel()

	oldMaxEntries := syncCursorCacheMaxEntries
	oldMaxItems := syncCursorCacheMaxTotalItems
	syncCursorCacheMaxEntries = 1
	syncCursorCacheMaxTotalItems = 2
	defer func() {
		syncCursorCacheMaxEntries = oldMaxEntries
		syncCursorCacheMaxTotalItems = oldMaxItems
	}()

	now := time.Now()
	svc := &SyncService{
		page: map[string]syncCursor{
			FormatSyncToken(1, 1): {
				AddressbookID: 1,
				HeadRevision:  5,
				ExpiresAt:     now.Add(time.Minute),
				Items: []syncPageItem{
					{Revision: 2, Href: "a.vcf"},
					{Revision: 3, Href: "b.vcf"},
					{Revision: 4, Href: "c.vcf"},
					{Revision: 5, Href: "d.vcf"},
				},
			},
		},
	}

	out, ok := svc.takeCursorPage(SyncToken{AddressbookID: 1, Revision: 1}, "alice", "contacts", 1)
	if !ok {
		t.Fatal("takeCursorPage ok=false, want true")
	}
	if !out.Truncated {
		t.Fatal("out.Truncated=false, want true")
	}
	if got := len(svc.page); got > syncCursorCacheMaxEntries {
		t.Fatalf("cursor entries=%d exceeds cap=%d", got, syncCursorCacheMaxEntries)
	}
	if got := countCachedSyncItems(svc.page); got > syncCursorCacheMaxTotalItems {
		t.Fatalf("cached items=%d exceeds cap=%d", got, syncCursorCacheMaxTotalItems)
	}
}

func TestBuildPagedSyncResult_CachesContinuationBeyondLegacyThreshold(t *testing.T) {
	t.Parallel()

	svc := &SyncService{page: make(map[string]syncCursor)}
	items := make([]syncPageItem, syncCursorMaxItems+2)
	for i := range items {
		items[i] = syncPageItem{
			Revision: int64(i + 1),
			Href:     "a.vcf",
			ETagHex:  "etag",
		}
	}

	out, err := svc.buildPagedSyncResult(1, int64(len(items)), "alice", "contacts", items, 1)
	if err != nil {
		t.Fatalf("buildPagedSyncResult: %v", err)
	}
	if !out.Truncated {
		t.Fatal("out.Truncated=false, want true")
	}
	if _, ok := svc.page[out.SyncToken]; !ok {
		t.Fatalf("expected continuation cursor for token %q to be cached", out.SyncToken)
	}
}

func TestTakeCursorPage_ReinsertsContinuationBeyondLegacyThreshold(t *testing.T) {
	t.Parallel()

	now := time.Now()
	svc := &SyncService{
		page: map[string]syncCursor{
			FormatSyncToken(1, 1): {
				AddressbookID: 1,
				HeadRevision:  int64(syncCursorMaxItems + 3),
				ExpiresAt:     now.Add(time.Minute),
				Items:         make([]syncPageItem, syncCursorMaxItems+2),
			},
		},
	}
	for i := range svc.page[FormatSyncToken(1, 1)].Items {
		svc.page[FormatSyncToken(1, 1)].Items[i] = syncPageItem{
			Revision: int64(i + 2),
			Href:     "a.vcf",
			ETagHex:  "etag",
		}
	}

	out, ok := svc.takeCursorPage(SyncToken{AddressbookID: 1, Revision: 1}, "alice", "contacts", 1)
	if !ok {
		t.Fatal("takeCursorPage ok=false, want true")
	}
	if !out.Truncated {
		t.Fatal("out.Truncated=false, want true")
	}
	if _, stillOld := svc.page[FormatSyncToken(1, 1)]; stillOld {
		t.Fatal("old cursor token still present after take")
	}
	if _, ok := svc.page[out.SyncToken]; !ok {
		t.Fatalf("expected continuation cursor for token %q to be reinserted", out.SyncToken)
	}
}

func countCachedSyncItems(m map[string]syncCursor) int {
	total := 0
	for _, cur := range m {
		total += len(cur.Items)
	}
	return total
}
