package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/emersion/go-vcard"
	"github.com/laamalif/go-contactd/internal/config"
	"github.com/laamalif/go-contactd/internal/db"
	"golang.org/x/crypto/bcrypt"
)

type VersionRunner func(prog string, args []string, stdout, stderr io.Writer) int

func RunCLI(prog string, args []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer, runVersion VersionRunner) int {
	if len(args) == 0 {
		printAdminUsage(stderr, prog)
		return 2
	}
	if args[0] == "--version" || args[0] == "-V" {
		if runVersion == nil {
			_, _ = fmt.Fprintln(stderr, "internal error: version handler unavailable")
			return 1
		}
		return runVersion(prog, nil, stdout, stderr)
	}
	if isHelpToken(args[0]) || args[0] == "help" {
		printAdminHelp(stdout, prog)
		return 0
	}
	switch args[0] {
	case "user":
		return RunUser(prog, args[1:], env, stdin, stdout, stderr)
	case "export":
		return runExport(args[1:], env, stdout, stderr)
	case "import":
		return runImport(args[1:], env, stdout, stderr)
	case "version":
		if runVersion == nil {
			_, _ = fmt.Fprintln(stderr, "internal error: version handler unavailable")
			return 1
		}
		return runVersion(prog, args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "%s: unknown command: %s\n", defaultProg(prog), args[0])
		printAdminUsage(stderr, defaultProg(prog))
		return 2
	}
}

func RunUser(prog string, args []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUserUsage(stderr)
		return 2
	}
	if isHelpToken(args[0]) || args[0] == "help" {
		printUserHelp(stdout)
		return 0
	}

	switch args[0] {
	case "add":
		return runUserAdd(args[1:], env, stdin, stdout, stderr)
	case "list":
		return runUserList(args[1:], env, stdout, stderr)
	case "delete":
		return runUserDelete(args[1:], env, stdout, stderr)
	case "passwd":
		return runUserPasswd(args[1:], env, stdin, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "%s: unknown user subcommand: %s\n", defaultProg(prog), args[0])
		printUserUsage(stderr)
		return 2
	}
}

func isHelpToken(s string) bool {
	return s == "-h" || s == "--help"
}

func defaultProg(prog string) string {
	if strings.TrimSpace(prog) == "" {
		return "contactctl"
	}
	return prog
}

func printAdminUsage(w io.Writer, prog string) {
	_, _ = fmt.Fprintf(w, "usage: %s <user|export|import|version>\n", defaultProg(prog))
}

func printAdminHelp(w io.Writer, prog string) {
	_, _ = fmt.Fprintf(w, "usage: %s <user|export|import|version>\n", defaultProg(prog))
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "commands:")
	_, _ = fmt.Fprintln(w, "  user add      create user")
	_, _ = fmt.Fprintln(w, "  user list     list users")
	_, _ = fmt.Fprintln(w, "  user delete   delete user")
	_, _ = fmt.Fprintln(w, "  user passwd   change user password")
	_, _ = fmt.Fprintln(w, "  export        export addressbook vCards")
	_, _ = fmt.Fprintln(w, "  import        import vCards into an addressbook")
	_, _ = fmt.Fprintln(w, "  version       print version")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "run 'contactctl user -h', 'contactctl export -h', or 'contactctl import -h' for details")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "flags:")
	_, _ = fmt.Fprintln(w, "  -V, --version  print version and exit")
	_, _ = fmt.Fprintln(w, "  -h, --help     print help and exit")
}

func printUserUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: contactctl user <add|list|delete|passwd>")
}

func printUserHelp(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: contactctl user <add|list|delete|passwd>")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "subcommands:")
	_, _ = fmt.Fprintln(w, "  add     create a user (use --password-stdin to avoid argv leaks)")
	_, _ = fmt.Fprintln(w, "  list    list users")
	_, _ = fmt.Fprintln(w, "  delete  delete a user by --username or --id")
	_, _ = fmt.Fprintln(w, "  passwd  update a user password (supports --password-stdin)")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "notes:")
	_, _ = fmt.Fprintln(w, "  --db is a short alias for --db-path on all user subcommands")
}

