package carddav

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/emersion/go-vcard"
	webdav "github.com/emersion/go-webdav"
	gocarddav "github.com/emersion/go-webdav/carddav"
	"github.com/laamalif/go-contactd/internal/db"
)

var _ gocarddav.Backend = (*Backend)(nil)

type Backend struct {
	store *db.Store
}

func NewBackend(store *db.Store) *Backend {
	return &Backend{store: store}
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, username string) context.Context {
	return context.WithValue(ctx, principalKey{}, username)
}

func principalFromContext(ctx context.Context) (string, error) {
	v, _ := ctx.Value(principalKey{}).(string)
	if strings.TrimSpace(v) == "" {
		return "", webdav.NewHTTPError(http.StatusUnauthorized, fmt.Errorf("missing principal"))
	}
	return v, nil
}

func (b *Backend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	user, err := principalFromContext(ctx)
	if err != nil {
		return "", err
	}
	return "/" + user + "/", nil
}

func (b *Backend) AddressBookHomeSetPath(ctx context.Context) (string, error) {
	return b.CurrentUserPrincipal(ctx)
}

func (b *Backend) ListAddressBooks(ctx context.Context) ([]gocarddav.AddressBook, error) {
	user, err := principalFromContext(ctx)
	if err != nil {
		return nil, err
	}
	abs, err := b.store.ListAddressbooksByUsername(ctx, user)
	if err != nil {
		return nil, err
	}
	out := make([]gocarddav.AddressBook, 0, len(abs))
	for _, ab := range abs {
		out = append(out, toAddressBook(ab))
	}
	return out, nil
}

func (b *Backend) GetAddressBook(ctx context.Context, p string) (*gocarddav.AddressBook, error) {
	user, slug, err := parseAddressbookPathForPrincipal(ctx, p)
	if err != nil {
		return nil, err
	}
	ab, err := b.store.GetAddressbookByUsernameSlug(ctx, user, slug)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	out := toAddressBook(ab)
	return &out, nil
}

func (b *Backend) CreateAddressBook(ctx context.Context, addressBook *gocarddav.AddressBook) error {
	if addressBook == nil {
		return webdav.NewHTTPError(http.StatusBadRequest, fmt.Errorf("nil addressbook"))
	}
	user, slug, err := parseAddressbookPathForPrincipal(ctx, addressBook.Path)
	if err != nil {
		return err
	}
	userID, err := b.store.UserIDByUsername(ctx, user)
	if err != nil {
		return mapStoreErr(err)
	}
	if _, _, err := b.store.EnsureAddressbook(ctx, userID, slug, defaultString(addressBook.Name, slug)); err != nil {
		if isUniqueErr(err) {
			return webdav.NewHTTPError(http.StatusMethodNotAllowed, err)
		}
		return mapStoreErr(err)
	}
	return nil
}

func (b *Backend) DeleteAddressBook(ctx context.Context, p string) error {
	user, slug, err := parseAddressbookPathForPrincipal(ctx, p)
	if err != nil {
		return err
	}
	if err := b.store.DeleteAddressbookByUsernameSlug(ctx, user, slug); err != nil {
		return mapStoreErr(err)
	}
	return nil
}

func (b *Backend) UpdateAddressBookMetadata(ctx context.Context, p string, displayname, description, color *string) error {
	user, slug, err := parseAddressbookPathForPrincipal(ctx, p)
	if err != nil {
		return err
	}
	if err := b.store.UpdateAddressbookMetadataByUsernameSlug(ctx, user, slug, displayname, description, color); err != nil {
		return mapStoreErr(err)
	}
	return nil
}

func (b *Backend) GetAddressBookColor(ctx context.Context, p string) (string, error) {
	user, slug, err := parseAddressbookPathForPrincipal(ctx, p)
	if err != nil {
		return "", err
	}
	ab, err := b.store.GetAddressbookByUsernameSlug(ctx, user, slug)
	if err != nil {
		return "", mapStoreErr(err)
	}
	return ab.Color, nil
}

