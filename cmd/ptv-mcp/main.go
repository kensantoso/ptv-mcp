// Command ptv-mcp is an MCP server for Melbourne public transport.
//
// It answers schedule questions from Victoria's published GTFS feed, which
// needs no credential. A realtime key is optional and only adds live delays.
//
// Transport is stdio: the MCP client launches this as a subprocess, so there is
// no server to host, no port to open, and any key stays in the user's own
// config file.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	gtfs "github.com/kensantoso/ptv-gtfs-go"
	"github.com/kensantoso/ptv-gtfs-go/live"
	"github.com/kensantoso/ptv-gtfs-go/realtime"
	"github.com/kensantoso/ptv-gtfs-go/store"
	"github.com/kensantoso/ptv-mcp/internal/render"
	"github.com/kensantoso/ptv-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"

	// Embed the IANA database. LoadLocation otherwise needs tzdata on the
	// host, which a minimal container or a Windows box does not have, and
	// this binary ships prebuilt inside the extension bundle.
	_ "time/tzdata"
)

const version = "0.6.0"

func main() {
	var (
		rebuild  = flag.Bool("rebuild", false, "rebuild the index and exit")
		showPath = flag.Bool("index-path", false, "print the index location and exit")
		dir      = flag.String("index-dir", store.DefaultDir(), "where the index lives")
		tz       = flag.String("tz", "Australia/Melbourne", "network timezone")
		modes    = flag.String("modes", "", "comma-separated modes to load: train,tram,bus. Empty loads every mode (~1.5 GB)")
		setKey   = flag.String("set-key", "", "store a realtime API key and exit")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	wanted, unknown := render.ParseModeList(*modes)
	if len(unknown) > 0 {
		fatal("unknown mode(s) %s in -modes; use train, tram or bus", strings.Join(unknown, ", "))
	}
	mgr := &store.Manager{Dir: *dir, Modes: wanted}

	if *showPath {
		fmt.Println(mgr.DBPath())
		return
	}

	if *setKey != "" {
		if err := os.MkdirAll(filepath.Dir(KeyFile), 0o700); err != nil {
			fatal("set-key: %v", err)
		}
		// Owner-only: this is a credential, and the directory is a cache others
		// have no reason to read.
		if err := os.WriteFile(KeyFile, []byte(strings.TrimSpace(*setKey)+"\n"), 0o600); err != nil {
			fatal("set-key: %v", err)
		}
		fmt.Fprintf(os.Stderr, "realtime key stored at %s\n", KeyFile)
		return
	}

	loc, err := time.LoadLocation(*tz)
	if err != nil {
		fatal("unknown timezone %q: %v", *tz, err)
	}

	if *rebuild {
		// Progress goes to stderr: stdout is the MCP wire protocol and any
		// stray byte there corrupts the session.
		if err := mgr.Build(ctx, logProgress); err != nil {
			fatal("rebuild: %v", err)
		}
		st, _ := mgr.Status(ctx)
		fmt.Fprintf(os.Stderr, "index rebuilt: %.0f MB at %s\n",
			float64(st.Bytes)/(1<<20), mgr.DBPath())
		return
	}

	deps := &tools.Deps{Loc: loc, Store: mgr, OnProgress: logProgress}

	st, _ := mgr.Status(ctx)
	switch {
	case st.Exists:
		db, err := mgr.EnsureBuilt(ctx, logProgress)
		if err != nil {
			fatal("index: %v", err)
		}
		defer db.Close()
		// Held on Deps as well, so a rebuild reopens with the same figures
		// rather than silently reverting to the graph's.
		deps.Policy = gtfs.Policy{TransferTimes: statedTransfers(ctx, db, *dir)}
		deps.SetIndex(gtfs.Open(db, loc, gtfs.WithPolicy(deps.Policy)))
		if st.Stale {
			fmt.Fprintf(os.Stderr,
				"ptv-mcp: index built %s and may be out of date; run with -rebuild to refresh\n",
				st.BuiltAt.Format("2 Jan"))
		}
	default:
		// No index yet. Building one takes minutes and a client waiting on the
		// handshake will give up long before that, so serve immediately and let
		// the tools explain themselves until the index arrives.
		fmt.Fprintln(os.Stderr, "ptv-mcp: no database yet; building in the background, tools will report progress")
		deps.Rebuild(ctx)
	}

	// The realtime key is optional. Without it every schedule tool still works;
	// with it, live delays become available. Failing to start over a missing
	// optional credential would be the wrong trade.
	key := realtimeKey()
	if key != "" {
		rt, err := realtime.NewClient(key)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ptv-mcp: realtime disabled: %v\n", err)
		} else {
			deps.Live = live.New(rt, gtfs.ModeMetroTrain, gtfs.ModeMetroTram, gtfs.ModeMetroBus)
		}
	} else {
		fmt.Fprintf(os.Stderr,
			"ptv-mcp: no realtime key; serving timetable only. Set PTV_REALTIME_KEY, or run: ptv-mcp -set-key YOUR_KEY (stored at %s)\n",
			KeyFile)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "ptv",
		Version: version,
	}, nil)
	tools.Register(server, deps)

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		fatal("server: %v", err)
	}
}

