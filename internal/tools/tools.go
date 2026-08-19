// Package tools exposes the GTFS index as MCP tools.
//
// Two conventions run through all of them, both aimed at an assistant rather
// than a program:
//
//   - Times are absolute ("14:32"), never relative ("in 3 minutes"). A model's
//     reply is read some time after it is generated, and a countdown is wrong
//     by then while a clock time is not.
//   - Stop lookup is its own tool. Models invent plausible numeric ids, so
//     find_stop exists to make them ask rather than guess.
package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	gtfs "github.com/kensantoso/ptv-gtfs-go"
	"github.com/kensantoso/ptv-gtfs-go/live"
	"github.com/kensantoso/ptv-gtfs-go/realtime"
	"github.com/kensantoso/ptv-gtfs-go/store"
	"github.com/kensantoso/ptv-mcp/internal/render"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Attribution is required whenever this data is surfaced, under the licence PTV
// publishes it beneath.
const Attribution = "Source: Licensed from Public Transport Victoria under a Creative Commons Attribution 4.0 International Licence."

// Deps are what the tools operate on.
//
// The index is held behind an atomic pointer because on a first run there is
// nothing to serve yet. Building it takes minutes, and a client that is waiting
// on the handshake will give up long before that, so the server starts
// immediately and the index is swapped in when it is ready.
type Deps struct {
	Live  *live.Cache // nil when no realtime key is configured
	Loc   *time.Location
	Store *store.Manager
	// Policy carries stated change times and any other judgements, so a rebuild
	// reopens the database with the same ones the process started with.
	Policy gtfs.Policy
	// OnProgress, if set, is called for every build progress report, so the
	// process can log to stderr while the tools report to the client.
	OnProgress func(store.Progress)

	index      atomic.Pointer[gtfs.Index]
	progress   atomic.Pointer[string]
	rebuilding atomic.Bool
}

// Rebuild loads a fresh database in the background and swaps it in when ready,
// reporting false if one is already under way.
//
// Background because it takes minutes: a tool call that blocked for that long
// would time out in the client, and the existing database keeps answering
// meanwhile. The new one is built under a temporary name and moved into place
// only on success, so a failure leaves the working copy untouched.
func (d *Deps) Rebuild(ctx context.Context) bool {
	if d.Store == nil || !d.rebuilding.CompareAndSwap(false, true) {
		return false
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		defer d.rebuilding.Store(false)
		err := d.Store.Build(ctx, func(p store.Progress) {
			if d.OnProgress != nil {
				d.OnProgress(p)
			}
			d.SetProgress(describeProgress(p))
		})
		if err != nil {
			d.SetProgress("the rebuild failed: " + err.Error())
			return
		}
		db, err := d.Store.EnsureBuilt(ctx, nil)
		if err != nil {
			d.SetProgress("the database could not be opened: " + err.Error())
			return
		}
		d.SetIndex(gtfs.Open(db, d.Loc, gtfs.WithPolicy(d.Policy)))
		d.SetProgress("")
	}()
	return true
}

// Rebuilding reports whether a build is under way.
func (d *Deps) Rebuilding() bool { return d.rebuilding.Load() }

// describeProgress turns a build report into something a client can read.
func describeProgress(p store.Progress) string {
	switch p.Stage {
	case "download":
		if p.Downloaded > 0 {
			return fmt.Sprintf("downloading the feed, %.0f MB so far", float64(p.Downloaded)/(1<<20))
		}
		return "downloading the feed (~250 MB)"
	case "extract":
		return "extracting the feed"
	case "index":
		if p.Rows > 0 {
			return fmt.Sprintf("loading %s: %d rows so far", p.Mode, p.Rows)
		}
		return "loading the feed into the database"
	}
	return "building the database"
}

// SetIndex publishes an index for the tools to use.
func (d *Deps) SetIndex(ix *gtfs.Index) { d.index.Store(ix) }

// SetProgress records what the build is doing, for tools to report while there
// is still no index.
func (d *Deps) SetProgress(msg string) { d.progress.Store(&msg) }

// errNotReady is returned by every tool until the index exists. It says what is
// happening and roughly how long it will take, because the alternative is a
// client concluding the server is broken.
func (d *Deps) idx() (*gtfs.Index, error) {
	if ix := d.index.Load(); ix != nil {
		return ix, nil
	}
	msg := "Building the timetable index. This happens once and takes a few minutes; the feed is about 280 MB."
	if p := d.progress.Load(); p != nil && *p != "" {
		msg += " Currently: " + *p + "."
	}
	return nil, errors.New(msg + " Ask again shortly.")
}

// Register adds every tool to the server.
func Register(s *mcp.Server, d *Deps) {
	registerFindStop(s, d)
	registerNextDepartures(s, d)
	registerPlanTrip(s, d)
	registerLastService(s, d)
	registerServiceAlerts(s, d)
	registerStopsNear(s, d)
	registerCompareDepartureStops(s, d)
	registerLegCalls(s, d)
	registerDatabaseStatus(s, d)
	registerRebuildDatabase(s, d)
}