func (b *Backend) GetAddressObject(ctx context.Context, p string, _ *gocarddav.AddressDataRequest) (*gocarddav.AddressObject, error) {
	user, slug, href, err := parseCardPathForPrincipal(ctx, p)
	if err != nil {
		return nil, err
	}
	ab, err := b.store.GetAddressbookByUsernameSlug(ctx, user, slug)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	row, err := b.store.GetCard(ctx, ab.ID, href)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	ao, err := toAddressObject(user, slug, row)
	if err != nil {
		return nil, webdav.NewHTTPError(http.StatusInternalServerError, err)
	}
	return &ao, nil
}

func (b *Backend) ListAddressObjects(ctx context.Context, p string, _ *gocarddav.AddressDataRequest) ([]gocarddav.AddressObject, error) {
	user, slug, err := parseAddressbookPathForPrincipal(ctx, p)
	if err != nil {
		return nil, err
	}
	ab, err := b.store.GetAddressbookByUsernameSlug(ctx, user, slug)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	rows, err := b.store.ListCards(ctx, ab.ID)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	out := make([]gocarddav.AddressObject, 0, len(rows))
	for _, row := range rows {
		ao, err := toAddressObject(user, slug, row)
		if err != nil {
			return nil, webdav.NewHTTPError(http.StatusInternalServerError, err)
		}
		out = append(out, ao)
	}
	return out, nil
}

func (b *Backend) QueryAddressObjects(ctx context.Context, p string, query *gocarddav.AddressBookQuery) ([]gocarddav.AddressObject, error) {
	if query == nil {
		return b.ListAddressObjects(ctx, p, nil)
	}
	aos, err := b.ListAddressObjects(ctx, p, &query.DataRequest)
	if err != nil {
		return nil, err
	}
	return gocarddav.Filter(query, aos)
}

func (b *Backend) PutAddressObject(ctx context.Context, p string, card vcard.Card, opts *gocarddav.PutAddressObjectOptions) (*gocarddav.AddressObject, error) {
	user, slug, href, err := parseCardPathForPrincipal(ctx, p)
	if err != nil {
		return nil, err
	}
	ab, err := b.store.GetAddressbookByUsernameSlug(ctx, user, slug)
	if err != nil {
		return nil, mapStoreErr(err)
	}

	var existing *db.Card
	if row, err := b.store.GetCard(ctx, ab.ID, href); err == nil {
		existing = &row
	} else if !errors.Is(err, db.ErrNotFound) {
		return nil, mapStoreErr(err)
	}

	if err := checkConditional(existing, opts); err != nil {
		return nil, err
	}

	uid := strings.TrimSpace(card.PreferredValue(vcard.FieldUID))
	if uid == "" {
		return nil, webdav.NewHTTPError(http.StatusBadRequest, fmt.Errorf("missing UID"))
	}
	raw, err := encodeCard(card)
	if err != nil {
		return nil, webdav.NewHTTPError(http.StatusBadRequest, fmt.Errorf("encode vcard: %w", err))
	}

	if _, err := b.store.PutCard(ctx, db.PutCardInput{
		AddressbookID: ab.ID,
		Href:          href,
		UID:           uid,
		VCard:         raw,
	}); err != nil {
		if isUniqueErr(err) {
			return nil, gocarddav.NewPreconditionError(gocarddav.PreconditionNoUIDConflict)
		}
		return nil, mapStoreErr(err)
	}

	row, err := b.store.GetCard(ctx, ab.ID, href)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	ao, err := toAddressObject(user, slug, row)
	if err != nil {
		return nil, webdav.NewHTTPError(http.StatusInternalServerError, err)
	}
	return &ao, nil
}

func (b *Backend) DeleteAddressObject(ctx context.Context, p string) error {
	user, slug, href, err := parseCardPathForPrincipal(ctx, p)
	if err != nil {
		return err
	}
	ab, err := b.store.GetAddressbookByUsernameSlug(ctx, user, slug)
	if err != nil {
		return mapStoreErr(err)
	}
	if err := b.store.DeleteCard(ctx, ab.ID, href); err != nil {
		return mapStoreErr(err)
	}
	return nil
}

