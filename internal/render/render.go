// Package render turns library results into the shapes clients receive.
//
// It exists because there are two clients — an assistant over stdio and a
// browser over HTTP — and they had grown separate copies of the same
// conversion. The copies drifted: walking legs, platform numbers and route
// numbers reached the browser and never reached the assistant, so the MCP
// server would plan a walk between a bus stop and a station and not mention the
// walk. One conversion, two thin adapters, and that cannot recur.
//
// Wording lives here rather than in the library. The library reports machine
// names; how they read to a person is a presentation decision.
package render

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	gtfs "github.com/kensantoso/ptv-gtfs-go"
	"github.com/kensantoso/ptv-gtfs-go/realtime"
)

// Deps are what rendering needs beyond the values themselves: the index, for
// detail the library does not carry on a journey, and the network's timezone.
type Deps struct {
	Index *gtfs.Index
	Loc   *time.Location
}

/* ---------- wire types ---------- */

// Stop is a place a service calls.
type Stop struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	Mode string  `json:"mode"`
	Lat  float64 `json:"lat,omitempty"`
	Lon  float64 `json:"lon,omitempty"`
}

// Departure is one service leaving a stop.
type Departure struct {
	Departs  string `json:"departs"`
	Route    string `json:"route"`
	Towards  string `json:"towards"`
	Mode     string `json:"mode"`
	Platform string `json:"platform,omitempty"`
	// Replacement marks a bus running in place of the train it replaces. These
	// are published on the train mode, so without it a passenger is sent to a
	// platform for a service leaving from the forecourt.
	Replacement bool `json:"replacement,omitempty"`
	// Status is one of: on time, late, early, cancelled, not stopping, or
	// "no live data" when realtime does not cover this service yet. It is never
	// "on time" merely because nothing is known.
	Status string `json:"status"`
	// Expected is the revised time, present only when realtime determined
	// something.
	Expected string `json:"expected,omitempty"`
	// DelayMinutes is signed; negative is early.
	DelayMinutes *int `json:"delay_minutes,omitempty"`
}

// Leg is one ride, or one walk, within a journey.
type Leg struct {
	Mode  string `json:"mode"`
	Route string `json:"route"`
	// RouteShort is what is written on the vehicle: "86" for a tram, "Alamein"
	// for a train.
	RouteShort     string `json:"route_short,omitempty"`
	Replacement    bool   `json:"replacement,omitempty"`
	Towards        string `json:"towards,omitempty"`
	From           string `json:"from"`
	Depart         string `json:"depart"`
	To             string `json:"to"`
	Arrive         string `json:"arrive"`
	Stops          int    `json:"intermediate_stops"`
	FromPlatform   string `json:"from_platform,omitempty"`
	ToPlatform     string `json:"to_platform,omitempty"`
	Status         string `json:"status"`
	ExpectedDepart string `json:"expected_depart,omitempty"`
	ExpectedArrive string `json:"expected_arrive,omitempty"`
	// Walk marks a leg on foot between two stops the feed does not connect.
	// The distance is straight-line, so the real walk is longer.
	Walk       bool `json:"walk,omitempty"`
	WalkMetres int  `json:"walk_metres,omitempty"`
	// TripID, FromSeq and ToSeq identify this leg for a detail lookup.
	TripID  string `json:"trip_id,omitempty"`
	FromSeq int    `json:"from_seq,omitempty"`
	ToSeq   int    `json:"to_seq,omitempty"`
}

// Journey is a complete trip from one place to another.
type Journey struct {
	Depart  string `json:"depart"`
	Arrive  string `json:"arrive"`
	Minutes int    `json:"minutes"`
	Changes int    `json:"changes"`
	// WaitMinutes is what is left at each change after any walking.
	WaitMinutes []int `json:"wait_at_change_minutes,omitempty"`
	// WaitToDepart is how long from the requested time until this leaves. A
	// short journey departing in ninety minutes is rarely the better trade, and
	// without this the ranking looks arbitrary.
	WaitToDepart int `json:"wait_before_departure_minutes"`
	// CityLoopStops is how many of Flagstaff, Melbourne Central and Parliament
	// this journey sits through without starting or ending there.
	CityLoopStops  int    `json:"city_loop_stops_passed"`
	Status         string `json:"status"`
	ExpectedArrive string `json:"expected_arrive,omitempty"`
	DelayMinutes   *int   `json:"delay_minutes,omitempty"`
	// Warning flags a journey realtime has broken, most often a connection that
	// no longer exists because the first leg is running late.
	Warning string `json:"warning,omitempty"`
	Legs    []Leg  `json:"legs"`
}

// Alert is a disruption.
type Alert struct {
	Headline string `json:"headline"`
	// Detail carries the substance. The feed's own header is frequently a bare
	// category such as "Minor Delay", so a caller reading only the headline
	// learns nothing about what happened.
	Detail string   `json:"detail,omitempty"`
	Lines  []string `json:"lines,omitempty"`
	From   string   `json:"from,omitempty"`
	Until  string   `json:"until,omitempty"`
	URL    string   `json:"url,omitempty"`
}

/* ---------- wording ---------- */

