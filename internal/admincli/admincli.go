// Package admincli implements the operator-facing `admin` subcommands of the
// MailMoose binary. They open the store directly, so recovery does not depend
// on the HTTP server or a working application configuration (in particular,
// APP_ENCRYPTION_KEY is not required).
package admincli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dellarb/mailmoose/internal/auth"
	"github.com/dellarb/mailmoose/internal/store"
)

// dataDirEnv matches the server's DATA_DIR so the command and a running
// service operate on the same database.
const dataDirEnv = "DATA_DIR"

const defaultDataDir = "/data"

// Run executes one `mailmoose admin` command and returns a process exit code.
// stdin/stdout/stderr are injected so tests can drive it without a terminal.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "reset-password":
		return runResetPassword(args[1:], stdin, stdout, stderr)
	case "revoke-api-keys":
		return runRevokeAPIKeys(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown admin command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `usage: mailmoose admin <command> [options]

Commands:
  reset-password <email> [--password-file PATH]
        Replace a user's password and revoke all of their browser sessions.
  revoke-api-keys <email>
        Revoke every API key on the user's account.

DATA_DIR selects the database directory (default /data).`)
}

// openStore opens the application database using DATA_DIR.
func openStore() (*store.Store, error) {
	dir := strings.TrimSpace(os.Getenv(dataDirEnv))
	if dir == "" {
		dir = defaultDataDir
	}
	return store.Open(dir)
}

func runResetPassword(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// Flags are parsed by hand so `--password-file` may appear before or after
	// the positional email, matching the documented invocation. The standard
	// flag package stops at the first positional argument.
	var positional []string
	passwordFile := ""
	passwordFileSet := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--password-file" || a == "-password-file":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "reset-password: --password-file requires a value")
				return 2
			}
			i++
			passwordFile = args[i]
			passwordFileSet = true
		case strings.HasPrefix(a, "--password-file="):
			passwordFile = strings.TrimPrefix(a, "--password-file=")
			passwordFileSet = true
		case strings.HasPrefix(a, "-password-file="):
			passwordFile = strings.TrimPrefix(a, "-password-file=")
			passwordFileSet = true
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "usage: mailmoose admin reset-password <email> [--password-file PATH]")
		return 2
	}
	if passwordFileSet && strings.TrimSpace(passwordFile) == "" {
		fmt.Fprintln(stderr, "reset-password: --password-file requires a value")
		return 2
	}
	email := strings.TrimSpace(positional[0])

	password, err := readNewPassword(passwordFile, stdin, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "reset-password: %v\n", err)
		return 1
	}
	if err := auth.ValidatePassword(password); err != nil {
		// The error never contains the password itself.
		fmt.Fprintf(stderr, "reset-password: %v\n", err)
		return 1
	}

	st, err := openStore()
	if err != nil {
		fmt.Fprintf(stderr, "reset-password: cannot open database: %v\n", err)
		return 1
	}
	defer st.Close()
	ctx := context.Background()
	user, err := st.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(stderr, "reset-password: no account found for %s\n", email)
			return 1
		}
		fmt.Fprintf(stderr, "reset-password: %v\n", err)
		return 1
	}
	if err := st.AdminResetPassword(ctx, user.ID, password); err != nil {
		if errors.Is(err, store.ErrSystemAdmin) {
			// The configured deployment secret would overwrite this on the next
			// restart, so send the operator to the source of truth.
			fmt.Fprintln(stderr, "reset-password: the system administrator's login is managed by the deployment configuration")
			fmt.Fprintln(stderr, "update ADMIN_PASSWORD or ADMIN_PASSWORD_FILE and restart MailMoose")
			return 1
		}
		fmt.Fprintf(stderr, "reset-password: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Password updated successfully.")
	fmt.Fprintln(stdout, "All existing sessions have been revoked.")
	return 0
}

func runRevokeAPIKeys(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("revoke-api-keys", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: mailmoose admin revoke-api-keys <email>")
		return 2
	}
	email := strings.TrimSpace(fs.Arg(0))
	st, err := openStore()
	if err != nil {
		fmt.Fprintf(stderr, "revoke-api-keys: cannot open database: %v\n", err)
		return 1
	}
	defer st.Close()
	ctx := context.Background()
	user, err := st.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(stderr, "revoke-api-keys: no account found for %s\n", email)
			return 1
		}
		fmt.Fprintf(stderr, "revoke-api-keys: %v\n", err)
		return 1
	}
	n, err := st.RevokeAPIKeysForAccount(ctx, user.AccountID)
	if err != nil {
		fmt.Fprintf(stderr, "revoke-api-keys: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Revoked %d API key(s) for %s\n", n, email)
	return 0
}

// readNewPassword resolves the new password from a file or an interactive
// prompt. A non-interactive stdin is refused rather than read silently, so a
// pipeline can never supply a password by accident; automation must use
// --password-file.
func readNewPassword(path string, stdin io.Reader, stdout io.Writer) (string, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("cannot read password file: %w", err)
		}
		pw := strings.TrimSpace(string(b))
		if pw == "" {
			return "", fmt.Errorf("password file is empty")
		}
		return pw, nil
	}
	f, ok := stdin.(*os.File)
	if !ok || !isTerminalFD(int(f.Fd())) {
		return "", fmt.Errorf("refusing to read a password from a non-interactive stdin; use --password-file")
	}
	fmt.Fprint(stdout, "New password: ")
	first, err := readSecretLine(f)
	fmt.Fprintln(stdout)
	if err != nil {
		return "", err
	}
	fmt.Fprint(stdout, "Confirm new password: ")
	second, err := readSecretLine(f)
	fmt.Fprintln(stdout)
	if err != nil {
		return "", err
	}
	if first != second {
		return "", fmt.Errorf("passwords do not match")
	}
	if first == "" {
		return "", fmt.Errorf("password must not be empty")
	}
	return first, nil
}