// resolveStop turns whatever the caller gave into a stop id.
//
// Tools take an id or a name because insisting on ids costs a round trip: the
// model must call find_stop first and wait for the answer before it can ask the
// real question. Resolving here collapses three exchanges into one. The chosen
// stop is named back in the response, and near misses listed, so a wrong pick is
// visible and correctable rather than silent.
func (d *Deps) resolveStop(ctx context.Context, id, name string, modes ...gtfs.Mode) (string, string, []string, error) {
	if id != "" {
		return id, "", nil, nil
	}
	if strings.TrimSpace(name) == "" {
		return "", "", nil, errors.New("give a stop id or a name")
	}
	ix, err := d.idx()
	if err != nil {
		return "", "", nil, err
	}
	stops, err := ix.FindStops(ctx, name, modes...)
	if err != nil {
		return "", "", nil, err
	}
	if len(stops) == 0 {
		return "", "", nil, fmt.Errorf("no stop matching %q", name)
	}
	// An exact name wins over a shorter one that merely contains the text:
	// "Richmond" should not resolve to "Richmond Rd/Something" because that
	// happens to be shorter than "Richmond Railway Station".
	best := stops[0]
	for _, st := range stops {
		if strings.EqualFold(st.Name, name) {
			best = st
			break
		}
	}
	var others []string
	for _, st := range stops {
		if st.ID != best.ID {
			others = append(others, fmt.Sprintf("%s (%s)", st.Name, st.Mode))
		}
		if len(others) == 4 {
			break
		}
	}
	return best.ID, fmt.Sprintf("%s (%s)", best.Name, best.Mode), others, nil
}

// origin resolves either a coordinate pair or a stop id to a point.
//
// An assistant has no GPS. Coordinates arrive when the user states them or the
// host supplies them; a stop id is the fallback that always works, because
// find_stop can reach one from a name.
func (d *Deps) origin(ctx context.Context, lat, lon float64, stopID string) (float64, float64, error) {
	if stopID == "" {
		if lat == 0 && lon == 0 {
			return 0, 0, fmt.Errorf("give either lat and lon, or near_stop_id from find_stop")
		}
		return lat, lon, nil
	}
	ix, err := d.idx()
	if err != nil {
		return 0, 0, err
	}
	st, err := ix.Stop(ctx, stopID)
	if err != nil {
		return 0, 0, fmt.Errorf("unknown stop %q", stopID)
	}
	return st.Lat, st.Lon, nil
}

// nearStop and modeGroup are the wire shapes. The grouping and ranking behind
// them is [gtfs.Index.PlanFromNearby]; only the naming is local.
type nearStop struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Mode    string          `json:"mode"`
	Metres  int             `json:"metres_straight_line"`
	Journey *render.Journey `json:"best_journey,omitempty"`
}

type modeGroup struct {
	Mode  string     `json:"mode"`
	Stops []nearStop `json:"stops"`
}

// liveNote wraps the shared wording with this index's horizon.
func liveNote(d *Deps, l *gtfs.Live, when time.Time) string {
	horizon := gtfs.DefaultPolicy().RealtimeHorizon
	if ix, err := d.idx(); err == nil {
		horizon = ix.Policy().RealtimeHorizon
	}
	return render.LiveNote(l, when, horizon)
}

// alertsFor renders alerts affecting a set of routes and stops.
func alertsFor(ctx context.Context, d *Deps, l *gtfs.Live, now time.Time,
	routeIDs, stopIDs []string, limit int) []render.Alert {
	if l == nil {
		return nil
	}
	found := l.AlertsFor(now, routeIDs, stopIDs)
	if len(found) == 0 {
		return nil
	}
	ix, err := d.idx()
	if err != nil {
		return nil
	}
	routes, err := ix.RouteNames(ctx)
	if err != nil {
		routes = nil
	}
	return render.Alerts(found, routes, d.Loc, limit)
}

// snapshot fetches live data, tolerating failure. Realtime is an enhancement,
// and a gateway outage must not turn a working timetable answer into an error.
func snapshot(ctx context.Context, d *Deps) *gtfs.Live {
	if d.Live == nil {
		return nil
	}
	l, err := d.Live.Get(ctx)
	if err != nil {
		return nil
	}
	return l
}

// liveNote describes the freshness of a snapshot, or its absence.
// ---------- find_stop ----------

type findStopIn struct {
	Name string `json:"name" jsonschema:"Station or stop name to search for, e.g. 'Flinders Street' or 'Melbourne Central'"`
	Mode string `json:"mode,omitempty" jsonschema:"Optional filter: train, tram, or bus. Omit to search every mode."`
}

type findStopOut struct {
	Stops       []render.Stop `json:"stops"`
	Attribution string        `json:"attribution"`
}

func registerFindStop(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "find_stop",
		Description: "Find a station or stop by name and return its id. " +
			"Always call this before any tool that needs a stop id: ids are not guessable.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in findStopIn) (*mcp.CallToolResult, findStopOut, error) {
		ix, err := d.idx()
		if err != nil {
			return nil, findStopOut{}, err
		}
		stops, err := ix.FindStops(ctx, in.Name, render.ParseModes(in.Mode)...)
		if err != nil {
			return nil, findStopOut{}, err
		}
		if len(stops) > 12 {
			stops = stops[:12]
		}
		out := findStopOut{Attribution: Attribution}
		for _, st := range stops {
			out.Stops = append(out.Stops, render.Stop{
				ID: st.ID, Name: st.Name, Mode: st.Mode.String(), Lat: st.Lat, Lon: st.Lon,
			})
		}
		return nil, out, nil
	})
}

// ---------- next_departures ----------

type nextDeparturesIn struct {
	Stop   string `json:"stop,omitempty" jsonschema:"Stop as a name, e.g. 'Flinders Street'. Use this rather than calling find_stop first."`
	StopID string `json:"stop_id,omitempty" jsonschema:"Stop id, when you already have one. Takes precedence over stop."`
	When   string `json:"when,omitempty" jsonschema:"Optional local time as YYYY-MM-DD HH:MM. Defaults to now."`
	Mode   string `json:"mode,omitempty" jsonschema:"Optional filter: train, tram, or bus"`
	Limit  int    `json:"limit,omitempty" jsonschema:"How many departures to return. Default 10."`
}

