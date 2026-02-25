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
