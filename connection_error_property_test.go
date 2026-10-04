package dalgo2postgres

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The property: for any connection string, in the keyword form or the URL form,
// with any separator the driver accepts or merely reads as part of a value, and
// any user name, password and option value (long, quoted, containing each other),
// the text of every error the constructors return holds no run of four bytes or
// more of a user name, a password, an option value or a setting name; whatever the
// driver's own error says, and whatever it wraps.
//
// The generator is deterministic: iteration i uses seed propertySeed+i, so a
// failure reproduces by running that iteration alone.
const (
	propertySeed       = 20261004
	propertyIterations = 1500
	propertyRealRuns   = 250 // strings sent through the real constructors and the real driver
)

// secretAlphabet is what a generated secret is made of besides its upper-case
// letters: word characters and every character a connection string treats
// specially, and two multi-byte runes.
var (
	secretAlphabet = []rune("abcdefghijklmnopqrstuvwxyz0123456789-_. \t'\\;,&=@/#?:%+*!$()[]{}<>|\"~^é日")
	// plainAlphabet is what a secret is made of when it must also be a possible
	// database name or host: such a secret can only be left out of an error because
	// it is a secret.
	plainAlphabet = []rune("abcdefghijklmnopqrstuvwxyz0123456789-")
	nameAlphabet  = []rune("abcdefghijklmnopqrstuvwxyz0123456789_")
	// tameAlphabet is what the secrets of many real strings are made of: nothing a
	// shape check would reject, so that only the structure of a string, not the
	// characters of a value, can keep a swallowed secret out of a named part.
	tameAlphabet = []rune("abcdefghijklmnopqrstuvwxyz0123456789-_.")
)

type generator struct{ r *rand.Rand }

func newGenerator(iteration int) *generator {
	return &generator{rand.New(rand.NewPCG(propertySeed+uint64(iteration), 7))}
}

func (g *generator) intn(n int) int          { return g.r.IntN(n) }
func (g *generator) chance(percent int) bool { return g.intn(100) < percent }
func (g *generator) pick(items ...string) string {
	return items[g.intn(len(items))]
}

// secret makes a value of between min and max bytes (it may overshoot by a few).
// It is built of chunks that each open with an upper-case letter and carry one to
// three characters of alphabet after it, so that every run of four bytes of it
// holds an upper-case letter or a byte that is not ASCII: such a run cannot be
// part of an error's fixed text, which is lower-case apart from a few words, or
// of a host, a port or a database name, which this generator makes lower-case.
func (g *generator) secret(min, max int, alphabet []rune) string {
	target := min + g.intn(max-min+1)
	var b strings.Builder
	for b.Len() < target {
		b.WriteByte(byte('A' + g.intn(26)))
		for n := 1 + g.intn(3); n > 0; n-- {
			b.WriteRune(alphabet[g.intn(len(alphabet))])
		}
	}
	return b.String()
}

// lowerName makes a lower-case word with the given alphabet: a host, a port's
// neighbour, a database name.
func (g *generator) lowerName(min, max int, alphabet []rune) string {
	n := min + g.intn(max-min+1)
	out := make([]rune, n)
	for i := range out {
		out[i] = alphabet[g.intn(len(alphabet))]
	}
	return string(out)
}

type setting struct{ key, value string }

// spec is a generated connection string and what must not appear in an error.
type spec struct {
	dsn            string
	form           string
	user, password string
	secrets        []string // user, password, option values and option names
	// The independent parts that the string was built with, when the generator
	// meant them to be nameable: nothing else of the string may be named.
	host, port, database string
	cross                string // which part was made to repeat a secret, if any
}

var keywordSeparators = []string{" ", "  ", "\t", "\n", "\r\n", "\v", "\f", " \t ", ";", ",", "&", "; ", ", ", " ; ", " & ", "&&", ";;"}