type nextDeparturesOut struct {
	OtherMatches []string           `json:"other_matches,omitempty"`
	Stop         string             `json:"stop"`
	AsAt         string             `json:"as_at"`
	Departures   []render.Departure `json:"departures"`
	Live         string             `json:"live"`
	Alerts       []render.Alert     `json:"alerts,omitempty"`
	Note         string             `json:"note,omitempty"`
	Attribution  string             `json:"attribution"`
}

func registerNextDepartures(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "next_departures",
		Description: "Upcoming departures from a stop, with live delays where realtime covers them. " +
			"Times are absolute local clock times, so they stay correct when read later. " +
			"Check each departure's status: 'no live data' means not known, not on time.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nextDeparturesIn) (*mcp.CallToolResult, nextDeparturesOut, error) {
		ix, err := d.idx()
		if err != nil {
			return nil, nextDeparturesOut{}, err
		}
		when, err := render.ParseWhen(in.When, d.Loc)
		if err != nil {
			return nil, nextDeparturesOut{}, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 10
		}
		stopID, _, stopAlt, err := d.resolveStop(ctx, in.StopID, in.Stop, render.ParseModes(in.Mode)...)
		if err != nil {
			return nil, nextDeparturesOut{}, err
		}
		deps, err := ix.Departures(ctx, gtfs.DeparturesRequest{
			StopID: stopID, After: when, Modes: render.ParseModes(in.Mode), Limit: limit,
		})
		if err != nil {
			return nil, nextDeparturesOut{}, err
		}
		out := nextDeparturesOut{
			AsAt:        when.Format("Mon 2 Jan 15:04"),
			Attribution: Attribution,
		}
		name, err := stopName(ctx, d, stopID)
		if err != nil {
			return nil, nextDeparturesOut{}, fmt.Errorf("unknown stop %q", stopID)
		}
		out.Stop, out.OtherMatches = name, stopAlt

		l := snapshot(ctx, d)
		out.Departures = render.Departures(l, deps)
		out.Live = liveNote(d, l, when)
		if alerts := alertsAffecting(ctx, d, l, when, deps); len(alerts) > 0 {
			out.Alerts = alerts
		}
		if len(out.Departures) == 0 {
			out.Note = "No scheduled departures in the next two hours. Services may have finished for the night."
		}
		return nil, out, nil
	})
}

// ---------- plan_trip ----------

type planTripIn struct {
	From       string `json:"from,omitempty" jsonschema:"Origin as a name, e.g. 'Flinders Street' or 'Auburn'. Use this rather than calling find_stop first; the response says which stop it resolved to."`
	To         string `json:"to,omitempty" jsonschema:"Destination as a name"`
	FromStopID string `json:"from_stop_id,omitempty" jsonschema:"Origin stop id, when you already have one. Takes precedence over from."`
	ToStopID   string `json:"to_stop_id,omitempty" jsonschema:"Destination stop id. Takes precedence over to."`
	When       string `json:"when,omitempty" jsonschema:"Optional local departure time as YYYY-MM-DD HH:MM. Defaults to now."`
	Mode       string `json:"mode,omitempty" jsonschema:"Restrict to one mode: train, tram, or bus"`
	Prefer     string `json:"prefer,omitempty" jsonschema:"What to optimise: 'fastest' (default, soonest arrival), 'shortest' (least time travelling, even if it leaves later), 'fewest_transfers', or 'leave_latest'"`
	// A pointer so 0 is distinguishable from absent: an int cannot tell
	// "direct only" from "unspecified", and defaulting the wrong way makes the
	// documented 0 unreachable.
	MaxChanges *int   `json:"max_changes,omitempty" jsonschema:"0 for direct services only, 1 to allow one change. Default 1."`
	Via        string `json:"via,omitempty" jsonschema:"Optional stop id from find_stop. Returns only journeys that change at this station, e.g. to change at Richmond or to double back from Flinders Street."`
	MaxWait    int    `json:"max_wait_minutes,omitempty" jsonschema:"How long the traveller will wait for a departure, in minutes. Pair this with prefer='shortest': unbounded, the least-travelling journey may leave in ninety minutes to save three. Set it whenever someone says how long they are willing to wait."`
}

type planTripOut struct {
	From string `json:"from"`
	To   string `json:"to"`
	// ResolvedFrom and ResolvedTo name the stop a name was matched to, so a
	// wrong guess is visible rather than silently planned from the wrong place.
	ResolvedFrom string           `json:"resolved_from,omitempty"`
	ResolvedTo   string           `json:"resolved_to,omitempty"`
	OtherMatches []string         `json:"other_matches,omitempty"`
	AsAt         string           `json:"as_at"`
	Journeys     []render.Journey `json:"journeys"`
	Live         string           `json:"live"`
	Alerts       []render.Alert   `json:"alerts,omitempty"`
	Note         string           `json:"note,omitempty"`
	Attribution  string           `json:"attribution"`
}

