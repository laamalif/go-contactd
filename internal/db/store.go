package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

const schemaVersion = 1

type Store struct {
	db    *sql.DB
	hooks TestHooks
	now   func() time.Time
}

var ErrNotFound = errors.New("not found")
var ErrPreconditionFailed = errors.New("precondition failed")

type TestHooks struct {
	BeforeCardChangeInsert func() error
}

type PutCardInput struct {
	AddressbookID int64
	Href          string
	UID           string
	VCard         []byte
}

type PutCardConditions struct {
	RequireAbsent          bool
	ExpectedCurrentETagHex *string
}

type PutCardResult struct {
	Created  bool
	ETagHex  string
	Revision int64
}

type CardChange struct {
	Href     string
	ETagHex  string
	Deleted  bool
	Revision int64
}

type CardSyncState struct {
	Href     string
	ETagHex  string
	Revision int64
}

type User struct {
	ID       int64
	Username string
}

type Addressbook struct {
	ID          int64
	UserID      int64
	Username    string
	Slug        string
	DisplayName string
	Description string
	Color       string
	Revision    int64
}

type Card struct {
	ID            int64
	AddressbookID int64
	Href          string
	UID           string
	ETagHex       string
	VCard         []byte
	ModTime       time.Time
}

func Open(ctx context.Context, path string) (*Store, error) {
	dbh, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	store := &Store{
		db:  dbh,
		now: time.Now,
	}

	if err := store.applyPragmas(ctx); err != nil {
		_ = dbh.Close()
		return nil, err
	}
	if err := store.migrate(ctx); err != nil {
		_ = dbh.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Ready(ctx context.Context) error {
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return fmt.Errorf("readiness query: %w", err)
	}
	if one != 1 {
		return fmt.Errorf("readiness query returned %d", one)
	}
	return nil
}

func (s *Store) SetTestHooks(h TestHooks) {
	s.hooks = h
}

func (s *Store) PragmaString(ctx context.Context, name string) (string, error) {
	var v string
	if err := s.db.QueryRowContext(ctx, fmt.Sprintf("PRAGMA %s;", name)).Scan(&v); err != nil {
		return "", fmt.Errorf("pragma %s: %w", name, err)
	}
	return v, nil
}

func (s *Store) PragmaInt(ctx context.Context, name string) (int, error) {
	var v int
	if err := s.db.QueryRowContext(ctx, fmt.Sprintf("PRAGMA %s;", name)).Scan(&v); err != nil {
		return 0, fmt.Errorf("pragma %s: %w", name, err)
	}
	return v, nil
}

func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO users (username, password_hash, created_at)
		VALUES (?, ?, ?)
	`, username, passwordHash, s.now().UTC())
	if err != nil {
		return 0, fmt.Errorf("insert user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("user last insert id: %w", err)
	}
	return id, nil
}

func (s *Store) UserCount(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

func (s *Store) UserIDByUsername(ctx context.Context, username string) (int64, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, username).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("select user id by username: %w", err)
	}
	return id, nil
}

func (s *Store) SetUserPasswordHash(ctx context.Context, userID int64, passwordHash string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, userID)
	if err != nil {
		return fmt.Errorf("update user password hash: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update user password rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, username FROM users ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username); err != nil {
			return nil, fmt.Errorf("scan user row: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return out, nil
}

func (s *Store) DeleteUserByUsername(ctx context.Context, username string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE username = ?`, username)
	if err != nil {
		return fmt.Errorf("delete user by username: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete user by username rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteUserByID(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete user by id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete user by id rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AuthenticateUser(ctx context.Context, username, password string) (bool, int64, error) {
	var (
		id           int64
		passwordHash string
	)
	if err := s.db.QueryRowContext(ctx, `
		SELECT id, password_hash
		FROM users
		WHERE username = ?
	`, username).Scan(&id, &passwordHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("select auth user: %w", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("compare password hash: %w", err)
	}
	return true, id, nil
}

func (s *Store) CreateAddressbook(ctx context.Context, userID int64, slug, displayname string) (int64, error) {
	now := s.now().UTC()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO addressbooks (user_id, slug, displayname, description, color, revision, created_at, updated_at)
		VALUES (?, ?, ?, '', '', 0, ?, ?)
	`, userID, slug, displayname, now, now)
	if err != nil {
		return 0, fmt.Errorf("insert addressbook: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("addressbook last insert id: %w", err)
	}
	return id, nil
}

func (s *Store) EnsureAddressbook(ctx context.Context, userID int64, slug, displayname string) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM addressbooks WHERE user_id = ? AND slug = ?
	`, userID, slug).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("select addressbook for ensure: %w", err)
	}
	id, err = s.CreateAddressbook(ctx, userID, slug, displayname)
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func (s *Store) HasAddressbook(ctx context.Context, username, slug string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM addressbooks ab
		JOIN users u ON u.id = ab.user_id
		WHERE u.username = ? AND ab.slug = ?
	`, username, slug).Scan(&n); err != nil {
		return false, fmt.Errorf("has addressbook: %w", err)
	}
	return n > 0, nil
}

func (s *Store) ListAddressbooksByUsername(ctx context.Context, username string) ([]Addressbook, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ab.id, ab.user_id, u.username, ab.slug, ab.displayname, ab.description, ab.color, ab.revision
		FROM addressbooks ab
		JOIN users u ON u.id = ab.user_id
		WHERE u.username = ?
		ORDER BY ab.id ASC
	`, username)
	if err != nil {
		return nil, fmt.Errorf("list addressbooks by username: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Addressbook
	for rows.Next() {
		var ab Addressbook
		if err := rows.Scan(&ab.ID, &ab.UserID, &ab.Username, &ab.Slug, &ab.DisplayName, &ab.Description, &ab.Color, &ab.Revision); err != nil {
			return nil, fmt.Errorf("scan addressbook row: %w", err)
		}
		out = append(out, ab)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate addressbooks: %w", err)
	}
	return out, nil
}

func (s *Store) GetAddressbookByUsernameSlug(ctx context.Context, username, slug string) (Addressbook, error) {
	var ab Addressbook
	err := s.db.QueryRowContext(ctx, `
		SELECT ab.id, ab.user_id, u.username, ab.slug, ab.displayname, ab.description, ab.color, ab.revision
		FROM addressbooks ab
		JOIN users u ON u.id = ab.user_id
		WHERE u.username = ? AND ab.slug = ?
	`, username, slug).Scan(&ab.ID, &ab.UserID, &ab.Username, &ab.Slug, &ab.DisplayName, &ab.Description, &ab.Color, &ab.Revision)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Addressbook{}, ErrNotFound
		}
		return Addressbook{}, fmt.Errorf("get addressbook by username/slug: %w", err)
	}
	return ab, nil
}