// spec generates one connection string. The user, password and option values
// come from the secret alphabet; the independent host, port and database from
// lower-case ones. In some strings the host or the database is made to repeat
// the user, the password or an option value (equal, contained in it, or
// containing it): it may then not be named.
func (g *generator) spec() spec {
	sp := spec{}
	alphabet := secretAlphabet
	if g.chance(35) {
		alphabet = tameAlphabet
	}
	user := g.secret(1, 120, alphabet)
	password := g.secret(1, 120, alphabet)
	if g.chance(15) {
		user = g.secret(1, 3, alphabet)
	}
	if g.chance(10) {
		password = "" // no password at all
	}
	appName := g.secret(4, 40, alphabet)
	sp.host = g.pick("127.0.0.1", "localhost", g.lowerName(3, 30, []rune("abcdefghijklmnopqrstuvwxyz0123456789-.")))
	sp.port = g.pick("1", "5432", fmt.Sprint(1+g.intn(65535)), "")
	if g.chance(40) { // a server that cannot exist: the real driver may be sent these
		sp.host, sp.port = "127.0.0.1", "1"
	}
	sp.database = g.lowerName(2, 40, nameAlphabet)
	if g.chance(10) {
		sp.database = g.lowerName(64, 90, nameAlphabet) // longer than a database name may be
	}

	if g.chance(20) { // make the host or the database repeat a secret that the driver keeps
		switch g.intn(6) {
		case 0:
			user = g.secret(4, 30, plainAlphabet)
			sp.database, sp.cross = user, "database equals user"
		case 1:
			password = g.secret(4, 30, plainAlphabet)
			sp.database, sp.cross = password, "database equals password"
		case 2:
			user = g.secret(4, 30, plainAlphabet)
			sp.database, sp.cross = "app_"+user+"_prod", "database contains user"
		case 3:
			password = g.secret(8, 40, plainAlphabet)
			sp.database, sp.cross = password[2:len(password)-1], "database inside password"
		case 4:
			password = g.secret(8, 40, plainAlphabet)
			sp.host, sp.cross = "db."+password+".example", "host contains password"
		case 5:
			appName = g.secret(6, 30, plainAlphabet)
			sp.host, sp.cross = appName, "host equals application_name"
		}
	}

	options := []setting{{"application_name", appName}}
	for n := g.intn(4); n > 0; n-- {
		switch g.intn(7) {
		case 0:
			options = append(options, setting{"options", "-c " + g.secret(4, 50, alphabet)})
		case 1:
			options = append(options, setting{"search_path", g.secret(4, 70, alphabet)})
		case 2:
			options = append(options, setting{"x_" + g.secret(3, 20, nameAlphabet), g.secret(4, 90, alphabet)})
		case 3:
			options = append(options, setting{"passfile", "/" + g.secret(4, 70, alphabet)})
		case 4:
			options = append(options, setting{"sslpassword", g.secret(4, 70, alphabet)})
		case 5:
			options = append(options, setting{"sslrootcert", "/" + g.secret(4, 70, alphabet)}) // makes the string unparsable
		case 6:
			options = append(options, setting{"sslmode", g.pick("disable", "prefer", "require")})
		}
	}
	for _, o := range options {
		if o.key == "sslmode" {
			continue
		}
		sp.secrets = append(sp.secrets, o.value)
		if strings.HasPrefix(o.key, "x_") {
			sp.secrets = append(sp.secrets, o.key) // a setting name of the string's own
		}
	}
	sp.user, sp.password = user, password
	sp.secrets = append(sp.secrets, user, password)

	render := func() {
		if g.chance(55) {
			sp.form = "keyword"
			sp.dsn = g.keywordDSN(&sp, user, password, options)
		} else {
			sp.form = "url"
			sp.dsn = g.urlDSN(&sp, user, password, options)
		}
	}
	render()
	if sp.cross == "host equals application_name" {
		// The driver keeps an option value only when it reads the pair as one: a
		// URL query that separates its pairs with a semicolon is dropped whole, and
		// a value it does not keep cannot be compared with the host. Such a string
		// gets an independent host instead.
		if cfg, err := pgx.ParseConfig(sp.dsn); err != nil || cfg.RuntimeParams["application_name"] != appName {
			sp.host, sp.cross = g.lowerName(3, 30, []rune("abcdefghijklmnopqrstuvwxyz0123456789-.")), ""
			render()
		}
	}
	return sp
}