func registerPlanTrip(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "plan_trip",
		Description: "Plan a journey between two stops, with or without a change. " +
			"Returns options ranked by arrival time, fewest changes, or latest departure, " +
			"with live delays and a warning when a connection no longer works. " +
			"Direct services and journeys with a change are searched together and ranked as one list, " +
			"so a change that beats every direct service is returned. " +
			"Each journey reports how long until it departs and how many City Loop stations it sits through, " +
			"so a shorter trip that leaves much later can be judged against one leaving now. " +
			"Use this for 'fastest way to X', 'shortest trip even if I wait', 'how do I avoid the loop', " +
			"'change at Richmond', and 'is my route still viable'.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in planTripIn) (*mcp.CallToolResult, planTripOut, error) {
		ix, err := d.idx()
		if err != nil {
			return nil, planTripOut{}, err
		}
		fromID, fromName2, fromAlt, err := d.resolveStop(ctx, in.FromStopID, in.From, render.ParseModes(in.Mode)...)
		if err != nil {
			return nil, planTripOut{}, err
		}
		toID, toName2, toAlt, err := d.resolveStop(ctx, in.ToStopID, in.To, render.ParseModes(in.Mode)...)
		if err != nil {
			return nil, planTripOut{}, err
		}
		when, err := render.ParseWhen(in.When, d.Loc)
		if err != nil {
			return nil, planTripOut{}, err
		}
		// Validate both ids before planning. An unknown id otherwise produces an
		// empty journey list, which reads as "no service" rather than "wrong id"
		// and sends a model looking for a timetable problem that does not exist.
		fromName, err := ix.StopName(ctx, fromID)
		if err != nil {
			return nil, planTripOut{}, fmt.Errorf("unknown origin %q", fromID)
		}
		toName, err := ix.StopName(ctx, toID)
		if err != nil {
			return nil, planTripOut{}, fmt.Errorf("unknown destination %q", toID)
		}

		maxChanges := 1
		if in.MaxChanges != nil {
			maxChanges = *in.MaxChanges
		}
		js, err := ix.Plan(ctx, gtfs.PlanRequest{
			FromStopID: fromID, ToStopID: toID, After: when,
			Modes: render.ParseModes(in.Mode), MaxTransfers: maxChanges,
			Rank: render.ParseRank(in.Prefer), Limit: 5, Via: in.Via,
			MaxWait: time.Duration(in.MaxWait) * time.Minute,
		})
		if err != nil {
			return nil, planTripOut{}, err
		}
		out := planTripOut{AsAt: when.Format("Mon 2 Jan 15:04"), Attribution: Attribution}
		out.From, out.To = fromName, toName
		out.ResolvedFrom, out.ResolvedTo = fromName2, toName2
		if len(fromAlt) > 0 || len(toAlt) > 0 {
			out.OtherMatches = append(append([]string{}, fromAlt...), toAlt...)
		}

		snap := snapshot(ctx, d)
		out.Live = liveNote(d, snap, when)
		out.Journeys = (&render.Deps{Index: ix, Loc: d.Loc}).Journeys(ctx, js, snap, when)
		if a := journeyAlerts(ctx, d, snap, when, js); len(a) > 0 {
			out.Alerts = a
		}
		if len(out.Journeys) == 0 {
			out.Note = "No journey found in the search window. Try a different time, or allow a change with max_changes=1."
			if in.Via != "" {
				out.Note = "No journey found changing at that station in the search window. Try omitting via, or a different time."
			}
		}
		return nil, out, nil
	})
}

// ---------- last_service ----------

type lastServiceIn struct {
	Stop   string `json:"stop,omitempty" jsonschema:"Stop as a name, e.g. 'Flinders Street'. Use this rather than calling find_stop first."`
	StopID string `json:"stop_id,omitempty" jsonschema:"Stop id, when you already have one. Takes precedence over stop."`
	Date   string `json:"date,omitempty" jsonschema:"Optional date as YYYY-MM-DD. Defaults to today."`
	Mode   string `json:"mode,omitempty" jsonschema:"Optional filter: train, tram, or bus"`
}

type lastServiceOut struct {
	OtherMatches []string           `json:"other_matches,omitempty"`
	Stop         string             `json:"stop"`
	Date         string             `json:"date"`
	Remaining    []render.Departure `json:"remaining_tonight"`
	Count        int                `json:"count"`
	Live         string             `json:"live"`
	Note         string             `json:"note,omitempty"`
	Attribution  string             `json:"attribution"`
}

func registerLastService(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "last_service",
		Description: "Services still to depart tonight from a stop, including after-midnight ones. " +
			"Answers 'when is the last train' and 'how many are left'.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in lastServiceIn) (*mcp.CallToolResult, lastServiceOut, error) {
		ix, err := d.idx()
		if err != nil {
			return nil, lastServiceOut{}, err
		}
		stopID, _, stopAlt, err := d.resolveStop(ctx, in.StopID, in.Stop, render.ParseModes(in.Mode)...)
		if err != nil {
			return nil, lastServiceOut{}, err
		}
		when := time.Now().In(d.Loc)
		if in.Date != "" {
			t, err := time.ParseInLocation("2006-01-02", in.Date, d.Loc)
			if err != nil {
				return nil, lastServiceOut{}, fmt.Errorf("date must be YYYY-MM-DD: %w", err)
			}
			when = t
		}
		// Long window: the last service of the night may be hours away, and
		// after-midnight trips belong to today's timetable.
		deps, err := ix.Departures(ctx, gtfs.DeparturesRequest{
			StopID: stopID, After: when, Within: 8 * time.Hour,
			Modes: render.ParseModes(in.Mode), Limit: 200,
		})
		if err != nil {
			return nil, lastServiceOut{}, err
		}
		out := lastServiceOut{
			Date: when.Format("Mon 2 Jan"), Count: len(deps), Attribution: Attribution,
		}
		out.Stop, _ = stopName(ctx, d, stopID)
		out.OtherMatches = stopAlt
		l := snapshot(ctx, d)
		out.Remaining = render.Departures(l, deps)
		out.Count = len(out.Remaining)
		out.Live = liveNote(d, l, when)
		if len(deps) == 0 {
			out.Note = "Nothing further scheduled tonight."
		}
		return nil, out, nil
	})
}