func toAddressBook(ab db.Addressbook) gocarddav.AddressBook {
	return gocarddav.AddressBook{
		Path:        "/" + ab.Username + "/" + ab.Slug + "/",
		Name:        ab.DisplayName,
		Description: ab.Description,
		SupportedAddressData: []gocarddav.AddressDataType{
			{ContentType: "text/vcard", Version: "3.0"},
		},
	}
}

func toAddressObject(user, slug string, row db.Card) (gocarddav.AddressObject, error) {
	card, err := decodeCard(row.VCard)
	if err != nil {
		return gocarddav.AddressObject{}, err
	}
	return gocarddav.AddressObject{
		Path:          "/" + user + "/" + slug + "/" + row.Href,
		ModTime:       row.ModTime,
		ContentLength: int64(len(row.VCard)),
		ETag:          quoteETag(row.ETagHex),
		Card:          card,
	}, nil
}

func encodeCard(card vcard.Card) ([]byte, error) {
	var buf bytes.Buffer
	if err := vcard.NewEncoder(&buf).Encode(card); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeCard(raw []byte) (vcard.Card, error) {
	return vcard.NewDecoder(bytes.NewReader(raw)).Decode()
}

func checkConditional(existing *db.Card, opts *gocarddav.PutAddressObjectOptions) error {
	if opts == nil {
		return nil
	}
	if opts.IfNoneMatch.IsSet() {
		if existing != nil {
			ok, err := opts.IfNoneMatch.MatchETag(existing.ETagHex)
			if err != nil {
				return webdav.NewHTTPError(http.StatusBadRequest, err)
			}
			if opts.IfNoneMatch.IsWildcard() || ok {
				return webdav.NewHTTPError(http.StatusPreconditionFailed, fmt.Errorf("If-None-Match condition failed"))
			}
		}
	}
	if opts.IfMatch.IsSet() {
		if existing == nil {
			return webdav.NewHTTPError(http.StatusPreconditionFailed, fmt.Errorf("If-Match condition failed"))
		}
		ok, err := opts.IfMatch.MatchETag(existing.ETagHex)
		if err != nil {
			return webdav.NewHTTPError(http.StatusBadRequest, err)
		}
		if !ok {
			return webdav.NewHTTPError(http.StatusPreconditionFailed, fmt.Errorf("If-Match condition failed"))
		}
	}
	return nil
}

func parseAddressbookPathForPrincipal(ctx context.Context, p string) (user, slug string, _ error) {
	principal, err := principalFromContext(ctx)
	if err != nil {
		return "", "", err
	}
	clean := path.Clean("/" + strings.TrimSpace(p))
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}
	trim := strings.Trim(clean, "/")
	parts := strings.Split(trim, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("addressbook path not found"))
	}
	if parts[0] != principal {
		return "", "", webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("cross-tenant addressbook path"))
	}
	return parts[0], parts[1], nil
}

func parseCardPathForPrincipal(ctx context.Context, p string) (user, slug, href string, _ error) {
	principal, err := principalFromContext(ctx)
	if err != nil {
		return "", "", "", err
	}
	clean := path.Clean("/" + strings.TrimSpace(p))
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}
	trim := strings.Trim(clean, "/")
	parts := strings.Split(trim, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("card path not found"))
	}
	if parts[0] != principal {
		return "", "", "", webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("cross-tenant card path"))
	}
	if strings.Contains(parts[2], "/") || strings.Contains(parts[2], `\`) || parts[2] == "." || parts[2] == ".." {
		return "", "", "", webdav.NewHTTPError(http.StatusBadRequest, fmt.Errorf("invalid href"))
	}
	return parts[0], parts[1], parts[2], nil
}

func quoteETag(hex string) string {
	return `"` + hex + `"`
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func isUniqueErr(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "unique")
}

func mapStoreErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, db.ErrNotFound) {
		return webdav.NewHTTPError(http.StatusNotFound, err)
	}
	return err
}

// Keep import of time stable for go-webdav AddressObject docs/usage and future use.
var _ = time.Time{}