// keywordValue writes a value of a keyword/value string: bare, with each special
// character escaped by a backslash, or in single quotes.
func (g *generator) keywordValue(value string) string {
	if value == "" {
		return "''"
	}
	simple := strings.Trim(value, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") == ""
	if simple && g.chance(85) {
		return value // the way most strings are written
	}
	if g.chance(50) {
		var b strings.Builder
		b.WriteByte('\'')
		for i := 0; i < len(value); i++ {
			if value[i] == '\'' || value[i] == '\\' {
				b.WriteByte('\\')
			}
			b.WriteByte(value[i])
		}
		b.WriteByte('\'')
		return b.String()
	}
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if strings.IndexByte(" \t\n\r\v\f'\\", value[i]) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(value[i])
	}
	return b.String()
}

func (g *generator) keywordDSN(sp *spec, user, password string, options []setting) string {
	pairs := []setting{{"host", sp.host}, {"user", user}, {g.pick("dbname", "database"), sp.database}}
	if sp.port != "" {
		pairs = append(pairs, setting{"port", sp.port})
	}
	if password != "" {
		pairs = append(pairs, setting{"password", password})
	}
	pairs = append(pairs, options...)
	g.r.Shuffle(len(pairs), func(i, j int) { pairs[i], pairs[j] = pairs[j], pairs[i] })
	separator := g.pick(keywordSeparators...)
	if g.chance(60) {
		separator = " " // the form that parses; the others are the hostile ones
	}
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.key + "=" + g.keywordValue(p.value)
	}
	dsn := strings.Join(out, separator)
	if g.chance(10) {
		dsn = " " + dsn + "\n"
	}
	return dsn
}

// urlPart writes a part of a URL: percent-encoded as it should be, or as written,
// which is how a password with a slash or a number sign mis-splits a URL.
func urlPart(value string, encode func(string) string, encoded bool) string {
	if encoded {
		return encode(value)
	}
	return value
}

func (g *generator) urlDSN(sp *spec, user, password string, options []setting) string {
	encoded := g.chance(70)
	var b strings.Builder
	b.WriteString(g.pick("postgres", "postgresql"))
	b.WriteString("://")
	if user != "" || password != "" {
		switch {
		case !encoded:
			b.WriteString(user)
			if password != "" {
				b.WriteString(":" + password)
			}
		case password == "":
			b.WriteString(url.User(user).String())
		default:
			b.WriteString(url.UserPassword(user, password).String())
		}
		b.WriteString("@")
	}
	b.WriteString(sp.host)
	if sp.port != "" {
		b.WriteString(":" + sp.port)
	}
	b.WriteString("/" + sp.database)
	querySeparator := g.pick("&", "&", "&", ";", ",")
	var query []string
	for _, o := range options {
		query = append(query, url.QueryEscape(o.key)+"="+urlPart(o.value, url.QueryEscape, encoded))
	}
	if len(query) > 0 {
		b.WriteString("?" + strings.Join(query, querySeparator))
	}
	dsn := b.String()
	switch {
	case g.chance(6):
		dsn = `"` + dsn + `"`
	case g.chance(5):
		dsn = " " + dsn
	case g.chance(5):
		dsn = strings.Replace(dsn, "postgres", g.pick("POSTGRES", "postgresql+psycopg2", "jdbc:postgresql"), 1)
	}
	return dsn
}

// windows returns every run of four bytes of s.
func windows(s string) []string {
	var out []string
	for i := 0; i+4 <= len(s); i++ {
		out = append(out, s[i:i+4])
	}
	return out
}