// ---------- helpers ----------

func stopName(ctx context.Context, d *Deps, stopID string) (string, error) {
	ix, err := d.idx()
	if err != nil {
		return "", err
	}
	return ix.StopName(ctx, stopID)
}

// ---------- service_alerts ----------

type serviceAlertsIn struct {
	Mode   string `json:"mode,omitempty" jsonschema:"Optional filter: train, tram, or bus. Omit for every mode."`
	StopID string `json:"stop_id,omitempty" jsonschema:"Optional stop id from find_stop, to limit alerts to services calling there"`
	Line   string `json:"line,omitempty" jsonschema:"Optional line name to match, e.g. 'Belgrave' or 'Frankston'"`
}

type lineStatusOut struct {
	Line string `json:"line"`
	// Tracked is how many services realtime is currently watching on this line.
	// A small number does not mean a small service, only a quiet horizon.
	Tracked      int    `json:"tracked"`
	WorstMinutes int    `json:"worst_delay_minutes"`
	TypicalDelay int    `json:"typical_delay_minutes"`
	Summary      string `json:"summary"`
}

type serviceAlertsOut struct {
	AsAt        string          `json:"as_at"`
	Alerts      []render.Alert  `json:"alerts"`
	Lines       []lineStatusOut `json:"line_status,omitempty"`
	Live        string          `json:"live"`
	Note        string          `json:"note,omitempty"`
	Attribution string          `json:"attribution"`
}

func registerServiceAlerts(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "service_alerts",
		Description: "Current disruptions and how late services are running, network-wide or for one line. " +
			"Use this for 'are there delays', 'is anything wrong on my line', and to decide " +
			"whether to take an alternate route or mode.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in serviceAlertsIn) (*mcp.CallToolResult, serviceAlertsOut, error) {
		ix, err := d.idx()
		if err != nil {
			return nil, serviceAlertsOut{}, err
		}
		now := time.Now().In(d.Loc)
		out := serviceAlertsOut{AsAt: now.Format("Mon 2 Jan 15:04"), Attribution: Attribution}

		l := snapshot(ctx, d)
		out.Live = liveNote(d, l, now)
		if l == nil {
			out.Note = "Disruption information needs a realtime key. Only the timetable is available."
			return nil, out, nil
		}

		modes := render.ParseModes(in.Mode)
		routes, err := routeNames(ctx, d, modes)
		if err != nil {
			return nil, serviceAlertsOut{}, err
		}

		var matched []realtime.Alert
		for _, a := range l.Alerts(now) {
			lines := render.LineNames(routes, a.Routes)
			if in.Line != "" && !matchesLine(in.Line, lines, a) {
				continue
			}
			if len(modes) > 0 && len(a.Routes) > 0 && len(lines) == 0 {
				continue // belongs to a mode the caller did not ask about
			}
			matched = append(matched, a)
		}
		out.Alerts = render.Alerts(matched, routes, d.Loc, 0)

		// Delay picture, if a stop was given to measure it at. Alerts say what
		// is wrong; this says how much it is actually costing right now.
		if in.StopID != "" {
			deps, err := ix.Departures(ctx, gtfs.DeparturesRequest{
				StopID: in.StopID, After: now, Within: time.Hour, Modes: modes, Limit: 300,
			})
			if err != nil {
				return nil, serviceAlertsOut{}, err
			}
			out.Lines = summariseLines(l.Disambiguate(l.Departures(deps)), in.Line)
		}

		if len(out.Alerts) == 0 && len(out.Lines) == 0 {
			out.Note = "No current disruptions match. Services are running to timetable as far as realtime can see."
		}
		return nil, out, nil
	})
}

