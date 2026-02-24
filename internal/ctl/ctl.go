package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"

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
	_, _ = fmt.Fprintf(w, "usage: %s user <add|list|delete|passwd>\n", defaultProg(prog))
}

func printAdminHelp(w io.Writer, prog string) {
	_, _ = fmt.Fprintf(w, "usage: %s user <add|list|delete|passwd>\n", defaultProg(prog))
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "commands:")
	_, _ = fmt.Fprintln(w, "  user add      create user")
	_, _ = fmt.Fprintln(w, "  user list     list users")
	_, _ = fmt.Fprintln(w, "  user delete   delete user")
	_, _ = fmt.Fprintln(w, "  user passwd   change user password")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "run 'contactctl user -h' for user subcommand help")
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
	case ".well-known", "healthz", "readyz":
		return fmt.Errorf("invalid --username: %q is reserved", username)
	}
	return nil
}