// allowedText is every piece of text an error may hold besides the named
// parts: each fixed sentence with and without a step, a SQLSTATE and named parts.
func allowedText() string {
	var parts []string
	codes := []string{"", "XX000"}
	for code := range serverSentences {
		codes = append(codes, code)
	}
	for kind := range kindSentences {
		for _, op := range []string{"", "sql.Open", "PingContext"} {
			for _, code := range codes {
				e := &ConnectionError{Kind: FailureKind(kind), SQLState: code, Host: "h", Port: "1", Database: "d", op: op}
				parts = append(parts, e.Error())
			}
		}
	}
	return strings.Join(parts, "\n")
}

// echoError is a driver error that repeats everything it was given, and wraps a
// cause, as pgx's own wrappers do.
type echoError struct {
	text string
	err  error
}

func (e *echoError) Error() string { return e.text + ": " + e.err.Error() }
func (e *echoError) Unwrap() error { return e.err }

// driverFailures are the failures of a driver, each carrying the whole string
// and every value in it, in every field of the error that could hold text.
func driverFailures(sp spec) map[string]error {
	echo := "failed to connect to `user=" + sp.user + " database=" + sp.database + "`: " +
		sp.dsn + " " + strings.Join(sp.secrets, " | ")
	wrap := func(err error) error { return &echoError{echo, err} }
	return map[string]error{
		"plain":            errors.New(echo),
		"parse":            wrap(pgconn.NewParseConfigError(sp.dsn, echo, errors.New(echo))),
		"server password":  wrap(&pgconn.PgError{Severity: "FATAL", Code: "28P01", Message: echo, Detail: echo, Hint: echo, Where: echo, SchemaName: echo, TableName: echo, ColumnName: echo, ConstraintName: echo}),
		"server database":  wrap(&pgconn.PgError{Code: "3D000", Message: echo}),
		"server odd code":  wrap(&pgconn.PgError{Code: "XX000", Message: echo}),
		"server fake code": wrap(&pgconn.PgError{Code: echo, Message: echo}),
		"dial":             wrap(&net.OpError{Op: "dial", Net: "tcp", Addr: fakeAddr(echo), Err: errors.New(echo)}),
		"dns":              wrap(&net.DNSError{Err: echo, Name: echo}),
		"deadline":         wrap(fmt.Errorf("%s: %w", echo, context.DeadlineExceeded)),
		"os deadline":      wrap(fmt.Errorf("%s: %w", echo, os.ErrDeadlineExceeded)),
		"timeout":          wrap(&net.OpError{Op: "read", Err: timeoutError{}}),
		"canceled":         wrap(fmt.Errorf("%s: %w", echo, context.Canceled)),
		"tls verify":       wrap(&tls.CertificateVerificationError{Err: x509.HostnameError{Certificate: &x509.Certificate{}, Host: echo}}),
		"tls header":       wrap(tls.RecordHeaderError{Msg: echo}),
		"tls authority":    wrap(x509.UnknownAuthorityError{}),
		"joined":           errors.Join(wrap(&net.DNSError{Err: echo}), wrap(&pgconn.PgError{Code: "28000", Message: echo})),
	}
}

// fakeAddr is a net.Addr whose text is whatever it is given.
type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

// propertyCheck holds what a run of the property counts, so that a generator
// that drifts into producing only strings that fail early is noticed.
type propertyStats struct {
	strings, keyword, url, parsed, refused, named, real int
}

var (
	hostField     = regexp.MustCompile(`^[A-Za-z0-9.:\[\]-]{1,253}$`)
	databaseField = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)
	portField     = regexp.MustCompile(`^[0-9]{1,5}$`)
)