// summariseLines turns per-departure delays into a per-line verdict, which is
// the shape of the question people actually ask.
func summariseLines(deps []gtfs.LiveDeparture, only string) []lineStatusOut {
	type acc struct {
		delays []time.Duration
		worst  time.Duration
	}
	byLine := map[string]*acc{}
	var order []string
	for _, x := range deps {
		if !x.Status.Known() || x.RouteName == "" {
			continue
		}
		if only != "" && !strings.Contains(strings.ToLower(x.RouteName), strings.ToLower(only)) {
			continue
		}
		a, ok := byLine[x.RouteName]
		if !ok {
			a = &acc{}
			byLine[x.RouteName] = a
			order = append(order, x.RouteName)
		}
		a.delays = append(a.delays, x.Delay)
		if x.Delay > a.worst {
			a.worst = x.Delay
		}
	}
	sort.Strings(order)

	out := make([]lineStatusOut, 0, len(order))
	for _, line := range order {
		a := byLine[line]
		// Median, not mean: one 17-minute outlier should not make a line that is
		// otherwise running fine look broken.
		sorted := append([]time.Duration(nil), a.delays...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		typical := sorted[len(sorted)/2]

		st := lineStatusOut{
			Line: line, Tracked: len(a.delays),
			WorstMinutes: int(a.worst.Round(time.Minute).Minutes()),
			TypicalDelay: int(typical.Round(time.Minute).Minutes()),
		}
		switch {
		case typical >= 5*time.Minute:
			st.Summary = "running late across the board"
		case a.worst >= 10*time.Minute:
			st.Summary = "mostly to time, with at least one badly delayed service"
		case typical >= 2*time.Minute:
			st.Summary = "minor delays"
		default:
			st.Summary = "running to time"
		}
		out = append(out, st)
	}
	return out
}

// routeNames maps route ids to line names so alerts can be reported in the
// words a passenger uses. The realtime feeds identify routes only by id.
func routeNames(ctx context.Context, d *Deps, modes []gtfs.Mode) (map[string]string, error) {
	ix, err := d.idx()
	if err != nil {
		return nil, err
	}
	return ix.RouteNames(ctx, modes...)
}

func matchesLine(want string, lines []string, a realtime.Alert) bool {
	want = strings.ToLower(want)
	// Where the alert names routes, those are the answer. Searching the prose as
	// well would match any line merely mentioned in passing, and disruption text
	// mentions neighbouring lines constantly.
	if len(lines) > 0 {
		for _, l := range lines {
			if strings.Contains(strings.ToLower(l), want) {
				return true
			}
		}
		return false
	}
	// Only an alert naming no route at all falls back to its text.
	return strings.Contains(strings.ToLower(a.Header+" "+a.Description), want)
}

// planned reports whether an alert is scheduled work rather than something
// happening now. Both matter, but a passenger deciding how to travel in the
// next ten minutes needs the incident first.
// incidentsFirst orders live incidents ahead of planned works, keeping each
// group in feed order.
// nearbyAlertLimit caps how many alerts ride along on a departures or journey
// answer. The dedicated service_alerts tool returns everything; these are a
// heads-up, and a wall of planned-works notices buries the timetable.
const nearbyAlertLimit = 3

// alertsAffecting returns alerts touching any of the services departing here.
func alertsAffecting(ctx context.Context, d *Deps, l *gtfs.Live, now time.Time, deps []gtfs.Departure) []render.Alert {
	ix, err := d.idx()
	if err != nil || l == nil || len(deps) == 0 {
		return nil
	}
	routeIDs := make([]string, 0, len(deps))
	seen := map[string]bool{}
	for _, x := range deps {
		if !seen[x.RouteID] {
			seen[x.RouteID] = true
			routeIDs = append(routeIDs, x.RouteID)
		}
	}
	stopIDs, err2 := ix.StopIDsForStation(ctx, deps[0].StopID)
	if err2 != nil {
		stopIDs = nil
	}
	found := l.AlertsFor(now, routeIDs, stopIDs)
	if len(found) == 0 {
		return nil
	}
	routes, err := ix.RouteNames(ctx)
	if err != nil {
		routes = nil
	}
	return render.Alerts(found, routes, d.Loc, nearbyAlertLimit)
}

func journeyAlerts(ctx context.Context, d *Deps, l *gtfs.Live, now time.Time, js []gtfs.Journey) []render.Alert {
	ix, err := d.idx()
	if err != nil || l == nil || len(js) == 0 {
		return nil
	}
	var routeIDs, stopIDs []string
	seen := map[string]bool{}
	for _, j := range js {
		for _, leg := range j.Legs {
			for _, id := range []string{leg.FromStop, leg.ToStop} {
				if !seen[id] {
					seen[id] = true
					stopIDs = append(stopIDs, id)
					if ids, err := ix.StopIDsForStation(ctx, id); err == nil {
						stopIDs = append(stopIDs, ids...)
					}
				}
			}
		}
	}
	for _, j := range js {
		for _, leg := range j.Legs {
			if r, err := ix.RouteIDForTrip(ctx, leg.TripID); err == nil && !seen[r] {
				seen[r] = true
				routeIDs = append(routeIDs, r)
			}
		}
	}
	found := l.AlertsFor(now, routeIDs, stopIDs)
	if len(found) == 0 {
		return nil
	}
	routes, err := ix.RouteNames(ctx)
	if err != nil {
		routes = nil
	}
	return render.Alerts(found, routes, d.Loc, nearbyAlertLimit)
}

type stopsNearIn struct {
	Lat        float64 `json:"lat,omitempty" jsonschema:"Latitude. Give this with lon, or use near_stop_id instead."`
	Lon        float64 `json:"lon,omitempty" jsonschema:"Longitude"`
	Near       string  `json:"near,omitempty" jsonschema:"A stop or station name to look around, when lat/lon is unknown"`
	NearStopID string  `json:"near_stop_id,omitempty" jsonschema:"Stop id to look around. Takes precedence over near."`
	Radius     int     `json:"radius_metres,omitempty" jsonschema:"Search radius in metres. Default 1200."`
	PerMode    int     `json:"per_mode,omitempty" jsonschema:"How many stops to return per mode. Default 3."`
	Mode       string  `json:"mode,omitempty" jsonschema:"Restrict to one mode: train, tram, or bus"`
}

type stopsNearOut struct {
	Modes       []modeGroup `json:"modes"`
	Note        string      `json:"note"`
	Attribution string      `json:"attribution"`
}

func registerStopsNear(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "stops_near",
		Description: "Stops near a point, grouped by mode so one mode cannot crowd out the others. " +
			"Use for 'what is around me' and to find a starting point when the user gives a location rather than a station. " +
			"Takes lat/lon, or near_stop_id if you only have a stop.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in stopsNearIn) (*mcp.CallToolResult, stopsNearOut, error) {
		nearID := in.NearStopID
		if nearID == "" && in.Near != "" {
			id, _, _, err := d.resolveStop(ctx, "", in.Near)
			if err != nil {
				return nil, stopsNearOut{}, err
			}
			nearID = id
		}
		lat, lon, err := d.origin(ctx, in.Lat, in.Lon, nearID)
		if err != nil {
			return nil, stopsNearOut{}, err
		}
		radius, perMode := float64(in.Radius), in.PerMode
		if radius <= 0 {
			radius = 1200
		}
		if perMode <= 0 {
			perMode = 3
		}
		ix, err := d.idx()
		if err != nil {
			return nil, stopsNearOut{}, err
		}
		groups, err := ix.StopsNearByMode(ctx, gtfs.NearbyRequest{
			Lat: lat, Lon: lon, RadiusMetres: radius, PerMode: perMode,
			Modes: render.ParseModes(in.Mode),
		})
		if err != nil {
			return nil, stopsNearOut{}, err
		}
		out := make([]modeGroup, 0, len(groups))
		for _, g := range groups {
			mg := modeGroup{Mode: g.Mode}
			for _, st := range g.Stops {
				mg.Stops = append(mg.Stops, nearStop{
					ID: st.ID, Name: st.Name, Mode: st.Mode.String(), Metres: int(st.Metres),
				})
			}
			out = append(out, mg)
		}
		return nil, stopsNearOut{
			Modes:       out,
			Note:        "Distance is straight-line, not walking distance. Real walking is longer.",
			Attribution: Attribution,
		}, nil
	})
}