// StatusText turns a library status into something a person reads.
//
// "no live data" is deliberately not "unknown": it says which fact is missing,
// and it must never read as reassurance.
func StatusText(s gtfs.LiveStatus) string {
	switch s {
	case gtfs.StatusOnTime:
		return "on time"
	case gtfs.StatusEarly:
		return "early"
	case gtfs.StatusLate:
		return "late"
	case gtfs.StatusCanceled:
		return "cancelled"
	case gtfs.StatusSkipped:
		return "not stopping"
	}
	return "no live data"
}

// LiveNote describes the freshness of a snapshot, or its absence.
func LiveNote(l *gtfs.Live, when time.Time, horizon time.Duration) string {
	if l == nil {
		return "Scheduled times only: no realtime key is configured, so delays and cancellations are not known."
	}
	// The feeds describe services running now. Measuring their age against a
	// query about tomorrow gives a sentence like "18h45m old", which is not
	// wrong so much as meaningless.
	if ahead := time.Until(when); ahead > horizon {
		return fmt.Sprintf("Scheduled times: realtime reaches about an hour ahead, and this is %s away.",
			ahead.Round(time.Minute))
	}
	age := time.Since(l.At).Round(time.Second)
	if age < 0 {
		age = 0
	}
	return fmt.Sprintf("Live data as at %s (%s old). Anything marked \"no live data\" is not known, not on time.",
		l.At.In(when.Location()).Format("15:04:05"), age)
}

/* ---------- conversions ---------- */

// Stops renders a stop search result.
func Stops(in []gtfs.Stop) []Stop {
	out := make([]Stop, 0, len(in))
	for _, s := range in {
		out = append(out, Stop{ID: s.ID, Name: s.Name, Mode: s.Mode.String(), Lat: s.Lat, Lon: s.Lon})
	}
	return out
}

// Departures applies realtime and renders. A nil snapshot is schedule-only.
func Departures(l *gtfs.Live, in []gtfs.Departure) []Departure {
	live := l.Disambiguate(l.Departures(in))
	out := make([]Departure, 0, len(live))
	for _, x := range live {
		d := Departure{
			Departs: x.Depart.Format("15:04"), Route: x.RouteName, Towards: x.Headsign,
			Mode: x.Mode.String(), Platform: x.Platform, Status: StatusText(x.Status),
			Replacement: x.Replacement,
		}
		if x.Status.Known() {
			d.Expected = x.Estimated.Format("15:04")
			m := int(x.Delay.Round(time.Minute).Minutes())
			d.DelayMinutes = &m
		}
		out = append(out, d)
	}
	return out
}

// Journeys renders planned journeys with realtime applied, dropping any that
// realtime has already broken.
func (d *Deps) Journeys(ctx context.Context, in []gtfs.Journey, l *gtfs.Live, when time.Time) []Journey {
	out := make([]Journey, 0, len(in))
	for _, j := range in {
		out = append(out, d.Journey(ctx, j, l, when))
	}
	return DropBroken(out)
}

// DropBroken removes journeys whose connection realtime has already eaten.
//
// A journey that cannot be made is not an option, and offering it alongside
// real ones invites picking it. The exception is when every option is broken:
// an empty list reads as "nothing runs", which is a different and wrong answer,
// and someone already aboard the first leg needs to be told their connection
// has gone rather than watch it vanish from the list.
func DropBroken(in []Journey) []Journey {
	out := make([]Journey, 0, len(in))
	for _, j := range in {
		if j.Warning == "" {
			out = append(out, j)
		}
	}
	if len(out) == 0 {
		return in
	}
	return out
}

// Journey renders one planned journey.
func (d *Deps) Journey(ctx context.Context, j gtfs.Journey, l *gtfs.Live, when time.Time) Journey {
	lj := l.Journey(j)
	out := Journey{
		Depart: j.Depart.Format("15:04"), Arrive: j.Arrive.Format("15:04"),
		Minutes: int(j.Duration().Minutes()), Changes: j.Transfers,
		WaitToDepart: int(j.Depart.Sub(when).Minutes()), Status: StatusText(lj.Status),
	}
	if d.Index != nil {
		if n, err := d.Index.CityLoopStops(ctx, j); err == nil {
			out.CityLoopStops = n
		}
	}
	if lj.Status.Known() {
		out.ExpectedArrive = lj.EstimatedArrive.Format("15:04")
		m := int(lj.Delay.Round(time.Minute).Minutes())
		out.DelayMinutes = &m
	}
	if lj.BrokenTransfer {
		out.Warning = "This connection no longer works: with current delays there is not enough time to make the change."
	}
	for i, w := range j.WaitAtTransfer {
		v := int(w.Minutes())
		if i < len(lj.TightTransfers) && lj.Status.Known() {
			v = int(lj.TightTransfers[i].Minutes())
		}
		out.WaitMinutes = append(out.WaitMinutes, v)
	}
	for i, leg := range j.Legs {
		lo := Leg{
			Mode: leg.Mode.String(), Route: leg.RouteName, RouteShort: leg.RouteShort,
			Replacement: leg.Replacement,
			Towards:     leg.Headsign, From: leg.FromName, Depart: leg.Depart.Format("15:04"),
			To: leg.ToName, Arrive: leg.Arrive.Format("15:04"), Stops: leg.StopsCount,
			FromPlatform: leg.FromPlatform, ToPlatform: leg.ToPlatform,
			TripID: leg.TripID, FromSeq: leg.FromSeq, ToSeq: leg.ToSeq,
			Walk: leg.Walk, WalkMetres: int(leg.WalkMetres),
			Status: StatusText(gtfs.StatusUnknown),
		}
		if i < len(lj.LiveLegs) {
			ll := lj.LiveLegs[i]
			lo.Status = StatusText(ll.Status)
			if ll.Status.Known() {
				lo.ExpectedDepart = ll.EstimatedDepart.Format("15:04")
				lo.ExpectedArrive = ll.EstimatedArrive.Format("15:04")
			}
		}
		out.Legs = append(out.Legs, lo)
	}
	return out
}