// checkNoLeak asserts the property for one error.
func checkNoLeak(t *testing.T, iteration int, route string, sp spec, err error, allowed string, stats *propertyStats) {
	t.Helper()
	if err == nil {
		t.Errorf("iteration %d %s: want an error for %q, got nil", iteration, route, sp.dsn)
		return
	}
	connErr, ok := err.(*ConnectionError)
	if !ok {
		t.Errorf("iteration %d %s: error is %T, want *ConnectionError, for %q", iteration, route, err, sp.dsn)
		return
	}
	if inner := errors.Unwrap(err); inner != nil {
		t.Errorf("iteration %d %s: the original error is reachable: %T", iteration, route, inner)
	}
	text := err.Error()
	textWindows := map[string]bool{}
	for _, w := range windows(text) {
		textWindows[w] = true
	}
	for _, secret := range sp.secrets {
		for _, w := range windows(secret) {
			if textWindows[w] && !strings.Contains(allowed, w) {
				t.Errorf("iteration %d %s (%s): the text holds %q, a run of a secret %q\n\tdsn:  %q\n\ttext: %q", iteration, route, sp.form, w, secret, sp.dsn, text)
				return
			}
		}
		for _, field := range []string{connErr.Host, connErr.Port, connErr.Database} {
			if field != "" && field == secret {
				t.Errorf("iteration %d %s: the error names %q, which is a secret of the string %q", iteration, route, field, sp.dsn)
			}
		}
	}
	// The parts that may be named have their shapes, whatever the string held.
	if connErr.Host != "" && !hostField.MatchString(connErr.Host) ||
		connErr.Database != "" && !databaseField.MatchString(connErr.Database) ||
		connErr.Port != "" && !portField.MatchString(connErr.Port) {
		t.Errorf("iteration %d %s: a named part has the wrong shape: %+v", iteration, route, *connErr)
	}
	if sp.cross != "" && (connErr.Database != "" && strings.HasPrefix(sp.cross, "database") || connErr.Host != "" && strings.HasPrefix(sp.cross, "host")) {
		t.Errorf("iteration %d %s: %s, and the error still names it: %+v", iteration, route, sp.cross, *connErr)
	}
	if connErr.Host != "" && connErr.Host != sp.host || connErr.Database != "" && connErr.Database != sp.database ||
		connErr.Port != "" && connErr.Port != sp.port && connErr.Port != "5432" {
		t.Errorf("iteration %d %s: the error names %+v, which is not what the string was built with (host %q, database %q): %q", iteration, route, *connErr, sp.host, sp.database, sp.dsn)
	}
	if connErr.Host != "" || connErr.Database != "" {
		stats.named++
	}
}

// safeToDial reports whether sending dsn through the real driver can reach
// nothing but port 1 of this machine: the driver cannot parse it, or refuses it,
// or every endpoint is 127.0.0.1:1 or a name that cannot be resolved.
func safeToDial(dsn string) bool {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || inspectDSN(dsn).misread {
		return true
	}
	endpoints := []struct {
		host string
		port uint16
	}{{cfg.Host, cfg.Port}}
	for _, f := range cfg.Fallbacks {
		endpoints = append(endpoints, struct {
			host string
			port uint16
		}{f.Host, f.Port})
	}
	for _, e := range endpoints {
		unresolvable := strings.ContainsAny(e.host, ";=,&@ !%'\"#?/$()[]{}<>|~^*+\\\t\n\r\v\f")
		local := e.host == "127.0.0.1" && e.port == 1
		if !local && !unresolvable {
			return false
		}
	}
	return true
}