type compareIn struct {
	To         string  `json:"to,omitempty" jsonschema:"Destination as a name"`
	ToStopID   string  `json:"to_stop_id,omitempty" jsonschema:"Destination stop id. Takes precedence over to."`
	Lat        float64 `json:"lat,omitempty" jsonschema:"Latitude of where the traveller is starting from"`
	Lon        float64 `json:"lon,omitempty" jsonschema:"Longitude"`
	Near       string  `json:"near,omitempty" jsonschema:"Where the traveller is, as a stop or station name, when lat/lon is unknown"`
	NearStopID string  `json:"near_stop_id,omitempty" jsonschema:"Stop id to start near. Takes precedence over near."`
	When       string  `json:"when,omitempty" jsonschema:"Optional local departure time as YYYY-MM-DD HH:MM. Defaults to now."`
	Radius     int     `json:"radius_metres,omitempty" jsonschema:"How far to look for departure stops, in metres. Default 1200."`
	PerMode    int     `json:"per_mode,omitempty" jsonschema:"Stops to compare per mode. Default 1; raise to compare neighbouring stops of the same mode."`
}

type compareOut struct {
	To           string      `json:"to"`
	OtherMatches []string    `json:"other_matches,omitempty"`
	AsAt         string      `json:"as_at"`
	Modes        []modeGroup `json:"modes"`
	Live         string      `json:"live,omitempty"`
	Note         string      `json:"note"`
	Attribution  string      `json:"attribution"`
}

func registerCompareDepartureStops(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "compare_departure_stops",
		Description: "Given a starting location and a destination, work out which nearby stop gets there soonest, and by which mode. " +
			"This is the 'fastest way from here' question. It answers the one that plan_trip cannot: " +
			"which of two nearby stations to walk to, when one is an express stop and the other is not. " +
			"Raise per_mode to compare neighbouring stops of the same mode.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in compareIn) (*mcp.CallToolResult, compareOut, error) {
		ix, err := d.idx()
		if err != nil {
			return nil, compareOut{}, err
		}
		nearID := in.NearStopID
		if nearID == "" && in.Near != "" {
			id, _, _, err := d.resolveStop(ctx, "", in.Near)
			if err != nil {
				return nil, compareOut{}, err
			}
			nearID = id
		}
		lat, lon, err := d.origin(ctx, in.Lat, in.Lon, nearID)
		if err != nil {
			return nil, compareOut{}, err
		}
		when, err := render.ParseWhen(in.When, d.Loc)
		if err != nil {
			return nil, compareOut{}, err
		}
		toID, _, toAlt, err := d.resolveStop(ctx, in.ToStopID, in.To)
		if err != nil {
			return nil, compareOut{}, err
		}
		toName, err := ix.StopName(ctx, toID)
		if err != nil {
			return nil, compareOut{}, fmt.Errorf("unknown destination %q", toID)
		}
		radius, perMode := float64(in.Radius), in.PerMode
		if radius <= 0 {
			radius = 1200
		}
		if perMode <= 0 {
			perMode = 1
		}
		snap := snapshot(ctx, d)
		rd := &render.Deps{Index: ix, Loc: d.Loc}
		groups, err := ix.PlanFromNearby(ctx,
			gtfs.NearbyRequest{Lat: lat, Lon: lon, RadiusMetres: radius, PerMode: perMode},
			toID,
			gtfs.PlanRequest{After: when, MaxTransfers: 1, Rank: gtfs.RankFastest, Limit: 1},
		)
		if err != nil {
			return nil, compareOut{}, err
		}
		out := make([]modeGroup, 0, len(groups))
		for _, g := range groups {
			mg := modeGroup{Mode: g.Mode}
			for _, o := range g.Origins {
				jo := rd.Journey(ctx, *o.Journey, snap, when)
				mg.Stops = append(mg.Stops, nearStop{
					ID: o.Stop.ID, Name: o.Stop.Name, Mode: o.Stop.Mode.String(),
					Metres: int(o.Stop.Metres), Journey: &jo,
				})
			}
			out = append(out, mg)
		}

		return nil, compareOut{
			To: toName, AsAt: when.Format("Mon 2 Jan 15:04"), Modes: out, OtherMatches: toAlt,
			Live:        liveNote(d, snap, when),
			Note:        "Distance is straight-line. Walking time to the stop is not included in the journey times.",
			Attribution: Attribution,
		}, nil
	})
}