func printUserAddHelp(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: contactctl user add --username <name> (--password <pw> | --password-stdin) [--db-path <path>|--db <path>|-d <path>] [--default-book-slug <slug>] [--default-book-name <name>]")
}

func printUserListHelp(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: contactctl user list [--db-path <path>|--db <path>|-d <path>] [--format table|json]")
}

func printUserDeleteHelp(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: contactctl user delete (--username <name> | --id <id>) [--db-path <path>|--db <path>|-d <path>]")
}

func printUserPasswdHelp(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: contactctl user passwd (--username <name> | --id <id>) (--password <pw> | --password-stdin) [--db-path <path>|--db <path>|-d <path>]")
}

func printExportHelp(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: contactctl export --username <name> [--book <slug>] [--format dir|concat] [--out <path>] [--dry-run] [--db-path <path>|--db <path>|-d <path>]")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "formats:")
	_, _ = fmt.Fprintln(w, "  dir     write one <href>.vcf file per card into --out directory (default)")
	_, _ = fmt.Fprintln(w, "  concat  write concatenated stored vCards to stdout (or --out file if set)")
	_, _ = fmt.Fprintln(w, "  --dry-run  validate and summarize without writing files or payloads")
}

func printImportHelp(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: contactctl import --username <name> [--book <slug>] [--dry-run] [--db-path <path>|--db <path>|-d <path>] <file-or-dir>")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "behavior:")
	_, _ = fmt.Fprintln(w, "  directory input: imports *.vcf files, href is the filename")
	_, _ = fmt.Fprintln(w, "  file input: imports concatenated vCards, href is <UID>.vcf")
	_, _ = fmt.Fprintln(w, "  --dry-run: parse/validate and summarize without writing to the database")
}

func runUserAdd(args []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && (isHelpToken(args[0]) || args[0] == "help") {
		printUserAddHelp(stdout)
		return 0
	}
	fs := newCLIFlagSet("user add")
	var (
		dbPath   = defaultCLIOpt(env["CONTACTD_DB_PATH"], config.DefaultDBPath)
		username string
		password string
		pwStdin  bool
		bookSlug = defaultCLIOpt(env["CONTACTD_DEFAULT_BOOK_SLUG"], "contacts")
		bookName = defaultCLIOpt(env["CONTACTD_DEFAULT_BOOK_NAME"], "Contacts")
	)
	fs.StringVar(&dbPath, "db-path", dbPath, "sqlite db path")
	fs.StringVar(&dbPath, "db", dbPath, "alias for --db-path")
	fs.StringVar(&dbPath, "d", dbPath, "alias for --db-path")
	fs.StringVar(&username, "username", "", "username")
	fs.StringVar(&password, "password", "", "password")
	fs.BoolVar(&pwStdin, "password-stdin", false, "read password from stdin (safer than argv)")
	fs.StringVar(&bookSlug, "default-book-slug", bookSlug, "default addressbook slug")
	fs.StringVar(&bookName, "default-book-name", bookName, "default addressbook name")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage error: unexpected positional arguments")
		return 2
	}
	if strings.TrimSpace(username) == "" {
		_, _ = fmt.Fprintln(stderr, "usage error: missing required --username")
		return 2
	}
	if err := validateUsername(username); err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}
	var err error
	password, err = resolvePasswordInput(password, pwStdin, stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}

	store, err := db.Open(context.Background(), dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	id, err := store.CreateUser(context.Background(), username, string(hash))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			_, _ = fmt.Fprintf(stderr, "usage error: username already exists: %s\n", username)
			return 2
		}
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	if _, _, err := store.EnsureAddressbook(context.Background(), id, bookSlug, bookName); err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "user added: id=%d username=%s\n", id, username)
	return 0
}