func (s *Store) UpdateAddressbookMetadataByUsernameSlug(ctx context.Context, username, slug string, displayname, description, color *string) error {
	if displayname == nil && description == nil && color == nil {
		return nil
	}
	now := s.now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE addressbooks
		SET
			displayname = CASE WHEN ? THEN ? ELSE displayname END,
			description = CASE WHEN ? THEN ? ELSE description END,
			color = CASE WHEN ? THEN ? ELSE color END,
			updated_at = CASE WHEN (? OR ? OR ?) THEN ? ELSE updated_at END
		WHERE id IN (
			SELECT ab.id
			FROM addressbooks ab
			JOIN users u ON u.id = ab.user_id
			WHERE u.username = ? AND ab.slug = ?
		)
	`,
		displayname != nil, stringValue(displayname),
		description != nil, stringValue(description),
		color != nil, stringValue(color),
		displayname != nil, description != nil, color != nil, now,
		username, slug,
	)
	if err != nil {
		return fmt.Errorf("update addressbook metadata by username/slug: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update addressbook metadata rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteAddressbookByUsernameSlug(ctx context.Context, username, slug string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM addressbooks
		WHERE id IN (
			SELECT ab.id
			FROM addressbooks ab
			JOIN users u ON u.id = ab.user_id
			WHERE u.username = ? AND ab.slug = ?
		)
	`, username, slug)
	if err != nil {
		return fmt.Errorf("delete addressbook by username/slug: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete addressbook rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func (s *Store) GetCard(ctx context.Context, addressbookID int64, href string) (Card, error) {
	var c Card
	err := s.db.QueryRowContext(ctx, `
		SELECT id, addressbook_id, href, uid, etag, vcard_text, mod_time
		FROM cards
		WHERE addressbook_id = ? AND href = ?
	`, addressbookID, href).Scan(&c.ID, &c.AddressbookID, &c.Href, &c.UID, &c.ETagHex, &c.VCard, &c.ModTime)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Card{}, ErrNotFound
		}
		return Card{}, fmt.Errorf("get card: %w", err)
	}
	return c, nil
}