type legCallsIn struct {
	TripID  string `json:"trip_id" jsonschema:"trip_id from a leg returned by plan_trip"`
	FromSeq int    `json:"from_seq" jsonschema:"from_seq from the same leg"`
	ToSeq   int    `json:"to_seq" jsonschema:"to_seq from the same leg"`
}

type callOut struct {
	Name     string `json:"name"`
	Platform string `json:"platform,omitempty"`
	Time     string `json:"time,omitempty"`
	Skipped  bool   `json:"skipped,omitempty"`
}

type legCallsOut struct {
	Calls       []callOut `json:"calls"`
	Attribution string    `json:"attribution"`
}

func registerLegCalls(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "leg_calls",
		Description: "Every station a leg of a journey calls at, and the ones it runs through without stopping. " +
			"Use after plan_trip when someone asks where a service stops, whether it is an express, " +
			"or whether it stops somewhere in particular. Pass trip_id, from_seq and to_seq from the leg.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in legCallsIn) (*mcp.CallToolResult, legCallsOut, error) {
		ix, err := d.idx()
		if err != nil {
			return nil, legCallsOut{}, err
		}
		calls, err := ix.LegCalls(ctx, in.TripID, in.FromSeq, in.ToSeq)
		if err != nil {
			return nil, legCallsOut{}, err
		}
		out := legCallsOut{Calls: make([]callOut, 0, len(calls)), Attribution: Attribution}
		for _, c := range calls {
			co := callOut{Name: c.Name, Platform: c.Platform, Skipped: c.Skipped}
			if !c.Skipped {
				co.Time = c.Time.Format("15:04")
			}
			out.Calls = append(out.Calls, co)
		}
		return nil, out, nil
	})
}

type statusOut struct {
	Present     bool   `json:"present"`
	BuiltAt     string `json:"built_at,omitempty"`
	AgeDays     int    `json:"age_days,omitempty"`
	Stale       bool   `json:"stale"`
	SizeMB      int    `json:"size_mb,omitempty"`
	TimetableTo string `json:"timetable_valid_to,omitempty"`
	Expired     bool   `json:"timetable_expired,omitempty"`
	Realtime    bool   `json:"realtime_configured"`
	Building    bool   `json:"rebuild_in_progress,omitempty"`
	Progress    string `json:"progress,omitempty"`
	Note        string `json:"note"`
}

func registerDatabaseStatus(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "database_status",
		Description: "How old the local timetable database is, and whether it has gone stale. " +
			"Worth checking when an answer looks wrong, or before relying on one for a date far ahead: " +
			"the feed is republished weekly and a stale copy plans against services that have changed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, statusOut, error) {
		out := statusOut{Realtime: d.Live.Enabled(), Building: d.Rebuilding()}
		if p := d.progress.Load(); p != nil && *p != "" {
			out.Progress = *p
		}
		if d.Store == nil {
			out.Note = "this server was started without a managed database"
			return nil, out, nil
		}
		st, err := d.Store.Status(ctx)
		if err != nil {
			return nil, statusOut{}, err
		}
		out.Present, out.Stale, out.SizeMB = st.Exists, st.Stale, int(st.Bytes/(1<<20))
		if !st.BuiltAt.IsZero() {
			out.BuiltAt = st.BuiltAt.In(d.Loc).Format("2 Jan 2006")
			out.AgeDays = int(time.Since(st.BuiltAt).Hours() / 24)
		}
		if ix, err := d.idx(); err == nil {
			if _, to, err := ix.Validity(ctx); err == nil && !to.IsZero() {
				out.TimetableTo = to.Format("2 Jan 2006")
				out.Expired = time.Now().After(to)
			}
		}
		switch {
		case !out.Present:
			out.Note = "no database yet; rebuild_database will build one"
		case out.Expired:
			out.Note = "the timetable period has passed and queries will fail; rebuild"
		case out.Stale:
			out.Note = "older than a week. Mention it and offer to rebuild rather than rebuilding unasked: it downloads about 250 MB"
		default:
			out.Note = "current"
		}
		return nil, out, nil
	})
}

type rebuildOut struct {
	Started bool   `json:"started"`
	Note    string `json:"note"`
}

func registerRebuildDatabase(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "rebuild_database",
		Description: "Rebuild the local timetable database from the published feed. " +
			"Ask the user before calling this: it downloads about 250 MB and takes minutes. " +
			"It returns immediately and builds in the background — the existing database keeps " +
			"answering meanwhile, and database_status reports progress. " +
			"Use it when database_status says stale or expired, not routinely.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, rebuildOut, error) {
		if d.Store == nil {
			return nil, rebuildOut{}, errors.New("this server was started without a managed database")
		}
		if d.Rebuilding() {
			return nil, rebuildOut{Note: "a rebuild is already running; database_status reports progress"}, nil
		}
		if !d.Rebuild(ctx) {
			return nil, rebuildOut{Note: "a rebuild is already running"}, nil
		}
		return nil, rebuildOut{
			Started: true,
			Note:    "building in the background, a few minutes. The current database keeps answering until it is ready; database_status reports progress.",
		}, nil
	})
}