func runUserList(args []string, env map[string]string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (isHelpToken(args[0]) || args[0] == "help") {
		printUserListHelp(stdout)
		return 0
	}
	fs := newCLIFlagSet("user list")
	dbPath := defaultCLIOpt(env["CONTACTD_DB_PATH"], config.DefaultDBPath)
	format := "table"
	fs.StringVar(&dbPath, "db-path", dbPath, "sqlite db path")
	fs.StringVar(&dbPath, "db", dbPath, "alias for --db-path")
	fs.StringVar(&dbPath, "d", dbPath, "alias for --db-path")
	fs.StringVar(&format, "format", format, "table|json")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage error: unexpected positional arguments")
		return 2
	}
	if format != "table" && format != "json" {
		_, _ = fmt.Fprintf(stderr, "usage error: invalid --format %q\n", format)
		return 2
	}

	store, err := db.Open(context.Background(), dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	users, err := store.ListUsers(context.Background())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}

	if format == "json" {
		type outUser struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		}
		out := make([]outUser, 0, len(users))
		for _, u := range users {
			out = append(out, outUser{ID: u.ID, Username: u.Username})
		}
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(out); err != nil {
			_, _ = fmt.Fprintf(stderr, "internal error: %v\n", err)
			return 1
		}
		return 0
	}

	_, _ = fmt.Fprintln(stdout, "ID\tUSERNAME")
	for _, u := range users {
		_, _ = fmt.Fprintf(stdout, "%d\t%s\n", u.ID, u.Username)
	}
	return 0
}

func runUserDelete(args []string, env map[string]string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (isHelpToken(args[0]) || args[0] == "help") {
		printUserDeleteHelp(stdout)
		return 0
	}
	fs := newCLIFlagSet("user delete")
	dbPath := defaultCLIOpt(env["CONTACTD_DB_PATH"], config.DefaultDBPath)
	var username string
	var id int64
	fs.StringVar(&dbPath, "db-path", dbPath, "sqlite db path")
	fs.StringVar(&dbPath, "db", dbPath, "alias for --db-path")
	fs.StringVar(&dbPath, "d", dbPath, "alias for --db-path")
	fs.StringVar(&username, "username", "", "username")
	fs.Int64Var(&id, "id", 0, "user id")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage error: unexpected positional arguments")
		return 2
	}
	if (username == "" && id == 0) || (username != "" && id != 0) {
		_, _ = fmt.Fprintln(stderr, "usage error: specify exactly one of --username or --id")
		return 2
	}

	store, err := db.Open(context.Background(), dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	if username != "" {
		err = store.DeleteUserByUsername(context.Background(), username)
	} else {
		err = store.DeleteUserByID(context.Background(), id)
	}
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			_, _ = fmt.Fprintln(stderr, "not found")
			return 3
		}
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}

	if username != "" {
		_, _ = fmt.Fprintf(stdout, "user deleted: username=%s\n", username)
	} else {
		_, _ = fmt.Fprintf(stdout, "user deleted: id=%d\n", id)
	}
	return 0
}

func runUserPasswd(args []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && (isHelpToken(args[0]) || args[0] == "help") {
		printUserPasswdHelp(stdout)
		return 0
	}
	fs := newCLIFlagSet("user passwd")
	dbPath := defaultCLIOpt(env["CONTACTD_DB_PATH"], config.DefaultDBPath)
	var username string
	var id int64
	var password string
	var pwStdin bool
	fs.StringVar(&dbPath, "db-path", dbPath, "sqlite db path")
	fs.StringVar(&dbPath, "db", dbPath, "alias for --db-path")
	fs.StringVar(&dbPath, "d", dbPath, "alias for --db-path")
	fs.StringVar(&username, "username", "", "username")
	fs.Int64Var(&id, "id", 0, "user id")
	fs.StringVar(&password, "password", "", "password")
	fs.BoolVar(&pwStdin, "password-stdin", false, "read password from stdin (safer than argv)")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage error: unexpected positional arguments")
		return 2
	}
	if (username == "" && id == 0) || (username != "" && id != 0) {
		_, _ = fmt.Fprintln(stderr, "usage error: specify exactly one of --username or --id")
		return 2
	}
	var err error
	password, err = resolvePasswordInput(password, pwStdin, stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}

	store, err := db.Open(context.Background(), dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	if username != "" {
		var userID int64
		userID, err = store.UserIDByUsername(context.Background(), username)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				_, _ = fmt.Fprintln(stderr, "not found")
				return 3
			}
			_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
			return 1
		}
		err = store.SetUserPasswordHash(context.Background(), userID, string(hash))
	} else {
		err = store.SetUserPasswordHash(context.Background(), id, string(hash))
	}
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			_, _ = fmt.Fprintln(stderr, "not found")
			return 3
		}
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}

	if username != "" {
		_, _ = fmt.Fprintf(stdout, "user password updated: username=%s\n", username)
	} else {
		_, _ = fmt.Fprintf(stdout, "user password updated: id=%d\n", id)
	}
	return 0
}

