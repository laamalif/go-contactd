package carddavx

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/laamalif/go-contactd/internal/db"
)

const syncTokenPrefix = "urn:contactd:sync:"

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
}

type SyncService struct {
	store *db.Store
}

func NewSyncService(store *db.Store) *SyncService {
	return &SyncService{store: store}
}

func (s *SyncService) SyncCollection(ctx context.Context, username, slug, rawToken string, limit int) (SyncResult, error) {
	ab, err := s.store.GetAddressbookByUsernameSlug(ctx, username, slug)
	if err != nil {
		return SyncResult{}, err
	}
	currentToken := FormatSyncToken(ab.ID, ab.Revision)

	if strings.TrimSpace(rawToken) == "" {
		cards, err := s.store.ListCards(ctx, ab.ID)
		if err != nil {
			return SyncResult{}, err
		}
		out := SyncResult{
			SyncToken: currentToken,
			Updated:   make([]SyncRef, 0, len(cards)),
		}
		for _, c := range cards {
			out.Updated = append(out.Updated, SyncRef{
				Href: "/" + username + "/" + slug + "/" + c.Href,
				ETag: `"` + c.ETagHex + `"`,
			})
		}
		return out, nil
	}

	token, err := ParseSyncToken(strings.TrimSpace(rawToken))
	if err != nil {
		return SyncResult{}, &invalidSyncTokenError{cause: err}
	}
	if token.AddressbookID != ab.ID {
		return SyncResult{}, &invalidSyncTokenError{cause: fmt.Errorf("addressbook mismatch")}
	}
	if token.Revision > ab.Revision {
		return SyncResult{}, &invalidSyncTokenError{cause: fmt.Errorf("revision ahead")}
	}

	changes, err := s.store.ListCardChangesSince(ctx, ab.ID, token.Revision, limit)
	if err != nil {
		return SyncResult{}, err
	}
	tokenRevision := ab.Revision
	if len(changes) > 0 {
		lastRevision := changes[len(changes)-1].Revision
		if lastRevision < tokenRevision {
			tokenRevision = lastRevision
		}
	}
	out := SyncResult{SyncToken: FormatSyncToken(ab.ID, tokenRevision)}
	for _, ch := range changes {
		fullHref := "/" + username + "/" + slug + "/" + ch.Href
		if ch.Deleted {
			out.Deleted = append(out.Deleted, fullHref)
			continue
		}
		out.Updated = append(out.Updated, SyncRef{
			Href: fullHref,
			ETag: `"` + ch.ETagHex + `"`,
		})
	}
	return out, nil
}