func (s *Store) ListCards(ctx context.Context, addressbookID int64) ([]Card, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, addressbook_id, href, uid, etag, vcard_text, mod_time
		FROM cards
		WHERE addressbook_id = ?
		ORDER BY href ASC
	`, addressbookID)
	if err != nil {
		return nil, fmt.Errorf("list cards: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Card
	for rows.Next() {
		var c Card
		if err := rows.Scan(&c.ID, &c.AddressbookID, &c.Href, &c.UID, &c.ETagHex, &c.VCard, &c.ModTime); err != nil {
			return nil, fmt.Errorf("scan card row: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cards: %w", err)
	}
	return out, nil
}

func (s *Store) AddressbookRevision(ctx context.Context, addressbookID int64) (int64, error) {
	var rev int64
	if err := s.db.QueryRowContext(ctx, `SELECT revision FROM addressbooks WHERE id = ?`, addressbookID).Scan(&rev); err != nil {
		return 0, fmt.Errorf("select addressbook revision: %w", err)
	}
	return rev, nil
}

func (s *Store) CardCount(ctx context.Context, addressbookID int64) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cards WHERE addressbook_id = ?`, addressbookID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count cards: %w", err)
	}
	return n, nil
}

func (s *Store) CardChangeCount(ctx context.Context, addressbookID int64) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM card_changes WHERE addressbook_id = ?`, addressbookID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count card_changes: %w", err)
	}
	return n, nil
}

func (s *Store) LastCardChange(ctx context.Context, addressbookID int64) (CardChange, error) {
	var out CardChange
	var deleted int
	if err := s.db.QueryRowContext(ctx, `
		SELECT href, COALESCE(etag, ''), deleted, revision
		FROM card_changes
		WHERE addressbook_id = ?
		ORDER BY revision DESC, id DESC
		LIMIT 1
	`, addressbookID).Scan(&out.Href, &out.ETagHex, &deleted, &out.Revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CardChange{}, ErrNotFound
		}
		return CardChange{}, fmt.Errorf("select last card_change: %w", err)
	}
	out.Deleted = deleted != 0
	return out, nil
}

func (s *Store) CardChangeRevisions(ctx context.Context, addressbookID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT revision
		FROM card_changes
		WHERE addressbook_id = ?
		ORDER BY revision ASC, id ASC
	`, addressbookID)
	if err != nil {
		return nil, fmt.Errorf("select card_change revisions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var revs []int64
	for rows.Next() {
		var rev int64
		if err := rows.Scan(&rev); err != nil {
			return nil, fmt.Errorf("scan card_change revision: %w", err)
		}
		revs = append(revs, rev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate card_change revisions: %w", err)
	}
	return revs, nil
}

func (s *Store) ListCardChangesSince(ctx context.Context, addressbookID int64, afterRevision int64, limit int) ([]CardChange, error) {
	query := `
		SELECT href, COALESCE(etag, ''), deleted, revision
		FROM card_changes
		WHERE addressbook_id = ? AND revision > ?
		ORDER BY revision ASC, id ASC
	`
	args := []any{addressbookID, afterRevision}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list card_changes since: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CardChange
	for rows.Next() {
		var c CardChange
		var deleted int
		if err := rows.Scan(&c.Href, &c.ETagHex, &deleted, &c.Revision); err != nil {
			return nil, fmt.Errorf("scan card_change row: %w", err)
		}
		c.Deleted = deleted != 0
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate card_changes since: %w", err)
	}
	return out, nil
}

func (s *Store) ListCurrentCardSyncStates(ctx context.Context, addressbookID int64, limit int) ([]CardSyncState, error) {
	query := `
		SELECT c.href, c.etag, MAX(cc.revision) AS revision
		FROM cards c
		JOIN card_changes cc
		  ON cc.addressbook_id = c.addressbook_id
		 AND cc.href = c.href
		WHERE c.addressbook_id = ?
		GROUP BY c.id, c.href, c.etag
		ORDER BY revision ASC, c.href ASC
	`
	args := []any{addressbookID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list current card sync states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CardSyncState
	for rows.Next() {
		var c CardSyncState
		if err := rows.Scan(&c.Href, &c.ETagHex, &c.Revision); err != nil {
			return nil, fmt.Errorf("scan current card sync state: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate current card sync states: %w", err)
	}
	return out, nil
}

func (s *Store) PutCard(ctx context.Context, in PutCardInput) (PutCardResult, error) {
	return s.putCard(ctx, in, nil)
}

func (s *Store) PutCardConditional(ctx context.Context, in PutCardInput, cond PutCardConditions) (PutCardResult, error) {
	return s.putCard(ctx, in, &cond)
}

func (s *Store) putCard(ctx context.Context, in PutCardInput, cond *PutCardConditions) (PutCardResult, error) {
	if in.AddressbookID == 0 {
		return PutCardResult{}, fmt.Errorf("addressbook_id is required")
	}
	if in.Href == "" || in.UID == "" {
		return PutCardResult{}, fmt.Errorf("href and uid are required")
	}
	canonical := CanonicalizeVCard(in.VCard)
	etagHex := ComputeETagHex(canonical)
	now := s.now().UTC()

	var out PutCardResult
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var (
			existing     int
			existingETag sql.NullString
		)
		switch err := conn.QueryRowContext(ctx, `
			SELECT etag FROM cards WHERE addressbook_id = ? AND href = ?
		`, in.AddressbookID, in.Href).Scan(&existingETag); {
		case err == nil:
			existing = 1
		case errors.Is(err, sql.ErrNoRows):
			existing = 0
		default:
			return fmt.Errorf("check existing card: %w", err)
		}

		if cond != nil {
			if cond.RequireAbsent && existing != 0 {
				return ErrPreconditionFailed
			}
			if cond.ExpectedCurrentETagHex != nil {
				if existing == 0 || !existingETag.Valid || existingETag.String != *cond.ExpectedCurrentETagHex {
					return ErrPreconditionFailed
				}
			}
		}

		if existing == 0 {
			if _, err := conn.ExecContext(ctx, `
				INSERT INTO cards (addressbook_id, href, uid, etag, vcard_text, mod_time)
				VALUES (?, ?, ?, ?, ?, ?)
			`, in.AddressbookID, in.Href, in.UID, etagHex, canonical, now); err != nil {
				return fmt.Errorf("insert card: %w", err)
			}
			out.Created = true
		} else {
			if _, err := conn.ExecContext(ctx, `
				UPDATE cards
				SET uid = ?, etag = ?, vcard_text = ?, mod_time = ?
				WHERE addressbook_id = ? AND href = ?
			`, in.UID, etagHex, canonical, now, in.AddressbookID, in.Href); err != nil {
				return fmt.Errorf("update card: %w", err)
			}
			out.Created = false
		}

		if _, err := conn.ExecContext(ctx, `
			UPDATE addressbooks
			SET revision = revision + 1, updated_at = ?
			WHERE id = ?
		`, now, in.AddressbookID); err != nil {
			return fmt.Errorf("bump addressbook revision: %w", err)
		}
		if err := conn.QueryRowContext(ctx, `
			SELECT revision FROM addressbooks WHERE id = ?
		`, in.AddressbookID).Scan(&out.Revision); err != nil {
			return fmt.Errorf("select new revision: %w", err)
		}

		if s.hooks.BeforeCardChangeInsert != nil {
			if err := s.hooks.BeforeCardChangeInsert(); err != nil {
				return fmt.Errorf("before card_changes insert hook: %w", err)
			}
		}

		if _, err := conn.ExecContext(ctx, `
			INSERT INTO card_changes (addressbook_id, href, etag, deleted, revision, changed_at)
			VALUES (?, ?, ?, 0, ?, ?)
		`, in.AddressbookID, in.Href, etagHex, out.Revision, now); err != nil {
			return fmt.Errorf("insert card_change: %w", err)
		}

		out.ETagHex = etagHex
		return nil
	})
	if err != nil {
		return PutCardResult{}, err
	}
	return out, nil
}

func (s *Store) DeleteCard(ctx context.Context, addressbookID int64, href string) error {
	return s.deleteCard(ctx, addressbookID, href, nil)
}

func (s *Store) DeleteCardConditional(ctx context.Context, addressbookID int64, href, expectedCurrentETagHex string) error {
	return s.deleteCard(ctx, addressbookID, href, &expectedCurrentETagHex)
}

func (s *Store) deleteCard(ctx context.Context, addressbookID int64, href string, expectedCurrentETagHex *string) error {
	if addressbookID == 0 || href == "" {
		return fmt.Errorf("addressbook_id and href are required")
	}
	now := s.now().UTC()

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var lastETag string
		if err := conn.QueryRowContext(ctx, `
			SELECT etag FROM cards WHERE addressbook_id = ? AND href = ?
		`, addressbookID, href).Scan(&lastETag); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("select card etag for delete: %w", err)
		}
		if expectedCurrentETagHex != nil && lastETag != *expectedCurrentETagHex {
			return ErrPreconditionFailed
		}

		if _, err := conn.ExecContext(ctx, `
			DELETE FROM cards WHERE addressbook_id = ? AND href = ?
		`, addressbookID, href); err != nil {
			return fmt.Errorf("delete card: %w", err)
		}

		if _, err := conn.ExecContext(ctx, `
			UPDATE addressbooks
			SET revision = revision + 1, updated_at = ?
			WHERE id = ?
		`, now, addressbookID); err != nil {
			return fmt.Errorf("bump addressbook revision on delete: %w", err)
		}

		var rev int64
		if err := conn.QueryRowContext(ctx, `SELECT revision FROM addressbooks WHERE id = ?`, addressbookID).Scan(&rev); err != nil {
			return fmt.Errorf("select revision after delete: %w", err)
		}

		if _, err := conn.ExecContext(ctx, `
			INSERT INTO card_changes (addressbook_id, href, etag, deleted, revision, changed_at)
			VALUES (?, ?, ?, 1, ?, ?)
		`, addressbookID, href, lastETag, rev, now); err != nil {
			return fmt.Errorf("insert delete card_change: %w", err)
		}
		return nil
	})
}