// KeyFile is a fallback location for the realtime key.
//
// The environment variable is the primary route and is what a bundle manifest
// populates. A file exists alongside it because a host that cannot present the
// manifest's configuration form would otherwise leave no way to supply a key at
// all, and because a file is easier to rotate than a reinstall.
var KeyFile = filepath.Join(store.DefaultDir(), "realtime.key")

// realtimeKey resolves the key from the environment, then the key file.
func realtimeKey() string {
	key := os.Getenv("PTV_REALTIME_KEY")
	// An unset optional field in a bundle manifest can arrive as the literal
	// placeholder rather than an empty string. Treating that as a key would mean
	// every realtime call failing with a 401 instead of the server simply
	// running without realtime.
	if strings.Contains(key, "${") {
		key = ""
	}
	if key != "" {
		return key
	}
	b, err := os.ReadFile(KeyFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func logProgress(p store.Progress) {
	switch p.Stage {
	case "download":
		if p.Downloaded > 0 && p.Downloaded%(50<<20) < (1<<20) {
			fmt.Fprintf(os.Stderr, "  downloading feed: %.0f MB\n", float64(p.Downloaded)/(1<<20))
		} else if p.Downloaded == 0 {
			fmt.Fprintln(os.Stderr, "ptv-mcp: building index, this takes a few minutes on first run")
			fmt.Fprintln(os.Stderr, "  downloading feed (~280 MB)")
		}
	case "extract":
		fmt.Fprintln(os.Stderr, "  extracting")
	case "index":
		if p.Rows > 0 && p.Rows%1000000 == 0 {
			fmt.Fprintf(os.Stderr, "  indexing %s: %dM rows (%s)\n",
				p.Mode, p.Rows/1000000, p.Elapsed.Round(time.Second))
		}
	}
}

// TransferFile is where stated change times live: platform pairs the operator's
// cautious graph gets wrong for you.
//
// Optional, and beside the database because both are per-installation. A missing
// file is silence; a malformed one is fatal, because starting with an override
// the user believes is active but is not produces journeys they cannot explain.
func TransferFile(dir string) string { return filepath.Join(dir, "transfers.json") }

func statedTransfers(ctx context.Context, db *sql.DB, dir string) map[gtfs.StopPair]time.Duration {
	f, err := os.Open(TransferFile(dir))
	if err != nil {
		return nil
	}
	defer f.Close()
	stated, err := gtfs.LoadTransferTimes(ctx, db, f)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Fprintf(os.Stderr, "ptv-mcp: %d stated change times from %s\n", len(stated), TransferFile(dir))
	return stated
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ptv-mcp: "+format+"\n", args...)
	os.Exit(1)
}