// Alerts renders disruptions, live incidents before planned works.
func Alerts(in []realtime.Alert, routes map[string]string, loc *time.Location, limit int) []Alert {
	out := make([]Alert, 0, len(in))
	flags := make([]bool, 0, len(in))
	for _, a := range in {
		out = append(out, alert(a, LineNames(routes, a.Routes), loc))
		flags = append(flags, Planned(a))
	}
	out = incidentsFirst(out, flags)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func alert(a realtime.Alert, lines []string, loc *time.Location) Alert {
	o := Alert{Headline: a.Header, Detail: a.Description, Lines: lines, URL: a.URL}
	if a.Header == "" {
		o.Headline, o.Detail = a.Description, ""
	}
	if !a.Start.IsZero() {
		o.From = a.Start.In(loc).Format("Mon 2 Jan 15:04")
	}
	if !a.End.IsZero() {
		o.Until = a.End.In(loc).Format("Mon 2 Jan 15:04")
	}
	return o
}

// Planned reports whether an alert is scheduled work rather than something
// happening now. Both matter, but a passenger deciding how to travel in the
// next ten minutes needs the incident first.
func Planned(a realtime.Alert) bool {
	if a.Cause == "CONSTRUCTION" || a.Cause == "MAINTENANCE" {
		return true
	}
	t := strings.ToLower(a.Header + " " + a.Description)
	return strings.Contains(t, "planned work") || strings.Contains(t, "plannedoccupation")
}

func incidentsFirst(in []Alert, planned []bool) []Alert {
	out := make([]Alert, 0, len(in))
	for _, want := range []bool{false, true} {
		for i, a := range in {
			if planned[i] == want {
				out = append(out, a)
			}
		}
	}
	return out
}

// LineNames turns the route ids realtime uses into the words a passenger uses.
func LineNames(routes map[string]string, ids []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		n, ok := routes[id]
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

/* ---------- request parsing ---------- */

// ParseModes accepts the words a person uses for a mode.
// ParseModeList parses a comma-separated list, for a caller narrowing what gets
// loaded rather than what gets queried. It also returns the names it did not
// recognise.
//
// Those must not be ignored. An empty list means "everything", so a typo like
// "trian" would otherwise load every mode — a 1.5 GB database and a long
// download instead of the 97 MB asked for. Worse is the partial case: "train,tam"
// silently drops the tram and the caller never learns why their tram queries
// return nothing.
func ParseModeList(s string) ([]gtfs.Mode, []string) {
	seen := map[gtfs.Mode]bool{}
	var out []gtfs.Mode
	var unknown []string
	for _, part := range strings.Split(s, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		ms := ParseModes(part)
		if len(ms) == 0 {
			unknown = append(unknown, strings.TrimSpace(part))
			continue
		}
		for _, m := range ms {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out, unknown
}

func ParseModes(s string) []gtfs.Mode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "train", "trains", "metro train":
		return []gtfs.Mode{gtfs.ModeMetroTrain, gtfs.ModeRegionalTrain}
	case "tram", "trams":
		return []gtfs.Mode{gtfs.ModeMetroTram}
	case "bus", "buses":
		return []gtfs.Mode{gtfs.ModeMetroBus, gtfs.ModeRegionalBus, gtfs.ModeRegionalCoach}
	}
	return nil
}

// ParseRank accepts several spellings because a model will produce any of them.
func ParseRank(s string) gtfs.Rank {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "fewest_transfers", "fewest_changes", "fewest":
		return gtfs.RankFewestTransfers
	case "leave_latest", "latest":
		return gtfs.RankLeaveLatest
	case "shortest", "shortest_journey", "least_travelling", "minimum_time":
		return gtfs.RankShortest
	}
	return gtfs.RankFastest
}

// ParseWhen accepts a few shapes because a model will produce any of them. An
// empty string means now.
func ParseWhen(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Now().In(loc), nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02", "15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			if layout == "15:04" {
				now := time.Now().In(loc)
				return time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc), nil
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("could not parse time %q; use YYYY-MM-DD HH:MM", s)
}

// AtoiOr reads an integer, falling back to a default.
func AtoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n
	}
	return def
}