func (s *Store) PruneCardChangesByAge(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM card_changes WHERE changed_at < ?`, cutoff.UTC())
	if err != nil {
		return 0, fmt.Errorf("prune card_changes by age: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune rows affected: %w", err)
	}
	return n, nil
}

func (s *Store) PruneCardChangesByMaxRevisions(ctx context.Context, keep int64) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM card_changes
		WHERE id IN (
			SELECT cc.id
			FROM card_changes cc
			JOIN addressbooks ab ON ab.id = cc.addressbook_id
			WHERE cc.revision <= (ab.revision - ?)
		)
	`, keep)
	if err != nil {
		return 0, fmt.Errorf("prune card_changes by max revisions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune by max revisions rows affected: %w", err)
	}
	return n, nil
}

func (s *Store) ForceCardChangesTimestamp(ctx context.Context, addressbookID int64, ts time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE card_changes SET changed_at = ? WHERE addressbook_id = ?`, ts.UTC(), addressbookID)
	if err != nil {
		return fmt.Errorf("force card_changes timestamp: %w", err)
	}
	return nil
}

func (s *Store) applyPragmas(ctx context.Context) error {
	stmts := []string{
		`PRAGMA foreign_keys = ON;`,
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA synchronous = NORMAL;`,
		`PRAGMA busy_timeout = 5000;`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply pragma %q: %w", stmt, err)
		}
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY
		);`,
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS addressbooks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			slug TEXT NOT NULL,
			displayname TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			color TEXT NOT NULL DEFAULT '',
			revision INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			UNIQUE (user_id, slug),
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS cards (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			addressbook_id INTEGER NOT NULL,
			href TEXT NOT NULL,
			uid TEXT NOT NULL,
			etag TEXT NOT NULL,
			vcard_text BLOB NOT NULL,
			mod_time TIMESTAMP NOT NULL,
			UNIQUE (addressbook_id, href),
			UNIQUE (addressbook_id, uid),
			FOREIGN KEY (addressbook_id) REFERENCES addressbooks(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS card_changes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			addressbook_id INTEGER NOT NULL,
			href TEXT NOT NULL,
			etag TEXT,
			deleted INTEGER NOT NULL,
			revision INTEGER NOT NULL,
			changed_at TIMESTAMP NOT NULL,
			FOREIGN KEY (addressbook_id) REFERENCES addressbooks(id) ON DELETE CASCADE
		);`,
		`CREATE INDEX IF NOT EXISTS idx_cards_addressbook_mod_time ON cards(addressbook_id, mod_time);`,
		`CREATE INDEX IF NOT EXISTS idx_cards_addressbook_uid ON cards(addressbook_id, uid);`,
		`CREATE INDEX IF NOT EXISTS idx_card_changes_addressbook_revision ON card_changes(addressbook_id, revision);`,
		`INSERT OR IGNORE INTO schema_migrations (version) VALUES (` + fmt.Sprintf("%d", schemaVersion) + `);`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration statement failed: %w", err)
		}
	}
	return nil
}

func (s *Store) withImmediateTx(ctx context.Context, fn func(*sql.Conn) error) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("db conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := s.applyConnPragmas(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
	}()

	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

func (s *Store) applyConnPragmas(ctx context.Context, conn *sql.Conn) error {
	stmts := []string{
		`PRAGMA foreign_keys = ON;`,
		`PRAGMA busy_timeout = 5000;`,
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply conn pragma %q: %w", stmt, err)
		}
	}
	return nil
}

func CanonicalizeVCard(in []byte) []byte {
	if len(in) == 0 {
		return nil
	}
	out := bytes.ReplaceAll(in, []byte("\r\n"), []byte("\n"))
	out = bytes.ReplaceAll(out, []byte("\r"), []byte("\n"))
	out = bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
	return out
}

func ComputeETagHex(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