func TestConnectionErrorLeaksNothing_Property(t *testing.T) {
	allowed := allowedText()
	var stats propertyStats
	for i := 0; i < propertyIterations; i++ {
		g := newGenerator(i)
		sp := g.spec()
		stats.strings++
		if sp.form == "keyword" {
			stats.keyword++
		} else {
			stats.url++
		}
		if _, err := pgx.ParseConfig(sp.dsn); err == nil {
			stats.parsed++
		}
		if inspectDSN(sp.dsn).misread {
			stats.refused++
		}
		for name, cause := range driverFailures(sp) {
			// The driver fails to open.
			open := func(string, string) (*sql.DB, error) { return nil, cause }
			_, err := newDatabase(sp.dsn, dal.NewSchema(nil, nil), dalgo2sql.DbOptions{}, nil, open)
			if inspectDSN(sp.dsn).misread { // refused before the driver, whatever it would say
				checkNoLeak(t, i, "refused", sp, err, allowed, &stats)
				break
			}
			checkNoLeak(t, i, "open/"+name, sp, err, allowed, &stats)

			// The driver opens, and the ping fails.
			if (i+len(name))%4 == 0 {
				mockDB, mock, mockErr := sqlmock.New(sqlmock.MonitorPingsOption(true))
				if mockErr != nil {
					t.Fatal(mockErr)
				}
				mock.ExpectPing().WillReturnError(cause)
				_, err = newDatabase(sp.dsn, dal.NewSchema(nil, nil), dalgo2sql.DbOptions{}, nil,
					func(string, string) (*sql.DB, error) { return mockDB, nil })
				checkNoLeak(t, i, "ping/"+name, sp, err, allowed, &stats)
			}
		}
		// The real constructors with the real driver.
		if stats.real < propertyRealRuns && safeToDial(sp.dsn) {
			stats.real++
			var err error
			if i%2 == 0 {
				_, err = NewDatabase(sp.dsn)
			} else {
				_, err = NewDatabaseWithOptions(sp.dsn, dal.NewSchema(nil, nil), dalgo2sql.DbOptions{})
			}
			if err == nil {
				t.Fatalf("iteration %d: the real constructor opened %q", i, sp.dsn)
			}
			checkNoLeak(t, i, "real", sp, err, allowed, &stats)
		}
		if t.Failed() {
			t.Fatalf("stopping at iteration %d", i)
		}
	}

	// The generator must keep producing strings worth checking.
	t.Logf("strings %d (keyword %d, url %d), parsed %d, refused %d, errors that name a host or a database %d, real constructor runs %d",
		stats.strings, stats.keyword, stats.url, stats.parsed, stats.refused, stats.named, stats.real)
	for what, ok := range map[string]bool{
		"keyword strings":           stats.keyword >= propertyIterations/3,
		"url strings":               stats.url >= propertyIterations/3,
		"strings the driver parses": stats.parsed >= propertyIterations/3,
		"strings that are refused":  stats.refused >= 10,
		"errors that name a part":   stats.named >= propertyIterations,
		"real constructor runs":     stats.real >= propertyRealRuns/2,
	} {
		if !ok {
			t.Errorf("the generator drifted: too few %s (%+v)", what, stats)
		}
	}
}

func TestPropertyGenerator_ProducesWhatItClaims(t *testing.T) {
	// Spot checks on the generator itself, so that a change to it cannot quietly
	// empty the property of its meaning.
	long, quoted, crossed, separators := false, false, map[string]bool{}, map[string]bool{}
	for i := 0; i < propertyIterations; i++ {
		sp := newGenerator(i).spec()
		for _, s := range sp.secrets {
			long = long || len(s) > 63
			for _, w := range windows(s) {
				if !strings.ContainsAny(w, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") && !strings.ContainsFunc(w, func(r rune) bool { return r > 127 }) {
					t.Fatalf("iteration %d: the run %q of the secret %q holds no upper-case letter and no multi-byte byte", i, w, s)
				}
			}
		}
		quoted = quoted || strings.Contains(sp.dsn, "='")
		if sp.cross != "" {
			crossed[sp.cross] = true
		}
		for _, sep := range []string{";", ",", "&", "\t", "\n"} {
			if sp.form == "keyword" && (strings.Contains(sp.dsn, sep+"user=") || strings.Contains(sp.dsn, sep+"host=") || strings.Contains(sp.dsn, sep+"password=")) {
				separators[sep] = true
			}
		}
	}
	if !long || !quoted || len(crossed) != 6 || len(separators) < 5 {
		t.Errorf("long values %v, quoted values %v, cross modes %v, separators seen %v", long, quoted, crossed, separators)
	}
}