func runExport(args []string, env map[string]string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (isHelpToken(args[0]) || args[0] == "help") {
		printExportHelp(stdout)
		return 0
	}
	fs := newCLIFlagSet("export")
	dbPath := defaultCLIOpt(env["CONTACTD_DB_PATH"], config.DefaultDBPath)
	username := ""
	book := "contacts"
	format := "dir"
	outPath := ""
	dryRun := false
	fs.StringVar(&dbPath, "db-path", dbPath, "sqlite db path")
	fs.StringVar(&dbPath, "db", dbPath, "alias for --db-path")
	fs.StringVar(&dbPath, "d", dbPath, "alias for --db-path")
	fs.StringVar(&username, "username", "", "username")
	fs.StringVar(&book, "book", book, "addressbook slug")
	fs.StringVar(&format, "format", format, "dir|concat")
	fs.StringVar(&outPath, "out", outPath, "output directory (dir) or file path (concat)")
	fs.BoolVar(&dryRun, "dry-run", false, "validate and summarize without writing output")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage error: unexpected positional arguments")
		return 2
	}
	if strings.TrimSpace(username) == "" {
		_, _ = fmt.Fprintln(stderr, "usage error: missing required --username")
		return 2
	}
	if format != "dir" && format != "concat" {
		_, _ = fmt.Fprintf(stderr, "usage error: invalid --format %q\n", format)
		return 2
	}
	if format == "dir" && strings.TrimSpace(outPath) == "" {
		_, _ = fmt.Fprintln(stderr, "usage error: --out is required for --format dir")
		return 2
	}

	store, err := db.Open(context.Background(), dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	ab, err := store.GetAddressbookByUsernameSlug(context.Background(), username, book)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			_, _ = fmt.Fprintln(stderr, "not found")
			return 3
		}
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	cards, err := store.ListCards(context.Background(), ab.ID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	totalBytes := 0
	for _, c := range cards {
		totalBytes += len(c.VCard)
	}
	if dryRun {
		switch format {
		case "concat":
			_, _ = fmt.Fprintf(stdout, "dry-run: user=%s book=%s cards=%d total_bytes=%d format=concat out=%s\n", username, book, len(cards), totalBytes, outPath)
			return 0
		default:
			for _, c := range cards {
				if _, err := safeExportCardFilename(c.Href); err != nil {
					_, _ = fmt.Fprintf(stderr, "io error: %v\n", err)
					return 1
				}
			}
			_, _ = fmt.Fprintf(stdout, "dry-run: user=%s book=%s cards=%d total_bytes=%d format=dir out=%s\n", username, book, len(cards), totalBytes, outPath)
			return 0
		}
	}

	switch format {
	case "concat":
		if strings.TrimSpace(outPath) == "" {
			for _, c := range cards {
				if _, err := stdout.Write(c.VCard); err != nil {
					_, _ = fmt.Fprintf(stderr, "internal error: %v\n", err)
					return 1
				}
			}
			return 0
		}
		if err := writeConcatExportFile(outPath, cards); err != nil {
			_, _ = fmt.Fprintf(stderr, "io error: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "exported: user=%s book=%s cards=%d format=concat out=%s\n", username, book, len(cards), outPath)
		return 0
	default: // dir
		if err := writeDirExport(outPath, cards); err != nil {
			_, _ = fmt.Fprintf(stderr, "io error: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "exported: user=%s book=%s cards=%d format=dir out=%s\n", username, book, len(cards), outPath)
		return 0
	}
}

func writeConcatExportFile(outPath string, cards []db.Card) error {
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open concat export file: %w", err)
	}
	defer func() { _ = f.Close() }()
	for _, c := range cards {
		if _, err := f.Write(c.VCard); err != nil {
			return fmt.Errorf("write concat export file: %w", err)
		}
	}
	return nil
}

func writeDirExport(outDir string, cards []db.Card) error {
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("mkdir export dir: %w", err)
	}
	for _, c := range cards {
		name, err := safeExportCardFilename(c.Href)
		if err != nil {
			return err
		}
		path := filepath.Join(outDir, name)
		if err := os.WriteFile(path, c.VCard, 0o600); err != nil {
			return fmt.Errorf("write export file %s: %w", name, err)
		}
	}
	return nil
}

func safeExportCardFilename(href string) (string, error) {
	if strings.TrimSpace(href) == "" {
		return "", fmt.Errorf("invalid card href for export: empty")
	}
	if strings.Contains(href, "/") || strings.Contains(href, `\`) {
		return "", fmt.Errorf("invalid card href for export: %q", href)
	}
	name := filepath.Base(href)
	if name == "." || name == ".." || name != href {
		return "", fmt.Errorf("invalid card href for export: %q", href)
	}
	return name, nil
}

func runImport(args []string, env map[string]string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (isHelpToken(args[0]) || args[0] == "help") {
		printImportHelp(stdout)
		return 0
	}
	fs := newCLIFlagSet("import")
	dbPath := defaultCLIOpt(env["CONTACTD_DB_PATH"], config.DefaultDBPath)
	username := ""
	book := "contacts"
	dryRun := false
	fs.StringVar(&dbPath, "db-path", dbPath, "sqlite db path")
	fs.StringVar(&dbPath, "db", dbPath, "alias for --db-path")
	fs.StringVar(&dbPath, "d", dbPath, "alias for --db-path")
	fs.StringVar(&username, "username", "", "username")
	fs.StringVar(&book, "book", book, "addressbook slug")
	fs.BoolVar(&dryRun, "dry-run", false, "validate and summarize without writing to database")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "usage error: %v\n", err)
		return 2
	}
	if strings.TrimSpace(username) == "" {
		_, _ = fmt.Fprintln(stderr, "usage error: missing required --username")
		return 2
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "usage error: expected exactly one <file-or-dir> path")
		return 2
	}
	srcPath := fs.Arg(0)

	store, err := db.Open(context.Background(), dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	ab, err := store.GetAddressbookByUsernameSlug(context.Background(), username, book)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			_, _ = fmt.Fprintln(stderr, "not found")
			return 3
		}
		_, _ = fmt.Fprintf(stderr, "db error: %v\n", err)
		return 1
	}

	st, err := os.Stat(srcPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "io error: stat import path: %v\n", err)
		return 1
	}
	var created, updated int
	if st.IsDir() {
		created, updated, err = importFromDir(context.Background(), store, ab.ID, srcPath, dryRun)
	} else {
		created, updated, err = importFromConcatFile(context.Background(), store, ab.ID, srcPath, dryRun)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "import error: %v\n", err)
		return 1
	}
	if dryRun {
		_, _ = fmt.Fprintf(stdout, "dry-run: user=%s book=%s created=%d updated=%d path=%s\n", username, book, created, updated, srcPath)
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "imported: user=%s book=%s created=%d updated=%d path=%s\n", username, book, created, updated, srcPath)
	return 0
}

func importFromDir(ctx context.Context, store *db.Store, addressbookID int64, dir string, dryRun bool) (int, int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, fmt.Errorf("read import dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".vcf") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var created, updated int
	for _, name := range names {
		href, err := safeExportCardFilename(name)
		if err != nil {
			return 0, 0, err
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return 0, 0, fmt.Errorf("read import file %s: %w", name, err)
		}
		card, err := decodeSingleCardBytes(raw)
		if err != nil {
			return 0, 0, fmt.Errorf("decode import file %s: %w", name, err)
		}
		uid := strings.TrimSpace(card.PreferredValue(vcard.FieldUID))
		if uid == "" {
			return 0, 0, fmt.Errorf("import file %s: missing UID", name)
		}
		res, err := putOrClassifyImportedCard(ctx, store, addressbookID, href, uid, raw, dryRun)
		if err != nil {
			return 0, 0, fmt.Errorf("put card %s: %w", name, err)
		}
		if res.Created {
			created++
		} else {
			updated++
		}
	}
	return created, updated, nil
}

func importFromConcatFile(ctx context.Context, store *db.Store, addressbookID int64, path string, dryRun bool) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("open import file: %w", err)
	}
	defer func() { _ = f.Close() }()
	dec := vcard.NewDecoder(f)
	var created, updated int
	for {
		card, err := dec.Decode()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, fmt.Errorf("decode import file: %w", err)
		}
		uid := strings.TrimSpace(card.PreferredValue(vcard.FieldUID))
		if uid == "" {
			return 0, 0, fmt.Errorf("decode import file: missing UID")
		}
		href := uid + ".vcf"
		var raw []byte
		if !dryRun {
			var buf bytes.Buffer
			if err := vcard.NewEncoder(&buf).Encode(card); err != nil {
				return 0, 0, fmt.Errorf("encode imported card %s: %w", uid, err)
			}
			raw = buf.Bytes()
		}
		res, err := putOrClassifyImportedCard(ctx, store, addressbookID, href, uid, raw, dryRun)
		if err != nil {
			return 0, 0, fmt.Errorf("put card %s: %w", uid, err)
		}
		if res.Created {
			created++
		} else {
			updated++
		}
	}
	return created, updated, nil
}

func putOrClassifyImportedCard(ctx context.Context, store *db.Store, addressbookID int64, href, uid string, raw []byte, dryRun bool) (db.PutCardResult, error) {
	if dryRun {
		_, err := store.GetCard(ctx, addressbookID, href)
		switch {
		case err == nil:
			return db.PutCardResult{Created: false}, nil
		case errors.Is(err, db.ErrNotFound):
			return db.PutCardResult{Created: true}, nil
		default:
			return db.PutCardResult{}, err
		}
	}
	return store.PutCard(ctx, db.PutCardInput{
		AddressbookID: addressbookID,
		Href:          href,
		UID:           uid,
		VCard:         raw,
	})
}

func decodeSingleCardBytes(raw []byte) (vcard.Card, error) {
	dec := vcard.NewDecoder(bytes.NewReader(raw))
	card, err := dec.Decode()
	if err != nil {
		return nil, err
	}
	return card, nil
}

func resolvePasswordInput(password string, passwordStdin bool, stdin io.Reader) (string, error) {
	hasPassword := password != ""
	if hasPassword == passwordStdin {
		return "", fmt.Errorf("specify exactly one of --password or --password-stdin")
	}
	if hasPassword {
		return password, nil
	}
	if stdin == nil {
		return "", fmt.Errorf("stdin is not available")
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	pw := strings.TrimRight(string(raw), "\r\n")
	if pw == "" {
		return "", fmt.Errorf("password from stdin is empty")
	}
	return pw, nil
}

func newCLIFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func defaultCLIOpt(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return strings.TrimSpace(v)
}

var usernameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,62}[a-z0-9])?$`)

func validateUsername(username string) error {
	if !usernameRE.MatchString(username) {
		return fmt.Errorf("invalid --username: use 1-64 chars [a-z0-9_-], start/end with [a-z0-9]")
	}
	switch username {
	case ".well-known", "health":
		return fmt.Errorf("invalid --username: %q is reserved", username)
	}
	return nil
}
